package workflow

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/docker/dockertest"
	"composesafe/internal/pg"
	"composesafe/internal/snapshot"
)

// sim is a tiny pretend Docker daemon: it tracks which services are running,
// answers the commands snapshot uses, and writes the files the helper
// container would write. It lets the tests assert on ORDER, which is what
// keeps data safe (writers stop before the dump, the database stops before
// the archive, everything starts again afterwards).
type sim struct {
	t       *testing.T
	mu      sync.Mutex
	running map[string]bool
	log     []string // semantic events in order
	failOn  string   // substring of a command line that must fail
	file    string
	fake    *dockertest.Fake
}

const simCompose = `{"name":"p","services":{
 "db":{"image":"postgres:16","volumes":[{"type":"volume","source":"pgdata","target":"/var/lib/postgresql/data"}]},
 "app":{"image":"x/app:1","depends_on":{"db":{"condition":"service_started"}}}},
 "volumes":{"pgdata":{"name":"p_pgdata"}}}`

func newSim(t *testing.T) *sim {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	dir := t.TempDir()
	s := &sim{t: t, running: map[string]bool{"db": true, "app": true}, file: filepath.Join(dir, "compose.yml")}
	os.WriteFile(s.file, []byte("services: {}\n"), 0o600)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN=abcdef123\n"), 0o600)
	s.fake = &dockertest.Fake{Handler: s.handle}
	return s
}

func (s *sim) note(format string, a ...any) {
	s.mu.Lock()
	s.log = append(s.log, fmt.Sprintf(format, a...))
	s.mu.Unlock()
}

func (s *sim) index(event string) int {
	for i, e := range s.log {
		if e == event {
			return i
		}
	}
	return -1
}

func ok(out string) (dockertest.Response, bool) { return dockertest.Response{Stdout: out}, true }

func (s *sim) handle(args []string, o docker.Opts) (dockertest.Response, bool) {
	line := strings.Join(args, " ")
	if s.failOn != "" && strings.Contains(line, s.failOn) {
		return dockertest.Response{Err: fmt.Errorf("injected failure"), Stderr: "boom"}, true
	}
	switch {
	case line == "context inspect":
		return ok(`[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`)
	case line == "version --format {{.Server.Version}}":
		return ok("29.0.0")
	case line == "compose version --short":
		return ok("5.0.0")
	case line == "system df -v --format json":
		return ok(`{"Volumes":[{"Name":"p_pgdata","Size":"1MB"}]}`)
	case strings.HasPrefix(line, "image inspect --format {{json .}}"):
		ref := args[len(args)-1]
		return ok(`{"Id":"sha256:` + strings.NewReplacer("/", "_", ":", "_").Replace(ref) + `","RepoDigests":["d@sha256:1"],"Config":{}}`)
	case args[0] == "compose":
		return s.compose(args)
	case args[0] == "inspect":
		return ok("sha256:img-of-" + args[len(args)-1])
	case args[0] == "tag":
		s.note("tag %s", args[2])
		return ok("")
	case args[0] == "run" && containsArg(args, "df"):
		return ok("Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 100000000 1000 99000000 1% /\n")
	case args[0] == "run" && containsArg(args, "-d"):
		s.note("check-server")
		return ok("cid")
	case args[0] == "run":
		return s.archive(args)
	case args[0] == "exec":
		return s.execInCheck(args, o)
	case args[0] == "stop":
		s.note("check-stop")
		return ok("")
	}
	return dockertest.Response{}, false
}

func containsArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

func (s *sim) compose(args []string) (dockertest.Response, bool) {
	// docker compose -p p -f FILE <verb> ...
	rest := args[5:]
	switch rest[0] {
	case "config":
		return ok(simCompose)
	case "ps":
		svc := rest[len(rest)-1]
		status := containsArg(rest, "--status")
		if status && !s.running[svc] {
			return ok("")
		}
		return ok("cid-" + svc)
	case "stop":
		for _, n := range rest[1:] {
			s.running[n] = false
		}
		s.note("stop %s", strings.Join(rest[1:], ","))
		return ok("")
	case "start":
		for _, n := range rest[1:] {
			s.running[n] = true
		}
		s.note("start %s", strings.Join(rest[1:], ","))
		return ok("")
	case "exec":
		script := rest[len(rest)-1]
		switch {
		case strings.Contains(script, "POSTGRES_INITDB_ARGS"):
			return ok("")
		case strings.Contains(script, "POSTGRES_USER"):
			return ok("appuser")
		case strings.Contains(script, "server_version"):
			return ok("16.4")
		case strings.Contains(script, "pg_dumpall"):
			s.note("dump")
			return ok(strings.Repeat("-- line\n", 400) + pg.Trailer + "\n")
		case strings.Contains(script, "pg_database"):
			return ok("postgres 1\nother 2\n")
		case strings.Contains(script, "pg_isready"):
			s.note("ready")
			return ok("")
		}
	}
	return dockertest.Response{}, false
}

