//go:build integration

// Package integration runs composesafe against a real Docker daemon.
// Run with: go test -tags integration ./test/integration -count=1
// It needs Docker, Compose >= 2.24 and the images below (pulled if missing).
package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/snapshot"
	"composesafe/internal/workflow"
)

const pgImage = "postgres:16-alpine"

func helperImage() string {
	if v := os.Getenv("CS_TEST_HELPER"); v != "" {
		return v
	}
	return "alpine:3"
}

// stack is a throwaway Compose project: db (Postgres), app (depends on db,
// owns a volume) and a database with two schemas' worth of tables.
type stack struct {
	t       *testing.T
	project string
	dir     string
	file    string
	cfg     *config.Config
}

func sh(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newStack(t *testing.T) *stack {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	var b [3]byte
	_, _ = rand.Read(b[:])
	s := &stack{t: t, project: "cstest" + hex.EncodeToString(b[:]), dir: t.TempDir()}
	s.file = filepath.Join(s.dir, "compose.yml")
	compose := fmt.Sprintf(`services:
  db:
    image: %s
    environment:
      POSTGRES_PASSWORD: s3cretpw
      POSTGRES_USER: appuser
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U appuser"]
      interval: 2s
      retries: 30
  app:
    image: %s
    command: ["sleep", "100000"]
    depends_on:
      db: {condition: service_healthy}
    volumes:
      - uploads:/srv/uploads
volumes:
  pgdata:
  uploads:
`, pgImage, helperImage())
	if err := os.WriteFile(s.file, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, ".env"), []byte("SECRET_TOKEN=tok-9f8e7d6c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Test-only teardown of the project we created.
		_ = exec.Command("docker", "compose", "-p", s.project, "-f", s.file, "down", "--volumes", "--remove-orphans", "--timeout", "2").Run()
		out, _ := exec.Command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "composesafe-keep/*").Output()
		for _, tag := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "rmi", tag).Run()
		}
	})

	sh(t, s.dir, "docker", "compose", "-p", s.project, "-f", s.file, "up", "-d", "--wait")
	s.waitForTCP()
	s.psql("postgres", "create table t(id int, v text)")
	s.psql("postgres", "insert into t select g, md5(g::text) from generate_series(1,5000) g")
	s.psql("postgres", "create database other")
	s.psql("other", "create table o(x int)")
	s.psql("other", "create table o2(y int)")
	sh(t, s.dir, "docker", "compose", "-p", s.project, "-f", s.file, "exec", "-T", "app", "sh", "-c",
		"echo user-upload > /srv/uploads/a.txt; head -c 300000 /dev/urandom > /srv/uploads/blob.bin")

	cfg := config.Default()
	cfg.StateDir = filepath.Join(s.dir, "state")
	cfg.HelperImage = helperImage()
	s.cfg = cfg
	return s
}

// waitForTCP waits until the real server accepts connections. The image's
// entrypoint first runs a temporary socket-only server for initialisation
// and restarts; a socket-based healthcheck can pass during that phase.
func (s *stack) waitForTCP() {
	s.t.Helper()
	for i := 0; i < 60; i++ {
		cmd := exec.Command("docker", "compose", "-p", s.project, "-f", s.file, "exec", "-T", "db", "pg_isready", "-q", "-h", "127.0.0.1", "-U", "appuser")
		if cmd.Run() == nil {
			return
		}
		time.Sleep(time.Second)
	}
	s.t.Fatal("postgres did not become ready")
}

func (s *stack) psql(db, sql string) string {
	return sh(s.t, s.dir, "docker", "compose", "-p", s.project, "-f", s.file, "exec", "-T", "db",
		"psql", "-U", "appuser", "-d", db, "-tAc", sql)
}

// started returns the container start timestamps, to tell "was restarted"
// from "was left alone".
func (s *stack) started(service string) string {
	id := sh(s.t, s.dir, "docker", "compose", "-p", s.project, "-f", s.file, "ps", "-q", service)
	return sh(s.t, s.dir, "docker", "inspect", "--format", "{{.State.StartedAt}}", id)
}

