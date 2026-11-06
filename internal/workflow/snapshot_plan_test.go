package workflow

import (
	"errors"
	"strings"
	"testing"

	"composesafe/internal/compose"
	"composesafe/internal/config"
)

func svcOf(image string, kind compose.Kind, deps []string, mounts ...compose.Mount) *compose.Service {
	return &compose.Service{Image: image, Ref: compose.ParseRef(image), Kind: kind, DependsOn: deps, Mounts: mounts}
}

func vol(name, target string) compose.Mount {
	return compose.Mount{Type: "volume", Source: name, VolumeName: "p_" + name, Target: target}
}

// stack: web -> app -> db (postgres), app also has a volume of unknown purpose,
// cache is redis, and worker depends on app.
func testProject() *compose.Project {
	p := &compose.Project{Name: "p", Services: map[string]*compose.Service{
		"db":     svcOf("postgres:16", compose.KindPostgres, nil, vol("pgdata", "/var/lib/postgresql/data")),
		"app":    svcOf("x/app:1", compose.KindNone, []string{"db"}, vol("uploads", "/srv/uploads")),
		"web":    svcOf("nginx:1", compose.KindNone, []string{"app"}),
		"worker": svcOf("x/worker:1", compose.KindNone, []string{"app"}),
		"cache":  svcOf("redis:7", compose.KindUnknownVolume, nil, vol("redis", "/data")),
	}}
	p.Services["app"].MaybeStateful = true
	for n, s := range p.Services {
		s.Name = n
	}
	return p
}

var sizes = map[string]int64{"p_pgdata": 1_000_000, "p_uploads": 500_000, "p_redis": 10_000}

func snapPlan(t *testing.T, o SnapshotOptions, cfg *config.Config) (*SnapshotPlan, error) {
	t.Helper()
	if cfg == nil {
		cfg = config.Default()
	}
	return planSnapshot(testProject(), cfg, o, sizes, func(string) int64 { return -1 }, 1<<40)
}

func TestDependents(t *testing.T) {
	p := testProject()
	if got := strings.Join(dependents(p, []string{"db"}), ","); got != "app,web,worker" {
		t.Errorf("db -> %s", got)
	}
	if got := strings.Join(dependents(p, []string{"app"}), ","); got != "web,worker" {
		t.Errorf("app -> %s", got)
	}
	if got := dependents(p, []string{"web"}); len(got) != 0 {
		t.Errorf("web -> %v", got)
	}
	// cycles terminate
	p.Services["db"].DependsOn = []string{"web"}
	if got := dependents(p, []string{"db"}); len(got) == 0 {
		t.Error("cycle: dependents should still be found")
	}
}