// archive plays the helper container: it writes a valid tgz into the backup dir.
func (s *sim) archive(args []string) (dockertest.Response, bool) {
	var dir, name string
	if containsArg(args, "-v") && strings.Contains(strings.Join(args, " "), ":/backup:ro") {
		return ok("") // unpacking an archive into a restored volume: nothing to write
	}
	for i, a := range args {
		if a == "-v" && strings.HasSuffix(args[i+1], ":/backup") {
			dir = strings.TrimSuffix(args[i+1], ":/backup")
		}
		if strings.HasPrefix(a, "BACKUP_NAME=") {
			name = strings.TrimPrefix(a, "BACKUP_NAME=")
		}
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	data := make([]byte, 4096)
	rand.Read(data)
	tw.WriteHeader(&tar.Header{Name: "./PG_VERSION", Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
	tw.Write(data)
	tw.Close()
	var gzb bytes.Buffer
	w := gzip.NewWriter(&gzb)
	w.Write(raw.Bytes())
	w.Close()
	if err := os.WriteFile(filepath.Join(dir, name), gzb.Bytes(), 0o600); err != nil {
		s.t.Fatal(err)
	}
	s.note("archive %s (db running=%v)", name, s.running["db"])
	return ok("")
}

func (s *sim) execInCheck(args []string, o docker.Opts) (dockertest.Response, bool) {
	script := args[len(args)-1]
	switch {
	case strings.Contains(script, "pg_isready"):
		return ok("")
	case strings.Contains(script, "ON_ERROR_STOP"):
		if o.Stdin != nil {
			io.Copy(io.Discard, o.Stdin)
		}
		s.note("restore")
		return dockertest.Response{Stderr: `psql:<stdin>:3: ERROR:  role "appuser" already exists`}, true
	case strings.Contains(script, "pg_database"):
		return ok("postgres 1\nother 2\n")
	}
	return dockertest.Response{}, false
}

func (s *sim) deps() SnapshotDeps {
	return SnapshotDeps{Runner: s.fake, Now: func() time.Time { return time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC) },
		FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, Sleep: func(time.Duration) {},
		UID: 501, GID: 20, Version: "test"}
}

func (s *sim) opts(services ...string) SnapshotOptions {
	return SnapshotOptions{ScanOptions: ScanOptions{Files: []string{s.file}, Project: "p"}, Services: services, Execute: true, Yes: true}
}

func (s *sim) cfg() *config.Config {
	c := config.Default()
	c.StateDir = filepath.Join(filepath.Dir(s.file), "state")
	c.HelperImage = "alpine:test"
	return c
}

func TestSnapshotOrderOfOperations(t *testing.T) {
	s := newSim(t)
	res, err := Snapshot(t.Context(), s.deps(), s.cfg(), s.opts("app"))
	if err != nil {
		t.Fatalf("%v\nlog: %v", err, s.log)
	}
	if !res.Manifest.Complete || !res.Manifest.Verified.RestoreCheck {
		t.Fatalf("manifest: %+v", res.Manifest.Verified)
	}

	// The order that keeps data consistent and downtime short:
	want := []string{"stop app", "dump", "stop db", "archive p_pgdata.tgz (db running=false)", "start db", "ready", "start app", "check-server", "restore", "check-stop"}
	last := -1
	for _, w := range want {
		i := s.index(w)
		if i < 0 {
			t.Fatalf("event %q never happened; log: %v", w, s.log)
		}
		if i < last {
			t.Errorf("event %q happened out of order; log: %v", w, s.log)
		}
		last = i
	}
	if !s.running["db"] || !s.running["app"] {
		t.Error("everything must be running afterwards")
	}
	if s.index("tag composesafe-keep/db:"+res.Manifest.RunID) < 0 || s.index("tag composesafe-keep/app:"+res.Manifest.RunID) < 0 {
		t.Errorf("images not pinned: %v", s.log)
	}

	m, err := snapshot.Load(res.Dir)
	if err != nil || !m.Complete || strings.Join(m.Quiesced, ",") != "app" {
		t.Fatalf("reloaded manifest: %+v %v", m, err)
	}
	if db := m.Services[1]; db.PGUser != "appuser" || db.PGVersion != "16.4" || db.PGTables["other"] != 2 || db.Image.KeepTag == "" {
		t.Errorf("db: %+v", db)
	}
	// the .env copy is private and the run log has no secrets
	if fi, _ := os.Stat(filepath.Join(res.Dir, "config", ".env")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf(".env copy: %v", fi)
	}
	if b, _ := os.ReadFile(filepath.Join(res.Dir, "run.jsonl")); strings.Contains(string(b), "abcdef123") {
		t.Error("secret in run log")
	}
}

func TestSnapshotFailureStillStartsEverythingAgain(t *testing.T) {
	for _, tc := range []struct{ name, failOn string }{
		{"archive fails", "BACKUP_NAME=p_pgdata.tgz"},
		{"dump fails", "pg_dumpall"},
		{"pinning the image fails", "tag sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t)
			s.failOn = tc.failOn
			res, err := Snapshot(t.Context(), s.deps(), s.cfg(), s.opts("app"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !s.running["db"] || !s.running["app"] {
				t.Errorf("services left stopped after %q; log: %v", tc.failOn, s.log)
			}
			if res.Manifest.Complete {
				t.Error("failed snapshot must not be complete")
			}
			if m, lerr := snapshot.Load(res.Dir); lerr != nil || m.Complete {
				t.Errorf("manifest on disk: %v %+v", lerr, m)
			}
		})
	}
}

func TestSnapshotRestoreCheckFailureFailsTheSnapshot(t *testing.T) {
	s := newSim(t)
	s.failOn = "" // errors come from the table counts instead
	h := s.fake.Handler
	s.fake.Handler = func(args []string, o docker.Opts) (dockertest.Response, bool) {
		if args[0] == "exec" && strings.Contains(args[len(args)-1], "pg_database") {
			return ok("postgres 1\n") // the restored copy is missing database "other"
		}
		return h(args, o)
	}
	res, err := Snapshot(t.Context(), s.deps(), s.cfg(), s.opts("app"))
	if err == nil || !strings.Contains(err.Error(), `"other" is missing`) || res.Manifest.Complete {
		t.Fatalf("err = %v complete=%v", err, res.Manifest.Complete)
	}
	if s.index("check-stop") < 0 {
		t.Error("the throwaway server must be stopped even when the check fails")
	}
}

func TestSnapshotUnexpectedRestoreErrorsFail(t *testing.T) {
	s := newSim(t)
	h := s.fake.Handler
	s.fake.Handler = func(args []string, o docker.Opts) (dockertest.Response, bool) {
		if args[0] == "exec" && strings.Contains(args[len(args)-1], "ON_ERROR_STOP") {
			io.Copy(io.Discard, o.Stdin)
			return dockertest.Response{Stderr: `psql:<stdin>:9: ERROR:  syntax error at or near "x"`}, true
		}
		return h(args, o)
	}
	_, err := Snapshot(t.Context(), s.deps(), s.cfg(), s.opts("app"))
	if err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("err = %v", err)
	}
}

func TestSnapshotDryRunAndConfirmation(t *testing.T) {
	s := newSim(t)
	o := s.opts("app")
	o.Execute = false
	res, err := Snapshot(t.Context(), s.deps(), s.cfg(), o)
	if err != nil || res.Executed || len(s.log) != 0 {
		t.Fatalf("dry run: err=%v executed=%v log=%v", err, res.Executed, s.log)
	}
	if _, err := os.Stat(s.cfg().StateDir); err == nil {
		t.Error("dry run created the state dir")
	}

	d := s.deps()
	d.Confirm = func(*SnapshotPlan) (bool, error) { return false, nil }
	o = s.opts("app")
	o.Yes = false
	if _, err := Snapshot(t.Context(), d, s.cfg(), o); err != ErrCancelled {
		t.Fatalf("declined: %v", err)
	}
	if len(s.log) != 0 || !s.running["db"] {
		t.Errorf("declining must change nothing: %v", s.log)
	}
	// --yes skips the prompt entirely
	asked := false
	d.Confirm = func(*SnapshotPlan) (bool, error) { asked = true; return false, nil }
	o.Yes = true
	if _, err := Snapshot(t.Context(), d, s.cfg(), o); err != nil || asked {
		t.Errorf("--yes: err=%v asked=%v", err, asked)
	}
}

func TestSnapshotRefusesToDumpAStoppedDatabase(t *testing.T) {
	s := newSim(t)
	s.running["db"] = false
	_, err := Snapshot(t.Context(), s.deps(), s.cfg(), s.opts("db"))
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v", err)
	}
	for _, e := range s.log {
		if strings.HasPrefix(e, "stop") || strings.HasPrefix(e, "archive") {
			t.Errorf("nothing may be stopped or archived: %v", s.log)
		}
	}
}

func TestSnapshotNotEnoughSpaceStopsBeforeAnything(t *testing.T) {
	s := newSim(t)
	d := s.deps()
	d.FreeBytes = func(string) (int64, error) { return 100, nil }
	_, err := Snapshot(t.Context(), d, s.cfg(), s.opts("app"))
	if err == nil || !strings.Contains(err.Error(), "not enough free space") || len(s.log) != 0 {
		t.Fatalf("err=%v log=%v", err, s.log)
	}
}

func TestSnapshotLocksTheProject(t *testing.T) {
	s := newSim(t)
	cfg := s.cfg()
	// pretend another composesafe is running: our own pid holds the lock file
	projDir := filepath.Join(cfg.StateDir, "p")
	os.MkdirAll(projDir, 0o700)
	os.WriteFile(filepath.Join(projDir, "lock"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
	_, err := Snapshot(t.Context(), s.deps(), cfg, s.opts("app"))
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v", err)
	}
	if len(s.log) != 0 {
		t.Errorf("nothing may happen while locked: %v", s.log)
	}
}