func (s *stack) running(service string) bool {
	return sh(s.t, s.dir, "docker", "compose", "-p", s.project, "-f", s.file, "ps", "-q", "--status", "running", service) != ""
}

func (s *stack) deps(r docker.Runner) workflow.SnapshotDeps {
	return workflow.SnapshotDeps{Runner: r, Now: time.Now, UID: os.Getuid(), GID: os.Getgid(), Version: "test",
		Progress: func(key string, args ...any) { s.t.Logf("%s %v", key, args) }}
}

func (s *stack) opts(services ...string) workflow.SnapshotOptions {
	return workflow.SnapshotOptions{
		ScanOptions: workflow.ScanOptions{Files: []string{s.file}, Project: s.project},
		Services:    services, Execute: true, Yes: true,
	}
}

func gunzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := b.ReadFrom(gz); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestSnapshotColdArchiveAndRestoreCheck(t *testing.T) {
	s := newStack(t)
	dbStart, appStart := s.started("db"), s.started("app")

	res, err := workflow.Snapshot(t.Context(), s.deps(docker.ExecRunner{}), s.cfg, s.opts("app"))
	if err != nil {
		t.Fatalf("snapshot: %v (problems %v)", err, res.Problems)
	}
	m := res.Manifest
	if !m.Complete || !m.Verified.Checksums || !m.Verified.DumpTrailer || !m.Verified.RestoreCheck {
		t.Fatalf("not fully verified: %+v", m.Verified)
	}

	// Everything we stopped is running again (restarted, not left alone).
	if !s.running("db") || !s.running("app") {
		t.Fatal("services must be running after the snapshot")
	}
	if s.started("db") == dbStart || s.started("app") == appStart {
		t.Error("db and app were stopped for the cold archive, so they must have been started again")
	}
	if got := s.psql("postgres", "select count(*) from t"); got != "5000" {
		t.Errorf("data after restart: %s rows", got)
	}

	// Artifacts: dump holds the data, archives hold both volumes, files are private and owned by us.
	dump := gunzip(t, filepath.Join(res.Dir, "db.sql.gz"))
	for _, want := range []string{"CREATE DATABASE other", "COPY public.t ", "PostgreSQL database cluster dump complete"} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q", want)
		}
	}
	for _, a := range m.Artifacts {
		fi, err := os.Stat(filepath.Join(res.Dir, a.Path))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", a.Path, fi.Mode().Perm())
		}
		if uid := fileUID(fi); uid != os.Getuid() {
			t.Errorf("%s is owned by uid %d, want %d (the archive helper runs as root)", a.Path, uid, os.Getuid())
		}
	}
	if len(m.Find(snapshot.KindVolume, "")) != 2 {
		t.Errorf("expected archives of both volumes: %+v", m.Artifacts)
	}
	db := m.Services[1]
	if db.Name != "db" || db.PGUser != "appuser" || !strings.HasPrefix(db.PGVersion, "16.") || db.PGTables["postgres"] != 1 || db.PGTables["other"] != 2 {
		t.Errorf("db metadata: %+v", db)
	}

	// The old image is pinned.
	for _, svc := range m.Services {
		if svc.Image.KeepTag == "" {
			t.Errorf("%s: no keep tag", svc.Name)
			continue
		}
		sh(t, "", "docker", "image", "inspect", svc.Image.KeepTag)
	}

	// Secrets: the .env value is stored (it is the backup) but never in the run log.
	if b, _ := os.ReadFile(filepath.Join(res.Dir, "run.jsonl")); strings.Contains(string(b), "tok-9f8e7d6c") || strings.Contains(string(b), "s3cretpw") {
		t.Error("secret leaked into run.jsonl")
	}

	// Throwaway restore-check containers are gone.
	if out := sh(t, "", "docker", "ps", "-a", "--filter", "label=composesafe.check", "--format", "{{.Names}}"); out != "" {
		t.Errorf("leftover check container: %s", out)
	}

	// A modified archive is caught.
	dumpPath := filepath.Join(res.Dir, "db.sql.gz")
	orig, _ := os.ReadFile(dumpPath)
	bad := append([]byte(nil), orig...)
	bad[len(bad)/2] ^= 0xff
	os.WriteFile(dumpPath, bad, 0o600)
	if v := snapshot.Verify(res.Dir, m); v.OK() || v.Checksums {
		t.Error("a modified dump must fail verification")
	}
	os.WriteFile(dumpPath, orig, 0o600)
	if v := snapshot.Verify(res.Dir, m); !v.OK() {
		t.Errorf("restored dump should verify again: %v", v.Problems)
	}
}

