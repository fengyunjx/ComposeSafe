package workflow

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggreg "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"composesafe/internal/config"
	"composesafe/internal/docker/dockertest"
	"composesafe/internal/notes"
	"composesafe/internal/plan"
	"composesafe/internal/registry"
	"composesafe/internal/risk"
	"composesafe/internal/runlog"
)

type env struct {
	t       *testing.T
	host    string // fake registry
	file    string
	fake    *dockertest.Fake
	deps    PlanDeps
	cfg     *config.Config
	pgPages *[]string
}

// newEnv builds a project with db (official postgres from a fake registry),
// app (depends on db) and a build-only worker, and fakes for Docker, the
// registry and postgresql.org.
func newEnv(t *testing.T, dbTag string, dbSize string) *env {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")

	srv := httptest.NewServer(ggreg.New(ggreg.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	img, _ := random.Image(64, 1)
	for _, tag := range []string{"14.5-alpine", "14.24-alpine", "15.19-alpine", "16.4-alpine", "18.6-alpine"} {
		ref, _ := name.NewTag(host + "/library/postgres:" + tag)
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
	}

	var pages []string
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := strings.Trim(strings.TrimPrefix(r.URL.Path, "/"), "/")
		pages = append(pages, v)
		fmt.Fprintf(w, `<div class="sect1"><h2 class="title">Release %s</h2><p>Notes for %s.</p></div><div class="navfooter">`, v, v)
	}))
	t.Cleanup(pg.Close)

	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yml")
	os.WriteFile(file, []byte("x"), 0o600)
	dbImage := host + "/library/postgres:" + dbTag
	composeJSON := fmt.Sprintf(`{"name":"p","services":{
	  "db":{"image":%q,"volumes":[{"type":"volume","source":"pgdata","target":"/var/lib/postgresql/data"}]},
	  "app":{"image":"example/app:1.0","depends_on":{"db":{"condition":"service_started"}}},
	  "worker":{"build":{"context":"."}}},
	  "volumes":{"pgdata":{"name":"p_pgdata"}}}`, dbImage)

	f := &dockertest.Fake{Responses: map[string]dockertest.Response{
		"context inspect":                                   {Stdout: `[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`},
		"version --format {{.Server.Version}}":              {Stdout: "29.0.0"},
		"compose version --short":                           {Stdout: "5.0.0"},
		"compose -p p -f " + file + " config --format json": {Stdout: composeJSON},
		"image inspect --format {{json .}} " + dbImage:      {Stdout: `{"Id":"sha256:aa","RepoDigests":["x@sha256:11"],"Config":{"Labels":null}}`},
		"image inspect --format {{json .}} example/app:1.0": {Stderr: "Error response from daemon: No such image: example/app:1.0", Err: fmt.Errorf("exit 1")},
		"system df -v --format json":                        {Stdout: fmt.Sprintf(`{"Volumes":[{"Name":"p_pgdata","Size":%q}]}`, dbSize)},
	}}

	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	return &env{t: t, host: host, file: file, fake: f, cfg: cfg, pgPages: &pages, deps: PlanDeps{
		Runner:    f,
		Registry:  registry.Client{AttemptTimeout: 2 * time.Second},
		Postgres:  notes.Postgres{BaseURL: pg.URL},
		Now:       func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
		FreeBytes: func(string) (int64, error) { return 100_000_000_000, nil },
		DirSize:   func(string) int64 { return -1 },
	}}
}

func (e *env) opts(service, target string) PlanOptions {
	return PlanOptions{ScanOptions: ScanOptions{Files: []string{e.file}, Project: "p"}, Service: service, Target: target, WriteReport: true}
}

func riskIDs(p *plan.Plan) map[string]risk.Level {
	m := map[string]risk.Level{}
	for _, r := range p.Risks {
		id := r.ID
		if r.Variant != "" {
			id += "." + r.Variant
		}
		m[id] = r.Level
	}
	return m
}

