package workflow

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"composesafe/internal/compose"
	"composesafe/internal/docker"
	"composesafe/internal/docker/dockertest"
	"composesafe/internal/errs"
	"composesafe/internal/plan"
	"composesafe/internal/registry"
	"composesafe/internal/snapshot"
	"composesafe/internal/version"
)

func pgSvc(target string, m ...compose.Mount) *compose.Service {
	return &compose.Service{Name: "db", Image: "postgres:16", Kind: compose.KindPostgres, Mounts: m}
}

func imageTo(tag string) compose.ImageRef {
	v, _ := version.ParseTag(tag)
	return compose.ImageRef{Raw: "postgres:" + tag, Tag: tag, Version: v}
}

func TestPlanPGMove(t *testing.T) {
	proj := &compose.Project{Services: map[string]*compose.Service{
		"db":  {Name: "db"},
		"app": {Name: "app", DependsOn: []string{"db"}},
	}}
	named := compose.Mount{Type: "volume", Source: "pgdata", VolumeName: "p_pgdata", Target: pgDataLegacy}

	mv, err := planPGMove(proj, pgSvc("", named), imageTo("17"))
	if err != nil || mv.Old != "p_pgdata" || mv.New != "p_pgdata_pg17" || mv.NewTarget != "" || mv.Suffix != "_pg17" ||
		strings.Join(mv.Writers, ",") != "app" {
		t.Fatalf("named, 16 -> 17: %+v %v", mv, err)
	}

	// Postgres 18 keeps its data under /var/lib/postgresql: the mount point moves with it
	mv, err = planPGMove(proj, pgSvc("", named), imageTo("18"))
	if err != nil || mv.NewTarget != pgDataParent || mv.New != "p_pgdata_pg18" {
		t.Fatalf("named, 17 -> 18: %+v %v", mv, err)
	}
	// already at the parent path (a 18 layout): unchanged
	atParent := named
	atParent.Target = pgDataParent
	if mv, err = planPGMove(proj, pgSvc("", atParent), imageTo("19")); err != nil || mv.NewTarget != "" {
		t.Fatalf("parent path: %+v %v", mv, err)
	}

	bind := compose.Mount{Type: "bind", Source: "/srv/pg/", Target: pgDataLegacy}
	if mv, err = planPGMove(proj, pgSvc("", bind), imageTo("17")); err != nil || mv.Old != "/srv/pg/" || mv.New != "/srv/pg_pg17" || mv.Type != "bind" {
		t.Fatalf("bind: %+v %v", mv, err)
	}

	anon := compose.Mount{Type: "volume", Target: pgDataLegacy}
	if _, err = planPGMove(proj, pgSvc("", anon), imageTo("17")); err == nil {
		t.Error("an anonymous volume cannot be moved")
	}
	ro := named
	ro.ReadOnly = true
	if _, err = planPGMove(proj, pgSvc("", ro), imageTo("17")); err == nil {
		t.Error("no writable data mount")
	}
	other := compose.Mount{Type: "volume", Source: "x", VolumeName: "p_x", Target: "/docker-entrypoint-initdb.d"}
	if _, err = planPGMove(proj, pgSvc("", other), imageTo("17")); err == nil {
		t.Error("only the data mount counts")
	}
	if _, err = planPGMove(proj, pgSvc("", named), compose.ImageRef{Raw: "postgres:latest"}); err == nil {
		t.Error("target without a version")
	}
}

const pgCompose = `services:
  db:
    image: postgres:16   # the database
    volumes:
      - pgdata:/var/lib/postgresql/data
  app:
    image: x/app:1
    depends_on: [db]
volumes:
  pgdata:
`

