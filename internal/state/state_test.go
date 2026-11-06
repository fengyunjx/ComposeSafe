package state

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

func TestProjectDirSanitizes(t *testing.T) {
	tests := map[string]string{
		"myapp":       "/s/myapp",
		"my app/../x": "/s/my_app_.._x",
		"..":          "/s/default",
		"":            "/s/default",
	}
	for in, want := range tests {
		if got := ProjectDir("/s", in); got != want {
			t.Errorf("ProjectDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewRunID(t *testing.T) {
	id := NewRunID(time.Date(2026, 11, 6, 9, 5, 3, 0, time.Local))
	if !regexp.MustCompile(`^20261106-090503-[0-9a-f]{4}$`).MatchString(id) {
		t.Errorf("bad run id %q", id)
	}
	if NewRunID(time.Now()) == NewRunID(time.Now()) {
		t.Log("two ids in the same second collided (1/65536 chance); rerun if flaky")
	}
}

func TestRunDirPermissions(t *testing.T) {
	d, err := RunDir(filepath.Join(t.TempDir(), "proj"), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestLockExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir, false); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v, want ErrLocked", err)
	}
	// Even --force-unlock must not break a lock held by a live process.
	if _, err := Lock(dir, true); !errors.Is(err, ErrLocked) {
		t.Fatalf("force on live lock: %v, want ErrLocked", err)
	}
	unlock()
	if u, err := Lock(dir, false); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	} else {
		u()
	}
}

func TestStaleLock(t *testing.T) {
	dir := t.TempDir()
	// pid 2^22+1 is above the default pid_max on Linux and macOS.
	os.WriteFile(filepath.Join(dir, "lock"), []byte(strconv.Itoa(4194305)+"\n"), 0o600)
	if _, err := Lock(dir, false); !errors.Is(err, ErrStaleLock) {
		t.Fatalf("got %v, want ErrStaleLock", err)
	}
	unlock, err := Lock(dir, true)
	if err != nil {
		t.Fatalf("force should replace a stale lock: %v", err)
	}
	unlock()
	if _, err := os.Stat(filepath.Join(dir, "lock")); !os.IsNotExist(err) {
		t.Error("unlock must remove the lock file")
	}
}
