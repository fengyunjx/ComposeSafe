//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/errs"
	"composesafe/internal/registry"
	"composesafe/internal/snapshot"
	"composesafe/internal/workflow"
)

// The toy images are built once for the whole package.
var toyTags = map[string][2]string{ // tag -> {APP_VERSION, MODE}
	"1":                   {"1", "normal"},
	"2":                   {"2", "normal"},
	"2-crash":             {"2", "crash"},
	"2-migrate-then-fail": {"2", "migrate-then-fail"},
	"2-bad-smoke":         {"2", "bad-smoke"},
	"2-unhealthy":         {"2", "unhealthy"},
}

var buildToys sync.Once
var buildErr error

func ensureToyImages(t *testing.T) {
	t.Helper()
	buildToys.Do(func() {
		for tag, v := range toyTags {
			cmd := exec.Command("docker", "build", "-q", "-t", "cs-toy:"+tag,
				"--build-arg", "BASE="+toyBase(), "--build-arg", "APP_VERSION="+v[0], "--build-arg", "MODE="+v[1],
				filepath.Join("..", "..", "testdata", "toyapp"))
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build cs-toy:%s: %v\n%s", tag, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}

func toyBase() string {
	if v := os.Getenv("CS_TEST_TOYBASE"); v != "" {
		return v
	}
	return "nginx:alpine"
}

type offline struct{}

func (offline) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }

const toyCompose = `# toy stack for composesafe tests
services:
  app:
    image: cs-toy:1   # pinned on purpose
    ports:
      - "127.0.0.1::8080"
    volumes:
      - data:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "-O-", "http://127.0.0.1:8080/healthz"]
      interval: 1s
      timeout: 2s
      retries: 5
      start_period: 1s

volumes:
  data:
`

type toy struct {
	t       *testing.T
	project string
	dir     string
	file    string
	cfg     *config.Config
	runner  docker.Runner
}

func newToy(t *testing.T, compose string) *toy {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	ensureToyImages(t)
	var b [3]byte
	_, _ = rand.Read(b[:])
	y := &toy{t: t, project: "cstoy" + hex.EncodeToString(b[:]), dir: t.TempDir(), runner: docker.ExecRunner{}}
	y.file = filepath.Join(y.dir, "compose.yml")
	if err := os.WriteFile(y.file, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = filepath.Join(y.dir, "state")
	cfg.HelperImage = helperImage()
	cfg.Services = map[string]*config.Service{"app": {
		HealthTimeout: 30 * time.Second,
		Smoke: []config.Smoke{{Name: "health", Type: "http", URL: `http://{{addr "app" 8080}}/healthz`,
			ExpectStatus: 200, ExpectBodyContains: "ok", Retries: 3, Interval: time.Second, Timeout: 5 * time.Second}},
	}}
	y.cfg = cfg

	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", y.project, "-f", y.file, "down", "--volumes", "--remove-orphans", "--timeout", "2").Run()
		out, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", "name="+y.project).Output()
		for _, v := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "volume", "rm", "-f", v).Run()
		}
		tags, _ := exec.Command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "composesafe-keep/*").Output()
		for _, tag := range strings.Fields(string(tags)) {
			_ = exec.Command("docker", "rmi", tag).Run()
		}
	})
	y.compose("up", "-d", "--wait")
	return y
}

func (y *toy) compose(args ...string) string {
	y.t.Helper()
	return sh(y.t, y.dir, "docker", append([]string{"compose", "-p", y.project, "-f", y.file}, args...)...)
}

// volumeFile reads a file from a Docker volume.
func (y *toy) volumeFile(volume, name string) string {
	y.t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "--network", "none", "-v", volume+":/d:ro", helperImage(), "cat", "/d/"+name).CombinedOutput()
	if err != nil {
		return "<missing: " + strings.TrimSpace(string(out)) + ">"
	}
	return strings.TrimSpace(string(out))
}

func (y *toy) volume(name string) string { return y.project + "_" + name }

func (y *toy) get(path string) string {
	y.t.Helper()
	port := y.compose("port", "app", "8080")
	addr := "127.0.0.1" + port[strings.LastIndex(port, ":"):]
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return "<error: " + err.Error() + ">"
	}
	defer resp.Body.Close()
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return strings.TrimSpace(string(b[:n]))
}