func TestMoveEditsBuildOnTheImageEdit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "compose.yml")
	os.WriteFile(f, []byte(pgCompose), 0o644)

	edits, err := compose.EditImage([]string{f}, nil, "db", "postgres:16", "postgres:18")
	if err != nil {
		t.Fatal(err)
	}
	mv := &pgMove{Service: "db", Type: "volume", Old: "p_pgdata", New: "p_pgdata_pg18", Target: pgDataLegacy, NewTarget: pgDataParent, Suffix: "_pg18"}
	edits, err = pgMoveEdits([]string{f}, edits, mv)
	if err != nil || len(edits) != 2 {
		t.Fatalf("%v %d", err, len(edits))
	}
	undo, err := compose.ApplyEdits(edits)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	want := "services:\n  db:\n    image: postgres:18   # the database\n    volumes:\n      - pgdata_pg18:/var/lib/postgresql\n" +
		"  app:\n    image: x/app:1\n    depends_on: [db]\nvolumes:\n  pgdata:\n  pgdata_pg18:\n    external: true\n    name: p_pgdata_pg18\n"
	if string(b) != want {
		t.Errorf("got\n%s\nwant\n%s", b, want)
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	if b, _ = os.ReadFile(f); string(b) != pgCompose {
		t.Errorf("undo must restore the original byte for byte:\n%s", b)
	}

	// a volume that is already declared under the new key is refused, not duplicated
	os.WriteFile(f, []byte(strings.Replace(pgCompose, "  pgdata:\n", "  pgdata:\n  pgdata_pg18:\n", 1)), 0o644)
	var me *compose.ManualEditError
	if _, err := pgMoveEdits([]string{f}, nil, mv); !errors.As(err, &me) {
		t.Errorf("already declared: %v", err)
	}
}

// ---- the whole flow against a pretend daemon --------------------------------------

const pgSimJSON = `{"name":"p","services":{
 "db":{"image":"postgres:16","healthcheck":{"test":["CMD","true"]},"volumes":[{"type":"volume","source":"pgdata","target":"/var/lib/postgresql/data"}]},
 "app":{"image":"x/app:1","healthcheck":{"test":["CMD","true"]},"depends_on":{"db":{"condition":"service_started"}}}},
 "volumes":{"pgdata":{"name":"p_pgdata"}}}`

// pgSim extends sim with what a Postgres major upgrade does on top of a
// snapshot: new volume, `compose up`/`pull`, restore into the new server.
type pgSim struct {
	*sim
	failRestore bool
	failUpInit  bool

	// rehearsal
	cmds         []string // "<project> <verb> <args>" of every compose command
	volumes      []string // volumes created with `docker volume create`
	failBaseline bool     // the copy's full `up` (no service named) fails
	failAppUp    bool     // `up` of the app in the copy fails after the baseline
	cpRuns       []string
}

func newPGSim(t *testing.T) *pgSim {
	s := &pgSim{sim: newSim(t)}
	if err := os.WriteFile(s.file, []byte(pgCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	s.fake.Handler = s.handle2
	return s
}

func (s *pgSim) handle2(args []string, o docker.Opts) (dockertest.Response, bool) {
	switch {
	case args[0] == "volume" && args[1] == "ls":
		return ok(strings.Join(s.volumes, "\n"))
	case args[0] == "volume" && args[1] == "rm":
		s.note("volume-rm %s", args[2])
		for i, v := range s.volumes {
			if v == args[2] {
				s.volumes = append(s.volumes[:i], s.volumes[i+1:]...)
				break
			}
		}
		return ok("")
	case args[0] == "image" && args[1] == "ls":
		return ok("")
	case args[0] == "rmi":
		s.note("rmi %s", args[1])
		return ok("")
	case args[0] == "run" && containsArg(args, "cp -a /from/. /to/"):
		s.cpRuns = append(s.cpRuns, strings.Join(args, " "))
		return ok("")
	case args[0] == "volume" && args[1] == "inspect":
		return dockertest.Response{Stdout: "[]", Stderr: "Error: No such volume", Err: errors.New("exit 1")}, true
	case args[0] == "volume" && args[1] == "create":
		s.note("volume-create %s", args[len(args)-1])
		s.volumes = append(s.volumes, args[len(args)-1])
		return ok("")
	case args[0] == "network":
		return ok("")
	case args[0] == "rm":
		return ok("")
	case args[0] == "ps" && containsArg(args, "--format"):
		return ok("")
	case args[0] == "ps": // docker ps -q --filter label=...service=NAME
		svc := strings.TrimPrefix(args[len(args)-1], "label=com.docker.compose.service=")
		if s.running[svc] || containsArg(args, "-a") {
			return ok("cid-" + svc)
		}
		return ok("")
	case args[0] == "stop" && len(args) > 1 && strings.HasPrefix(args[len(args)-1], "cid-"):
		var names []string
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "cid-") {
				n := strings.TrimPrefix(a, "cid-")
				s.running[n] = false
				names = append(names, n)
			}
		}
		s.note("docker-stop %s", strings.Join(names, ","))
		return ok("")
	case args[0] == "inspect" && containsArg(args, "{{.State.StartedAt}}"):
		return ok("2026-11-06T10:00:00Z")
	case args[0] == "compose" && args[1] != "version":
		return s.compose2(args, o)
	}
	return s.sim.handle(args, o)
}