func TestPlanListsCandidates(t *testing.T) {
	e := newEnv(t, "14.5-alpine", "1.5GB")
	res, err := Plan(t.Context(), e.deps, e.cfg, e.opts("db", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan != nil {
		t.Fatal("no target: no plan")
	}
	var got []string
	for _, c := range res.Candidates {
		got = append(got, c.Label+"="+c.Tag)
	}
	if strings.Join(got, " ") != "same-major=14.24-alpine next-major=15.19-alpine latest=18.6-alpine" {
		t.Errorf("candidates = %v", got)
	}
	if _, err := os.Stat(e.cfg.StateDir); err == nil {
		t.Error("listing candidates must not create state")
	}
}

func TestPlanPostgresMajorUpgrade(t *testing.T) {
	e := newEnv(t, "14.5-alpine", "1.5GB")
	res, err := Plan(t.Context(), e.deps, e.cfg, e.opts("db", "16.4-alpine"))
	if err != nil {
		t.Fatal(err)
	}
	p := res.Plan
	if p.Path != plan.PathPGDumpRestore || p.Blocked {
		t.Errorf("path=%s blocked=%v", p.Path, p.Blocked)
	}
	ids := riskIDs(p)
	if ids["pg-major-bump"] != risk.Critical || ids["pg-skip-versions"] != risk.High || ids["pg-eol-source.soon"] != risk.Info {
		t.Errorf("risks: %v", ids)
	}
	if _, ok := ids["digest-unpinned"]; ok {
		t.Error("digest is known from the local image")
	}
	// release notes: 15.0 and 16.0 (majors) plus 16.4 (target minor)
	if strings.Join(*e.pgPages, ",") != "15.0,16.0,16.4" || len(p.Notes) != 3 || p.NotesState != "ok" {
		t.Errorf("pages=%v notes=%d state=%s", *e.pgPages, len(p.Notes), p.NotesState)
	}
	if p.DataSize != 1_500_000_000 || p.BackupFree != 100_000_000_000 {
		t.Errorf("sizes: data=%d free=%d", p.DataSize, p.BackupFree)
	}

	// the report and the run log exist and are private
	b, err := os.ReadFile(p.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Upgrade assessment", "postgres:14.5-alpine", "pg-major-bump", "Release 15", "Nothing was changed"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report missing %q", want)
		}
	}
	evs, err := runlog.Read(filepath.Join(filepath.Dir(p.ReportPath), "run.jsonl"))
	if err != nil || len(evs) != 2 || evs[0].Step != "assess.notes" || evs[1].Step != "assess.risks" {
		t.Errorf("run log: %v %+v", err, evs)
	}
	if fi, _ := os.Stat(p.ReportPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("report mode = %v", fi.Mode().Perm())
	}
	// nothing but read-only docker commands
	for _, c := range e.fake.Calls {
		switch c[0] {
		case "context", "version", "compose", "image", "system", "info":
		default:
			t.Errorf("unexpected docker command %v", c)
		}
		if c[0] == "compose" && len(c) > 1 && (c[len(c)-1] == "up" || c[len(c)-1] == "pull") {
			t.Errorf("plan must not change anything: %v", c)
		}
	}
}

func TestPlanSimpleUpgradeOfApp(t *testing.T) {
	e := newEnv(t, "16.4-alpine", "2GB")
	e.cfg.Services = map[string]*config.Service{"app": {SourceRepo: "postgres"}} // any source works for the test
	res, err := Plan(t.Context(), e.deps, e.cfg, e.opts("app", "1.1"))
	if err != nil {
		t.Fatal(err)
	}
	p := res.Plan
	if p.Path != plan.PathSimple {
		t.Errorf("path = %s", p.Path)
	}
	// app is stateless but depends on db: the db's data is what a snapshot must protect
	if p.DataSize != 2_000_000_000 {
		t.Errorf("data size = %d, want the dependency's volume", p.DataSize)
	}
	// image not present locally: digest unknown
	if riskIDs(p)["digest-unpinned"] != risk.Medium {
		t.Errorf("risks: %v", riskIDs(p))
	}
}