func TestPlanSnapshotOfAnAppProtectsItsDatabase(t *testing.T) {
	pl, err := snapPlan(t, SnapshotOptions{Services: []string{"web"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// web -> app (maybe-stateful but not named) -> db: only db holds known data
	if len(pl.Protected) != 1 || pl.Protected[0].Name != "db" || !pl.Protected[0].Dump || !pl.Protected[0].Archive {
		t.Fatalf("protected: %+v", pl.Protected)
	}
	if strings.Join(pl.ColdStop, ",") != "db" || strings.Join(pl.Quiesce, ",") != "app,web,worker" {
		t.Errorf("stop db=%v writers=%v", pl.ColdStop, pl.Quiesce)
	}
	if strings.Join(pl.KeepImages, ",") != "db,web" {
		t.Errorf("keep = %v", pl.KeepImages)
	}
	if pl.DataSize != 1_000_000 || pl.BackupNeed != 2_200_000 || pl.DockerNeed != 1_000_000 || pl.Mode != "logical+volume" {
		t.Errorf("sizes: %+v", pl)
	}
}

func TestPlanSnapshotNamedMaybeStatefulServiceIsIncluded(t *testing.T) {
	pl, err := snapPlan(t, SnapshotOptions{Services: []string{"app"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, p := range pl.Protected {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "app,db" || pl.Mode != "mixed" || pl.DataSize != 1_500_000 {
		t.Errorf("protected=%v mode=%s data=%d", names, pl.Mode, pl.DataSize)
	}
	// app is both archived and a data service, so it is not "a writer" of itself
	if strings.Join(pl.ColdStop, ",") != "app,db" || strings.Join(pl.Quiesce, ",") != "web,worker" {
		t.Errorf("cold=%v writers=%v", pl.ColdStop, pl.Quiesce)
	}
}

func TestPlanSnapshotLogicalOnly(t *testing.T) {
	pl, err := snapPlan(t, SnapshotOptions{Services: []string{"db"}, LogicalOnly: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Mode != "logical" || len(pl.ColdStop) != 0 || len(pl.Quiesce) != 0 || pl.BackupNeed != 1_100_000 {
		t.Errorf("a pure logical dump stops nothing: %+v", pl)
	}
	if !pl.Protected[0].RestoreCheck {
		t.Error("restore check stays on")
	}

	// non-Postgres data cannot be dumped: skipped with a loud warning
	pl, err = snapPlan(t, SnapshotOptions{Services: []string{"app"}, LogicalOnly: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Protected) != 1 || pl.Protected[0].Name != "db" || len(pl.Warnings) != 1 || !strings.Contains(pl.Warnings[0], "NOT backed up") {
		t.Errorf("protected=%+v warnings=%v", pl.Protected, pl.Warnings)
	}
	// ... and when nothing is left, say so
	if _, err := snapPlan(t, SnapshotOptions{Services: []string{"cache"}, LogicalOnly: true}, nil); !errors.Is(err, ErrNothingToSnapshot) {
		t.Errorf("err = %v", err)
	}
}

func TestPlanSnapshotRestoreCheckSwitches(t *testing.T) {
	pl, _ := snapPlan(t, SnapshotOptions{Services: []string{"db"}, SkipRestoreCheck: true}, nil)
	if pl.Protected[0].RestoreCheck || pl.DockerNeed != 0 {
		t.Errorf("flag ignored: %+v", pl)
	}
	off := false
	cfg := config.Default()
	cfg.Services = map[string]*config.Service{"db": {Snapshot: config.Snapshot{RestoreCheck: &off}}}
	pl, _ = snapPlan(t, SnapshotOptions{Services: []string{"db"}}, cfg)
	if pl.Protected[0].RestoreCheck {
		t.Error("config restore_check: false ignored")
	}
}

func TestPlanSnapshotErrors(t *testing.T) {
	if _, err := snapPlan(t, SnapshotOptions{}, nil); err == nil {
		t.Error("no services")
	}
	if _, err := snapPlan(t, SnapshotOptions{Services: []string{"nope"}}, nil); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("unknown: %v", err)
	}
	if _, err := snapPlan(t, SnapshotOptions{Services: []string{"web"}}, nil); err != nil {
		t.Errorf("web depends on a database: %v", err)
	}
	p := testProject()
	delete(p.Services, "db")
	delete(p.Services, "cache")
	p.Services["app"].DependsOn = nil
	p.Services["app"].MaybeStateful = false
	if _, err := planSnapshot(p, config.Default(), SnapshotOptions{Services: []string{"web"}}, sizes, nil, 0); !errors.Is(err, ErrNothingToSnapshot) {
		t.Errorf("stateless stack: %v", err)
	}
}

func TestPlanSnapshotQuiesceFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Services = map[string]*config.Service{"db": {Snapshot: config.Snapshot{Quiesce: []string{"cache"}}}}
	pl, err := snapPlan(t, SnapshotOptions{Services: []string{"db"}}, cfg)
	if err != nil || !strings.Contains(strings.Join(pl.Quiesce, ","), "cache") {
		t.Errorf("%v %v", pl.Quiesce, err)
	}
	cfg.Services["db"].Snapshot.Quiesce = []string{"ghost"}
	if _, err := snapPlan(t, SnapshotOptions{Services: []string{"db"}}, cfg); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("unknown writer: %v", err)
	}
}

func TestPlanSnapshotVolumeProblems(t *testing.T) {
	p := testProject()
	p.Services["db"].Mounts = []compose.Mount{{Type: "volume", Target: "/var/lib/postgresql/data"}} // anonymous
	if _, err := planSnapshot(p, config.Default(), SnapshotOptions{Services: []string{"db"}}, sizes, nil, 0); err == nil ||
		!strings.Contains(err.Error(), "no data mount") {
		t.Errorf("anonymous only: %v", err)
	}

	p = testProject()
	p.Services["db"].Mounts = []compose.Mount{{Type: "bind", Source: "/srv/pg", Target: "/var/lib/postgresql/data"}}
	if _, err := planSnapshot(p, config.Default(), SnapshotOptions{Services: []string{"db"}}, sizes, func(string) int64 { return -1 }, 0); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing bind dir: %v", err)
	}
	pl, err := planSnapshot(p, config.Default(), SnapshotOptions{Services: []string{"db"}}, sizes, func(string) int64 { return 4_000_000 }, 0)
	if err != nil || pl.DataSize != 4_000_000 || pl.Protected[0].Volumes[0].Source != "/srv/pg" {
		t.Errorf("bind mount: %+v %v", pl, err)
	}

	p = testProject()
	p.Services["db"].Mounts[0].External = true
	pl, _ = planSnapshot(p, config.Default(), SnapshotOptions{Services: []string{"db"}}, sizes, nil, 0)
	if len(pl.Warnings) != 1 || !strings.Contains(pl.Warnings[0], "external") {
		t.Errorf("warnings = %v", pl.Warnings)
	}

	// an unknown volume size makes the totals unknown, never silently small
	pl, _ = planSnapshot(testProject(), config.Default(), SnapshotOptions{Services: []string{"db"}}, map[string]int64{}, nil, 0)
	if pl.DataSize != -1 || pl.BackupNeed != -1 {
		t.Errorf("unknown size: %+v", pl)
	}
}

func TestNames(t *testing.T) {
	if got := keepTag("My_App.Web", "20261106-101055-df9e"); got != "composesafe-keep/my_app.web:20261106-101055-df9e" {
		t.Errorf("keepTag = %q", got)
	}
	if got := keepTag("db", "r"); got != "composesafe-keep/db:r" {
		t.Errorf("keepTag = %q", got)
	}
	if got := archiveName(VolumePlan{Type: "volume", Name: "proj_pg/data"}); got != "proj_pg_data.tgz" {
		t.Errorf("volume archive = %q", got)
	}
	a := archiveName(VolumePlan{Type: "bind", Source: "/srv/pg"})
	b := archiveName(VolumePlan{Type: "bind", Source: "/other/pg"})
	if !strings.HasPrefix(a, "bind-pg-") || a == b {
		t.Errorf("bind archives must differ by full path: %q %q", a, b)
	}
	if sanitizeFile("../..") != "data" {
		t.Error("sanitizeFile must never return an empty or dot name")
	}
}