func (s *pgSim) compose2(args []string, o docker.Opts) (dockertest.Response, bool) {
	i := 1
	var files []string
	for i < len(args) && (args[i] == "-p" || args[i] == "-f") {
		if args[i] == "-f" {
			files = append(files, filepath.Base(args[i+1]))
		}
		i += 2
	}
	verb, rest := args[i], args[i+1:]
	project := args[2]
	s.cmds = append(s.cmds, project+" "+verb+" "+strings.Join(rest, " "))
	last := rest[len(rest)-1]
	names := func(from int) []string {
		var out []string
		for _, a := range rest[from:] {
			if !strings.HasPrefix(a, "-") && a != "120" {
				out = append(out, a)
			}
		}
		return out
	}
	switch verb {
	case "config":
		if containsArg(rest, "-q") {
			return ok("")
		}
		return ok(pgSimJSON)
	case "ps":
		if containsArg(rest, "--status") && !s.running[last] {
			return ok("")
		}
		return ok("cid-" + last)
	case "stop":
		for _, n := range rest {
			s.running[n] = false
		}
		s.note("stop %s", strings.Join(rest, ","))
		return ok("")
	case "start":
		for _, n := range rest {
			s.running[n] = true
		}
		s.note("start %s", strings.Join(rest, ","))
		return ok("")
	case "down":
		s.note("down %s", project)
		return ok("")
	case "pull":
		b, _ := os.ReadFile(s.file)
		s.note("pull (image %s)", map[bool]string{true: "new", false: "old"}[strings.Contains(string(b), "postgres:17")])
		return ok("")
	case "up":
		svcs := names(0)
		if project != "p" && len(svcs) == 0 && s.failBaseline {
			return dockertest.Response{Stderr: "app cannot reach its mail server", Err: errors.New("exit 1")}, true
		}
		if project != "p" && s.failAppUp && len(svcs) == 1 && svcs[0] == "app" && !strings.Contains(strings.Join(files, ","), "rollback-override") {
			return dockertest.Response{Stderr: "app crashed", Err: errors.New("exit 1")}, true
		}
		ov := ""
		if len(files) > 1 {
			ov = " +" + files[len(files)-1]
		}
		b, _ := os.ReadFile(s.file)
		vol := "old-volume"
		if strings.Contains(string(b), "pgdata_pg17") {
			vol = "new-volume"
		}
		s.note("up %s%s [%s]", strings.Join(svcs, ","), ov, vol)
		if s.failUpInit && strings.HasSuffix(ov, "pgmajor-override.yml") {
			return dockertest.Response{Stderr: "container exited", Err: errors.New("exit 1")}, true
		}
		for _, n := range svcs {
			s.running[n] = true
		}
		return ok("")
	case "exec":
		script := last
		switch {
		case strings.Contains(script, "ON_ERROR_STOP"):
			if o.Stdin != nil {
				io.Copy(io.Discard, o.Stdin)
			}
			s.note("restore-into-new")
			if s.failRestore {
				return dockertest.Response{Stderr: `psql:<stdin>:9: ERROR:  invalid command \N`}, true
			}
			return dockertest.Response{Stderr: `psql:<stdin>:3: ERROR:  role "appuser" already exists`}, true
		case strings.Contains(script, "vacuumdb"):
			s.note("analyze")
			return ok("")
		case strings.Contains(script, "pg_dumpall"):
			s.note("dump")
			return ok(strings.Repeat("-- line\n", 400) + "-- PostgreSQL database cluster dump complete\n")
		case strings.Contains(script, "pg_isready"):
			return ok("")
		case strings.Contains(script, "POSTGRES_INITDB_ARGS"):
			return ok("")
		case strings.Contains(script, "POSTGRES_USER"):
			return ok("appuser")
		case strings.Contains(script, "server_version"):
			return ok("16.4")
		case strings.Contains(script, "pg_database"):
			return ok("postgres 1\nother 2\n")
		}
	}
	return dockertest.Response{}, false
}

func (s *pgSim) upgradeDeps() UpgradeDeps {
	reg := registry.Client{AttemptTimeout: 20 * time.Millisecond, Opts: []remote.Option{remote.WithTransport(offlineRT{})}}
	return UpgradeDeps{
		Plan:     PlanDeps{Runner: s.fake, Registry: reg, FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, DirSize: func(string) int64 { return 0 }},
		Snapshot: s.deps(),
		Run:      RunDeps{Runner: s.fake, Sleep: func(time.Duration) {}, StableWindow: time.Millisecond},
	}
}