func TestPlanBlocksOnLowDisk(t *testing.T) {
	e := newEnv(t, "14.5-alpine", "50GB")
	e.deps.FreeBytes = func(string) (int64, error) { return 60_000_000_000, nil } // needs 110 GB
	res, err := Plan(t.Context(), e.deps, e.cfg, e.opts("db", "14.24-alpine"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Plan.Blocked || riskIDs(res.Plan)["low-disk"] != risk.Block {
		t.Errorf("risks: %v", riskIDs(res.Plan))
	}
}

func TestPlanRegistryDownStillAssesses(t *testing.T) {
	e := newEnv(t, "14.5-alpine", "1GB")
	e.deps.Registry = registry.Client{AttemptTimeout: 200 * time.Millisecond}
	// point the project at a registry that does not answer
	dead := "127.0.0.1:1"
	moved := map[string]dockertest.Response{}
	for k, v := range e.fake.Responses {
		moved[strings.ReplaceAll(k, e.host, dead)] = dockertest.Response{
			Stdout: strings.ReplaceAll(v.Stdout, e.host, dead), Stderr: v.Stderr, Err: v.Err}
	}
	e.fake.Responses = moved
	res, err := Plan(t.Context(), e.deps, e.cfg, e.opts("db", "14.24-alpine"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan.Warnings) == 0 || !strings.Contains(res.Plan.Warnings[0], "target not validated") {
		t.Errorf("warnings: %v", res.Plan.Warnings)
	}
	// listing candidates, by contrast, needs the registry and says how to continue
	_, err = Plan(t.Context(), e.deps, e.cfg, e.opts("db", ""))
	if err == nil || !strings.Contains(err.Error(), "db=<tag>") {
		t.Errorf("err = %v", err)
	}
}

func TestPlanRejects(t *testing.T) {
	e := newEnv(t, "14.5-alpine", "1GB")
	for name, tc := range map[string]struct{ svc, target, want string }{
		"unknown service": {"nope", "1", "unknown service"},
		"build-only":      {"worker", "1", "built locally"},
		"same tag":        {"db", "14.5-alpine", "already uses"},
		"missing tag":     {"db", "99.9-alpine", "does not exist"},
	} {
		_, err := Plan(t.Context(), e.deps, e.cfg, e.opts(tc.svc, tc.target))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, tc.want)
		}
	}
}

func TestPlanWithoutWritableStateStillWorks(t *testing.T) {
	e := newEnv(t, "14.5-alpine", "1GB")
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	e.cfg.StateDir = filepath.Join(blocker, "sub") // a path below a regular file cannot be created
	res, err := Plan(t.Context(), e.deps, e.cfg, e.opts("db", "14.24-alpine"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.ReportPath != "" || len(res.Plan.Warnings) == 0 {
		t.Errorf("expected a warning and no report: %+v", res.Plan.Warnings)
	}
}

func TestProtectedServices(t *testing.T) {
	e := newEnv(t, "16.4-alpine", "1GB")
	scan, err := Scan(t.Context(), e.fake, e.cfg, ScanOptions{Files: []string{e.file}, Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := protectedServices(scan.Project, e.cfg, "app"); strings.Join(got, ",") != "db" {
		t.Errorf("app -> %v", got)
	}
	if got := protectedServices(scan.Project, e.cfg, "db"); strings.Join(got, ",") != "db" {
		t.Errorf("db -> %v", got)
	}
	e.cfg.Services = map[string]*config.Service{"worker": {SnapshotWith: []string{"db"}}}
	if got := protectedServices(scan.Project, e.cfg, "worker"); strings.Join(got, ",") != "db" {
		t.Errorf("worker (snapshot_with) -> %v", got)
	}
}

func TestFreeBytesAndDirSize(t *testing.T) {
	dir := t.TempDir()
	if n, err := freeBytes(filepath.Join(dir, "not", "yet", "created")); err != nil || n <= 0 {
		t.Errorf("freeBytes of a missing path should use the nearest parent: %d %v", n, err)
	}
	os.WriteFile(filepath.Join(dir, "a"), make([]byte, 1000), 0o600)
	os.Mkdir(filepath.Join(dir, "sub"), 0o700)
	os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 500), 0o600)
	if n := dirSize(dir); n != 1500 {
		t.Errorf("dirSize = %d", n)
	}
	if dirSize(filepath.Join(dir, "missing")) != -1 {
		t.Error("missing dir must be -1 (unknown), not 0")
	}
}