func (y *toy) composeFile() string {
	b, err := os.ReadFile(y.file)
	if err != nil {
		y.t.Fatal(err)
	}
	return string(b)
}

func (y *toy) deps(r docker.Runner) workflow.UpgradeDeps {
	run := workflow.RunDeps{Runner: r, Now: time.Now, UID: os.Getuid(), GID: os.Getgid(), Version: "test",
		StableWindow: 2 * time.Second, Progress: func(key string, args ...any) { y.t.Logf("%s %v", key, args) }}
	return workflow.UpgradeDeps{
		Plan: workflow.PlanDeps{Runner: r, Now: time.Now,
			Registry:  registry.Client{AttemptTimeout: 50 * time.Millisecond, Opts: []remote.Option{remote.WithTransport(offline{})}},
			FreeBytes: func(string) (int64, error) { return 1 << 40, nil }},
		Snapshot: workflow.SnapshotDeps{Runner: r, Now: time.Now, UID: os.Getuid(), GID: os.Getgid(), Version: "test",
			Progress: func(key string, args ...any) { y.t.Logf("%s %v", key, args) }},
		Run: run,
	}
}

func (y *toy) upgrade(r docker.Runner, target string) (*workflow.UpgradeResult, error) {
	return workflow.Upgrade(context.Background(), y.deps(r), y.cfg, workflow.UpgradeOptions{
		ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project},
		Service:     "app", Target: target, Execute: true, Yes: true,
		AllowRisk: []string{"unversioned-tag", "major-bump", "image-variant-change"}, SkipRestoreCheck: true,
	})
}

// ---- scenarios ---------------------------------------------------------------

func TestUpgradeSuccess(t *testing.T) {
	y := newToy(t, toyCompose)
	if got := y.get("/version"); got != "v1" {
		t.Fatalf("before: %s", got)
	}
	res, err := y.upgrade(y.runner, "2")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if res.Outcome != workflow.OutcomeSuccess {
		t.Fatalf("outcome = %s (%s)", res.Outcome, res.Failure)
	}
	if got := y.get("/version"); got != "v2" {
		t.Errorf("after: %s", got)
	}
	if got := y.volumeFile(y.volume("data"), "schema"); got != "2" {
		t.Errorf("the migration should have run: schema=%s", got)
	}
	// Only the image changed in the user's file: comment, quoting, spacing stay.
	want := strings.Replace(toyCompose, "image: cs-toy:1   # pinned on purpose", "image: cs-toy:2   # pinned on purpose", 1)
	if y.composeFile() != want {
		t.Errorf("compose file changed beyond the image:\n%s", y.composeFile())
	}
	m, err := snapshot.Load(res.Dir)
	if err != nil || m.Upgrade == nil || m.Upgrade.To != "cs-toy:2" || m.PostUpgrade == nil || m.PostUpgrade.ToImage.ImageID == "" {
		t.Errorf("manifest: %+v %v", m, err)
	}
	if b, err := os.ReadFile(res.ReportPath); err != nil || !strings.Contains(string(b), "the upgrade succeeded") {
		t.Errorf("report: %v", err)
	}
}