func (s *pgSim) opts() UpgradeOptions {
	return UpgradeOptions{ScanOptions: ScanOptions{Files: []string{s.file}, Project: "p"}, Service: "db", Target: "17",
		Execute: true, Yes: true, AllowRisk: []string{"pg-major-bump", "notes-unavailable", "unversioned-tag", "major-bump", "image-variant-change"}}
}

// after returns the events that happened after the first one equal to marker.
func (s *sim) after(marker string) []string {
	i := s.index(marker)
	if i < 0 {
		s.t.Fatalf("event %q never happened; log: %v", marker, s.log)
	}
	return s.log[i+1:]
}

// inOrder asserts that wants appear in events in the given order.
func inOrder(t *testing.T, events []string, wants ...string) {
	t.Helper()
	pos := 0
	for _, w := range wants {
		found := false
		for ; pos < len(events); pos++ {
			if events[pos] == w {
				found, pos = true, pos+1
				break
			}
		}
		if !found {
			t.Fatalf("expected %q (in this order: %v) in %v", w, wants, events)
		}
	}
}

func TestPGMajorUpgradeSucceeds(t *testing.T) {
	s := newPGSim(t)
	res, err := Upgrade(t.Context(), s.upgradeDeps(), s.cfg(), s.opts())
	if err != nil {
		t.Fatalf("err=%v res=%+v\nlog: %v", err, res, s.log)
	}
	if res.Outcome != OutcomeSuccess {
		t.Fatalf("outcome=%s failure=%s\nlog: %v", res.Outcome, res.Failure, s.log)
	}

	// After the snapshot: the new volume exists before the files name it, the
	// writers stop before the final dump, the new server starts empty, the dump
	// is restored, statistics refreshed, the container recreated from the
	// user's own files, and only then do the writers return.
	inOrder(t, s.after("check-stop"),
		"volume-create p_pgdata_pg17",
		"pull (image new)",
		"stop app",
		"dump",
		"up db +pgmajor-override.yml [new-volume]",
		"restore-into-new",
		"analyze",
		"up db [new-volume]",
		"up app [new-volume]",
	)

	want := strings.NewReplacer("postgres:16   # the database", "postgres:17   # the database",
		"      - pgdata:/var/lib", "      - pgdata_pg17:/var/lib").Replace(pgCompose) +
		"  pgdata_pg17:\n    external: true\n    name: p_pgdata_pg17\n"
	if got := s.read(); got != want {
		t.Errorf("compose file:\n%s\nwant:\n%s", got, want)
	}

	m, err := snapshot.Load(res.Dir)
	if err != nil {
		t.Fatalf("manifest in %q: %v", res.Dir, err)
	}
	if m.Upgrade == nil || len(m.Upgrade.DataMoves) != 1 {
		t.Fatalf("manifest: %+v", m.Upgrade)
	}
	if mv := m.Upgrade.DataMoves[0]; mv.Old != "p_pgdata" || mv.New != "p_pgdata_pg17" || mv.Service != "db" {
		t.Errorf("move: %+v", mv)
	}
	if strings.Join(m.Upgrade.Stopped, ",") != "app" {
		t.Errorf("stopped writers: %v", m.Upgrade.Stopped)
	}
	if len(m.Find(snapshot.KindPGUpgradeDump, "db")) != 1 || len(m.Find(snapshot.KindPGDump, "db")) != 1 {
		t.Errorf("artifacts: %+v", m.Artifacts)
	}
	if res := snapshot.Verify(res.Dir, m); !res.OK() {
		t.Errorf("the whole snapshot must still verify: %v", res.Problems)
	}
	if !s.running["db"] || !s.running["app"] {
		t.Error("everything runs afterwards")
	}
	if ov, _ := os.ReadFile(filepath.Join(res.Dir, "pgmajor-override.yml")); !strings.Contains(string(ov), "POSTGRES_DB: postgres") {
		t.Errorf("first-start override: %s", ov)
	}
	if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "p_pgdata") || !strings.Contains(string(b), "no longer used") {
		t.Errorf("the report must say the old volume is left behind:\n%s", b)
	}
}

