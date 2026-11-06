// Package state manages the on-disk state directory (DESIGN section 7):
// per-project run directories and the per-project lock.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ProjectDir is <stateDir>/<project>, with the project name made filename-safe.
func ProjectDir(stateDir, project string) string {
	name := strings.Trim(unsafeRe.ReplaceAllString(project, "_"), "._")
	if name == "" {
		name = "default"
	}
	return filepath.Join(stateDir, name)
}

// NewRunID returns YYYYMMDD-HHMMSS-<4 hex>, in local time.
func NewRunID(now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// EnsureDir creates dir (and parents) with mode 0700.
func EnsureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

// RunDir creates and returns <projectDir>/<runID>.
func RunDir(projectDir, runID string) (string, error) {
	d := filepath.Join(projectDir, runID)
	if err := EnsureDir(d); err != nil {
		return "", err
	}
	return d, nil
}

var (
	// ErrLocked: another composesafe process holds the project lock.
	ErrLocked = errors.New("project is locked by another composesafe process")
	// ErrStaleLock: the lock file exists but its process is gone.
	ErrStaleLock = errors.New("stale lock file left by a process that no longer exists (use --force-unlock)")
)

// Lock takes the project lock so that only one state-changing command runs
// at a time. With force, a stale lock (dead pid) is removed and re-acquired;
// a lock held by a live process is never broken.
func Lock(projectDir string, force bool) (unlock func(), err error) {
	if err := EnsureDir(projectDir); err != nil {
		return nil, err
	}
	path := filepath.Join(projectDir, "lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		pid := holder(path)
		if pid > 0 && alive(pid) {
			return nil, fmt.Errorf("%w (pid %d)", ErrLocked, pid)
		}
		if !force {
			return nil, ErrStaleLock
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, ErrLocked
}

func holder(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