// failing new versions must be rolled back, with the data restored when the
// new version ran.
func TestUpgradeFailuresRollBack(t *testing.T) {
	cases := []struct {
		name, target string
		migrated     bool // did the failed version change the volume it ran on?
	}{
		{"crashes at start", "2-crash", false},
		{"migrates, then dies", "2-migrate-then-fail", true},
		{"never becomes healthy", "2-unhealthy", true},
		{"healthy but fails the smoke check", "2-bad-smoke", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := newToy(t, toyCompose)
			res, err := y.upgrade(y.runner, tc.target)
			if !errors.Is(err, errs.ErrRolledBack) {
				t.Fatalf("err = %v, want ErrRolledBack (outcome %s, failure %s)", err, res.Outcome, res.Failure)
			}
			if res.Outcome != workflow.OutcomeRolledBack || res.Rollback == nil || res.Rollback.Kind != workflow.RollbackData {
				t.Fatalf("outcome %s rollback %+v", res.Outcome, res.Rollback)
			}
			if got := y.get("/version"); got != "v1" {
				t.Errorf("the old version must be serving again, got %q", got)
			}
			if y.get("/healthz") != "ok" {
				t.Error("old version must pass its checks")
			}

			// The data the app uses now is the snapshot's: schema 1, no migration trace.
			rb := res.Rollback.Restored
			if len(rb) != 1 || rb[0].Old != y.volume("data") || !strings.HasPrefix(rb[0].New, y.volume("data")+"_rb_") {
				t.Fatalf("restored: %+v", rb)
			}
			if got := y.volumeFile(rb[0].New, "schema"); got != "1" {
				t.Errorf("restored volume schema = %s, want 1", got)
			}
			// The volume that held the failed state is untouched, evidence included.
			wantOld := "1"
			if tc.migrated {
				wantOld = "2"
			}
			if got := y.volumeFile(rb[0].Old, "schema"); got != wantOld {
				t.Errorf("failed-state volume schema = %s, want %s: it must be left as it was", got, wantOld)
			}

			// The compose file is back to the old image and points at the restored volume,
			// so a plain `docker compose up` keeps working with no help from composesafe.
			f := y.composeFile()
			if !strings.Contains(f, "image: cs-toy:1   # pinned on purpose") || strings.Contains(f, "cs-toy:2") {
				t.Errorf("image not restored:\n%s", f)
			}
			if !strings.Contains(f, "data_rb_"+res.RunID+":/data") || !strings.Contains(f, "external: true") || !strings.Contains(f, "name: "+rb[0].New) {
				t.Errorf("compose does not point at the restored volume:\n%s", f)
			}
			y.compose("up", "-d", "--wait")
			if got := y.get("/version"); got != "v1" {
				t.Errorf("after a plain compose up: %q", got)
			}
			if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "FAILED") && !strings.Contains(string(b), "rolled back") {
				t.Error("report should say what happened")
			}
		})
	}
}

func TestPullFailureOnlyRevertsTheFiles(t *testing.T) {
	y := newToy(t, toyCompose)
	before := y.composeFile()
	res, err := y.upgrade(y.runner, "no-such-tag")
	if !errors.Is(err, errs.ErrRolledBack) || res.Outcome != workflow.OutcomeReverted {
		t.Fatalf("err = %v outcome = %s", err, res.Outcome)
	}
	if y.composeFile() != before {
		t.Errorf("compose file must be byte-identical again:\n%s", y.composeFile())
	}
	if res.Rollback != nil {
		t.Error("no container was replaced, so there is nothing to roll back")
	}
	if got := y.get("/version"); got != "v1" {
		t.Errorf("service must be untouched: %q", got)
	}
	if got := y.volumeFile(y.volume("data"), "schema"); got != "1" {
		t.Errorf("schema = %s", got)
	}
}

func TestDryRunChangesNothingAndShowsTheEdit(t *testing.T) {
	y := newToy(t, toyCompose)
	before := y.composeFile()
	o := workflow.UpgradeOptions{ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project},
		Service: "app", Target: "2", AllowRisk: nil}
	res, err := workflow.Upgrade(context.Background(), y.deps(y.runner), y.cfg, o)
	if err != nil || res.Outcome != workflow.OutcomeDryRun || res.Executed {
		t.Fatalf("%v %+v", err, res)
	}
	if len(res.Preview.Edits) != 1 || !strings.Contains(res.Preview.Edits[0], "cs-toy:1 → cs-toy:2") {
		t.Errorf("edits: %v", res.Preview.Edits)
	}
	if y.composeFile() != before || y.get("/version") != "v1" {
		t.Error("a dry run must change nothing")
	}
	if _, err := os.Stat(y.cfg.StateDir); err == nil {
		t.Error("a dry run must not create state")
	}
}

func TestHighRisksNeedToBeAccepted(t *testing.T) {
	y := newToy(t, toyCompose)
	o := workflow.UpgradeOptions{ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project},
		Service: "app", Target: "2", Execute: true, Yes: true} // no --allow-risk
	res, err := workflow.Upgrade(context.Background(), y.deps(y.runner), y.cfg, o)
	if !errors.Is(err, errs.ErrBlocked) || res.Executed {
		t.Fatalf("err = %v executed = %v", err, res.Executed)
	}
	if !strings.Contains(err.Error(), "--allow-risk=") {
		t.Errorf("the error must say how to accept: %v", err)
	}
	if y.get("/version") != "v1" {
		t.Error("nothing may have changed")
	}
}