func TestPGMajorUpgradeDryRunShowsTheMigration(t *testing.T) {
	s := newPGSim(t)
	o := s.opts()
	o.Execute = false
	res, err := Upgrade(t.Context(), s.upgradeDeps(), s.cfg(), o)
	if err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("%v %+v", err, res)
	}
	mg := res.Preview.Migration
	if mg == nil || mg.From != "p_pgdata" || mg.To != "p_pgdata_pg17" || strings.Join(mg.Writers, ",") != "app" {
		t.Fatalf("migration: %+v", mg)
	}
	if len(res.Preview.Edits) != 2 || s.read() != pgCompose || s.has("volume-create") || s.has("pull") {
		t.Errorf("a dry run changes nothing: edits=%v log=%v", res.Preview.Edits, s.log)
	}
	if res.Preview.Plan.Path != plan.PathPGDumpRestore {
		t.Errorf("path: %s", res.Preview.Plan.Path)
	}
}

func (s *sim) read() string {
	b, _ := os.ReadFile(s.file)
	return string(b)
}

func (s *sim) has(prefix string) bool {
	for _, e := range s.log {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func TestPGMajorFailedRestoreSwitchesBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*pgSim)
	}{
		{"restore reports errors", func(s *pgSim) { s.failRestore = true }},
		{"the new server does not start", func(s *pgSim) { s.failUpInit = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newPGSim(t)
			tc.set(s)
			res, err := Upgrade(t.Context(), s.upgradeDeps(), s.cfg(), s.opts())
			if !errors.Is(err, errs.ErrRolledBack) || res.Outcome != OutcomeRolledBack {
				t.Fatalf("err=%v outcome=%s failure=%s\nlog: %v", err, res.Outcome, res.Failure, s.log)
			}
			if res.Rollback == nil || res.Rollback.Kind != RollbackSwitchBack {
				t.Fatalf("the old volume was never written: expected a switch back, got %+v", res.Rollback)
			}
			if got := s.read(); got != pgCompose {
				t.Errorf("the compose file must be back byte for byte:\n%s", got)
			}
			// the old database comes back first on the OLD volume, with the old image
			// kept locally and never pulled; the writers it had stopped follow
			inOrder(t, s.after("dump"),
				"docker-stop db",
				"up db +rollback-override.yml [old-volume]",
				"up app +rollback-override.yml [old-volume]",
			)
			if !s.running["db"] || !s.running["app"] {
				t.Errorf("everything runs again: %v", s.running)
			}
			for _, e := range s.log {
				if strings.Contains(e, "volume-rm") || strings.Contains(e, "prune") {
					t.Errorf("nothing may be deleted: %s", e)
				}
			}
			if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "p_pgdata_pg17") {
				t.Errorf("the report names the migrated copy that is kept:\n%s", b)
			}
		})
	}
}

func TestPGMajorRefusesAnExistingTarget(t *testing.T) {
	s := newPGSim(t)
	inner := s.fake.Handler
	s.fake.Handler = func(args []string, o docker.Opts) (dockertest.Response, bool) {
		if args[0] == "volume" && args[1] == "inspect" {
			return ok(`[{"Name":"p_pgdata_pg17"}]`)
		}
		return inner(args, o)
	}
	res, err := Upgrade(t.Context(), s.upgradeDeps(), s.cfg(), s.opts())
	if err == nil || !strings.Contains(err.Error(), "already exists") || res.Executed {
		t.Fatalf("err=%v executed=%v", err, res.Executed)
	}
	if s.read() != pgCompose || s.has("stop") {
		t.Errorf("nothing may change: %v", s.log)
	}
}

func TestPGMajorWithoutAWritableDataMountChangesNothing(t *testing.T) {
	s := newPGSim(t)
	os.WriteFile(s.file, []byte(strings.Replace(pgCompose, "      - pgdata:/var/lib/postgresql/data\n", "", 1)), 0o644)
	inner := s.fake.Handler
	s.fake.Handler = func(args []string, o docker.Opts) (dockertest.Response, bool) {
		if args[0] == "compose" && contains(args, "config") && !contains(args, "-q") {
			return ok(strings.Replace(pgSimJSON, `"volumes":[{"type":"volume","source":"pgdata","target":"/var/lib/postgresql/data"}]`, `"volumes":[]`, 1))
		}
		return inner(args, o)
	}
	res, err := Upgrade(t.Context(), s.upgradeDeps(), s.cfg(), s.opts())
	if err == nil || res.Executed {
		t.Fatalf("err=%v executed=%v", err, res.Executed)
	}
	if s.has("stop") || s.has("pull") {
		t.Errorf("nothing may change: %v", s.log)
	}
}