func TestLogicalOnlyStopsNothing(t *testing.T) {
	s := newStack(t)
	dbStart, appStart := s.started("db"), s.started("app")
	o := s.opts("app")
	o.LogicalOnly = true
	res, err := workflow.Snapshot(t.Context(), s.deps(docker.ExecRunner{}), s.cfg, o)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if s.started("db") != dbStart || s.started("app") != appStart {
		t.Error("a logical dump must not stop or restart anything")
	}
	if len(res.Manifest.Find(snapshot.KindVolume, "")) != 0 || len(res.Manifest.Quiesced) != 0 {
		t.Errorf("no archives and no quiescing expected: %+v", res.Manifest)
	}
	if !res.Manifest.Verified.RestoreCheck {
		t.Error("restore check must still run")
	}
}

// failingRunner makes one command fail, to prove that services we stopped
// are started again when a snapshot dies halfway.
type failingRunner struct {
	docker.Runner
	when string
}

func (f failingRunner) Run(ctx context.Context, args []string, o docker.Opts) (docker.Result, error) {
	if strings.Contains(strings.Join(args, " "), f.when) {
		return docker.Result{}, errors.New("injected failure")
	}
	return f.Runner.Run(ctx, args, o)
}

func TestFailureHalfwayStartsServicesAgain(t *testing.T) {
	s := newStack(t)
	r := failingRunner{Runner: docker.ExecRunner{}, when: "BACKUP_NAME=" + s.project + "_pgdata.tgz"}
	res, err := workflow.Snapshot(t.Context(), s.deps(r), s.cfg, s.opts("app"))
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	if res.Manifest.Complete {
		t.Error("a failed snapshot must not be marked complete")
	}
	if !s.running("db") || !s.running("app") {
		t.Fatal("services stopped for the archive must be running again after a failure")
	}
	if got := s.psql("postgres", "select count(*) from t"); got != "5000" {
		t.Errorf("data intact after the failure: %s rows", got)
	}
	if reloaded, err := snapshot.Load(res.Dir); err != nil || reloaded.Complete {
		t.Errorf("manifest on disk must exist and say incomplete: %v %+v", err, reloaded)
	}
}

func TestDatabaseNotRunningIsReportedBeforeAnythingStops(t *testing.T) {
	s := newStack(t)
	sh(t, s.dir, "docker", "compose", "-p", s.project, "-f", s.file, "stop", "app", "db")
	_, err := workflow.Snapshot(t.Context(), s.deps(docker.ExecRunner{}), s.cfg, s.opts("db"))
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v", err)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	s := newStack(t)
	dbStart := s.started("db")
	o := s.opts("app")
	o.Execute = false
	res, err := workflow.Snapshot(t.Context(), s.deps(docker.ExecRunner{}), s.cfg, o)
	if err != nil || res.Executed || res.Plan == nil {
		t.Fatalf("%v %+v", err, res)
	}
	if s.started("db") != dbStart {
		t.Error("dry run touched the database container")
	}
	if _, err := os.Stat(s.cfg.StateDir); err == nil {
		t.Error("dry run must not create the state directory")
	}
}