// A crash in the middle of an upgrade leaves a manifest and a run log; the
// rollback command recovers from those alone.
func TestRollbackAfterTheProcessDiedMidUpgrade(t *testing.T) {
	y := newToy(t, toyCompose)
	crash := &crashAfterUp{Runner: y.runner}
	var res *workflow.UpgradeResult
	func() {
		defer func() { recover() }() // the "crash"
		res, _ = y.upgrade(crash, "2")
	}()
	if !crash.fired {
		t.Fatal("the simulated crash never happened")
	}
	// The new version is running; no rollback has been done.
	if got := y.get("/version"); got != "v2" {
		t.Fatalf("expected the new version to be running after the crash, got %q", got)
	}
	_ = res

	root := filepath.Join(y.cfg.StateDir, y.project)
	ents, _ := os.ReadDir(root)
	if len(ents) != 1 {
		t.Fatalf("run directories: %v", ents)
	}
	rd := y.deps(y.runner).Run
	// dry run first
	rep, err := workflow.Rollback(context.Background(), workflow.RollbackDeps{Run: rd}, y.cfg, workflow.RollbackOptions{
		ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project}})
	if err != nil || rep.Executed || rep.Preview.Kind != workflow.RollbackData || len(rep.Preview.ChangedFiles) != 0 {
		t.Fatalf("preview: %v %+v", err, rep)
	}
	rep, err = workflow.Rollback(context.Background(), workflow.RollbackDeps{Run: rd}, y.cfg, workflow.RollbackOptions{
		ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project}, Execute: true, Yes: true})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := y.get("/version"); got != "v1" {
		t.Errorf("after the rollback: %q", got)
	}
	if rep.Result.Verify == nil || !rep.Result.Verify.OK() {
		t.Errorf("verify: %+v", rep.Result.Verify)
	}
}

// crashAfterUp panics once `compose up` has succeeded, standing in for a
// process that was killed at that moment.
type crashAfterUp struct {
	docker.Runner
	fired bool
}

func (c *crashAfterUp) Run(ctx context.Context, args []string, o docker.Opts) (docker.Result, error) {
	res, err := c.Runner.Run(ctx, args, o)
	if !c.fired && err == nil && len(args) > 0 && strings.Contains(strings.Join(args, " "), " up -d --no-deps --wait") {
		c.fired = true
		panic("simulated crash")
	}
	return res, err
}

func TestRollbackNoticesEditsMadeAfterTheUpgrade(t *testing.T) {
	y := newToy(t, toyCompose)
	if _, err := y.upgrade(y.runner, "2"); err != nil {
		t.Fatal(err)
	}
	// somebody edits the file afterwards
	edited := strings.Replace(y.composeFile(), "interval: 1s", "interval: 2s", 1)
	if err := os.WriteFile(y.file, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	rd := y.deps(y.runner).Run
	opts := workflow.RollbackOptions{ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project}, Execute: true}

	// no consent: refused, nothing touched
	_, err := workflow.Rollback(context.Background(), workflow.RollbackDeps{Run: rd}, y.cfg, opts)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}
	// declined at the prompt
	var seen *workflow.RollbackPreview
	_, err = workflow.Rollback(context.Background(), workflow.RollbackDeps{Run: rd,
		Confirm: func(pv *workflow.RollbackPreview) (bool, error) { seen = pv; return false, nil }}, y.cfg, opts)
	if !errors.Is(err, workflow.ErrCancelled) {
		t.Fatalf("err = %v", err)
	}
	if seen == nil || len(seen.ChangedFiles) != 1 || !strings.Contains(seen.Diff, "interval: 2s") {
		t.Errorf("the preview must show what would be overwritten: %+v", seen)
	}
	if y.composeFile() != edited || y.get("/version") != "v2" {
		t.Error("declining must change nothing")
	}
}

// With a database in the stack the upgrade of the app must not touch it, and
// a data rollback must bring back the database as it was at the snapshot.
func TestDatabaseStackRollbackRestoresTheDatabase(t *testing.T) {
	compose := strings.Replace(toyCompose, "services:\n", `services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_PASSWORD: pw
      POSTGRES_USER: appuser
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U appuser"]
      interval: 2s
      retries: 30
`, 1)
	compose = strings.Replace(compose, "    image: cs-toy:1   # pinned on purpose\n", "    image: cs-toy:1   # pinned on purpose\n    depends_on:\n      db: {condition: service_healthy}\n", 1)
	compose = strings.Replace(compose, "volumes:\n  data:\n", "volumes:\n  data:\n  pgdata:\n", 1)
	y := newToy(t, compose)
	waitDB := func() {
		for i := 0; i < 60; i++ {
			if exec.Command("docker", "compose", "-p", y.project, "-f", y.file, "exec", "-T", "db", "pg_isready", "-q", "-h", "127.0.0.1", "-U", "appuser").Run() == nil {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatal("db not ready")
	}
	psql := func(sql string) string {
		return y.compose("exec", "-T", "db", "psql", "-U", "appuser", "-d", "postgres", "-tAc", sql)
	}
	waitDB()
	psql("create table orders(id int)")
	psql("insert into orders values (1),(2),(3)")

	// While the upgrade is in flight (after the snapshot) a customer places an order.
	rec := &recorder{Runner: y.runner, onPull: func() {
		if out, err := exec.Command("docker", "compose", "-p", y.project, "-f", y.file, "exec", "-T", "db", "psql", "-U", "appuser", "-d", "postgres", "-c", "insert into orders values (4)").CombinedOutput(); err != nil {
			t.Errorf("late write: %v %s", err, out)
		}
	}}
	res, err := y.upgrade(rec, "2-migrate-then-fail")
	if !errors.Is(err, errs.ErrRolledBack) {
		t.Fatalf("err = %v (%s)", err, res.Failure)
	}

	// `up` for the app used --no-deps: the database is not touched by the upgrade itself.
	sawUp := false
	for _, c := range rec.lines() {
		if strings.Contains(c, " up -d") && strings.Contains(c, "upgrade") {
			t.Logf("unexpected: %s", c)
		}
		if strings.Contains(c, "up -d --no-deps --wait --wait-timeout 30 app") {
			sawUp = true
		}
	}
	if !sawUp {
		t.Errorf("the app must be started with --no-deps:\n%s", strings.Join(rec.lines(), "\n"))
	}

	// Data rollback: the database is the snapshot's (3 orders); the late write sits in the old volume.
	waitDB()
	if got := psql("select count(*) from orders"); got != "3" {
		t.Errorf("restored database has %s orders, want 3 (the snapshot)", got)
	}
	var oldPG string
	for _, r := range res.Rollback.Restored {
		if r.Service == "db" {
			oldPG = r.Old
		}
	}
	if oldPG != y.volume("pgdata") {
		t.Fatalf("restored mounts: %+v", res.Rollback.Restored)
	}
	f := y.composeFile()
	if !strings.Contains(f, "pgdata_rb_"+res.RunID+":/var/lib/postgresql/data") || !strings.Contains(f, "data_rb_"+res.RunID+":/data") {
		t.Errorf("compose should point both services at the restored volumes:\n%s", f)
	}
	if y.get("/version") != "v1" {
		t.Error("old app must be serving")
	}
	b, _ := os.ReadFile(res.ReportPath)
	if !strings.Contains(string(b), "NOT in the restored data") {
		t.Errorf("the report must state the data-loss window:\n%s", b)
	}
}

type recorder struct {
	docker.Runner
	mu     sync.Mutex
	calls  []string
	onPull func()
	pullAt string // suffix of the command that triggers onPull; " pull app" when empty
	done   bool
}

func (r *recorder) Run(ctx context.Context, args []string, o docker.Opts) (docker.Result, error) {
	line := strings.Join(args, " ")
	r.mu.Lock()
	r.calls = append(r.calls, line)
	at := r.pullAt
	if at == "" {
		at = " pull app"
	}
	fire := !r.done && r.onPull != nil && strings.HasSuffix(line, at)
	if fire {
		r.done = true
	}
	r.mu.Unlock()
	if fire {
		r.onPull()
	}
	return r.Runner.Run(ctx, args, o)
}

func (r *recorder) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}
