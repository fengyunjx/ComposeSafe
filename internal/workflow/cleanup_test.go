package workflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/docker/dockertest"
	"composesafe/internal/snapshot"
)

type stateFixture struct {
	t    *testing.T
	root string
	cfg  *config.Config
}

func newState(t *testing.T) *stateFixture {
	root := filepath.Join(t.TempDir(), "state")
	os.MkdirAll(filepath.Join(root, "cache"), 0o700)
	c := config.Default()
	c.StateDir = root
	return &stateFixture{t: t, root: root, cfg: c}
}

func (f *stateFixture) run(project, id string, mut func(*snapshot.Manifest)) string {
	dir := filepath.Join(f.root, project, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	m := &snapshot.Manifest{Schema: snapshot.SchemaVersion, RunID: id, Created: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
		Project: snapshot.ProjectInfo{Name: project}, Complete: true}
	if mut != nil {
		mut(m)
	}
	if err := m.Save(dir); err != nil {
		f.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "data.bin"), make([]byte, 2000), 0o600)
	return dir
}

func upgradeOf(m *snapshot.Manifest) {
	m.Upgrade = &snapshot.UpgradeInfo{Service: "db", From: "postgres:16", To: "postgres:17"}
}

func TestHistoryDescribesEveryKindOfRun(t *testing.T) {
	f := newState(t)
	f.run("p", "20261101-100000-aaaa", nil)
	f.run("p", "20261102-100000-bbbb", func(m *snapshot.Manifest) {
		upgradeOf(m)
		m.PostUpgrade = &snapshot.PostUpgrade{ToImage: snapshot.ImageInfo{ImageID: "sha256:x"}}
	})
	f.run("p", "20261103-100000-cccc", func(m *snapshot.Manifest) {
		upgradeOf(m)
		m.PostUpgrade = &snapshot.PostUpgrade{}
		m.Rollback = &snapshot.RollbackInfo{Complete: true}
	})
	f.run("p", "20261104-100000-dddd", func(m *snapshot.Manifest) { m.Complete = false })
	f.run("p", "20261105-100000-eeee", func(m *snapshot.Manifest) { upgradeOf(m); m.PostUpgrade = &snapshot.PostUpgrade{} })
	f.run("p", "20261106-100000-ffff", func(m *snapshot.Manifest) { m.Schema = 99 })
	rh := filepath.Join(f.root, "p", "20261107-100000-gggg")
	os.MkdirAll(rh, 0o700)
	b, _ := json.Marshal(rehearsalMeta{Outcome: "failed", Stage: "baseline", Service: "app", From: "x/app:1", To: "x/app:2"})
	os.WriteFile(filepath.Join(rh, "rehearsal.json"), b, 0o600)
	os.WriteFile(filepath.Join(f.root, "p", "lock"), nil, 0o600)                    // not a run
	os.MkdirAll(filepath.Join(f.root, "p", "20261108-notarun"), 0o700)              // no manifest
	os.MkdirAll(filepath.Join(f.root, rhPrefix+"x", "20261109-000000-hhhh"), 0o700) // a copy's own state

	f.cfg.Retention.Snapshots = 3
	h, err := History(f.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range h.Rows {
		got[r.RunID[9:15]+"-"+r.RunID[16:]] = r.Kind + "/" + r.Outcome
	}
	want := map[string]string{
		"100000-aaaa": "snapshot/ok", "100000-bbbb": "upgrade/upgraded", "100000-cccc": "upgrade/rolled-back",
		"100000-dddd": "snapshot/incomplete", "100000-eeee": "upgrade/unfinished", "100000-ffff": "unknown/unreadable",
		"100000-gggg": "rehearsal/failed",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if len(h.Rows) != len(want) {
		t.Errorf("rows: %d, want %d", len(h.Rows), len(want))
	}
	if h.Rows[0].RunID != "20261107-100000-gggg" {
		t.Errorf("newest first, got %s", h.Rows[0].RunID)
	}
	if h.OverRetention != 5 { // snapshots and upgrades; rehearsals and unreadable do not count
		t.Errorf("over retention: %d", h.OverRetention)
	}
	if h.Rows[len(h.Rows)-1].Size < 2000 {
		t.Errorf("size: %d", h.Rows[len(h.Rows)-1].Size)
	}
}

func TestReportOfPrefersTheWrittenReportAndRendersSnapshots(t *testing.T) {
	f := newState(t)
	a := f.run("p", "20261101-100000-aaaa", func(m *snapshot.Manifest) {
		m.Artifacts = []snapshot.Artifact{{Kind: "pg_dump", Service: "db", Path: "db.sql.gz", Size: 42}}
	})
	b := f.run("p", "20261102-100000-bbbb", upgradeOf)
	os.WriteFile(filepath.Join(b, "report.md"), []byte("# the upgrade report\n"), 0o600)

	s, err := ReportOf(f.cfg, nil, "20261102")
	if err != nil || s != "# the upgrade report\n" {
		t.Fatalf("%q %v", s, err)
	}
	s, err = ReportOf(f.cfg, []string{"p"}, "20261101-100000-aaaa")
	if err != nil || !strings.Contains(s, "Snapshot of p") || !strings.Contains(s, "db.sql.gz") || !strings.Contains(s, a) {
		t.Fatalf("snapshot report: %q %v", s, err)
	}
	if s, _ := ReportOf(f.cfg, nil, "latest"); !strings.Contains(s, "upgrade report") {
		t.Errorf("latest: %q", s)
	}
	if _, err := ReportOf(f.cfg, nil, "2026110"); err == nil || !strings.Contains(err.Error(), "matches 2 runs") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := ReportOf(f.cfg, nil, "nope"); err == nil {
		t.Error("unknown id")
	}
}

// recorder answers the few docker calls cleanup makes and remembers deletions.
type delFake struct {
	rm       []string // "volume rm X", "rmi T"
	tags     string
	volumes  string // output of `volume ls`
	mounted  map[string]bool
	existing map[string]bool
	model    string
}

func (d *delFake) handle(args []string, _ docker.Opts) (dockertest.Response, bool) {
	line := strings.Join(args, " ")
	ok := func(s string) (dockertest.Response, bool) { return dockertest.Response{Stdout: s}, true }
	switch {
	case line == "context inspect":
		return ok(`[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`)
	case line == "version --format {{.Server.Version}}":
		return ok("29.0.0")
	case line == "compose version --short":
		return ok("5.0.0")
	case args[0] == "compose" && contains(args, "config"):
		return ok(d.model)
	case line == "system df -v --format json":
		return ok(`{"Volumes":[{"Name":"p_old","Size":"5MB"}]}`)
	case args[0] == "volume" && args[1] == "inspect":
		if d.existing[args[2]] {
			return ok(`[{"Name":"` + args[2] + `"}]`)
		}
		return dockertest.Response{Stdout: "[]", Err: errors.New("exit 1")}, true
	case args[0] == "ps" && contains(args, "-q") && strings.Contains(line, "volume="):
		if d.mounted[strings.TrimPrefix(args[len(args)-1], "volume=")] {
			return ok("cid")
		}
		return ok("")
	case args[0] == "volume" && args[1] == "ls":
		return ok(d.volumes)
	case args[0] == "volume" && args[1] == "rm":
		d.rm = append(d.rm, "volume rm "+args[2])
		return ok("")
	case args[0] == "image" && args[1] == "ls":
		return ok(d.tags)
	case args[0] == "rmi":
		d.rm = append(d.rm, "rmi "+args[1])
		return ok("")
	case args[0] == "ps", args[0] == "network":
		return ok("")
	}
	return dockertest.Response{}, false
}

func (d *delFake) deps(confirm func(CleanupItem) (bool, error)) CleanupDeps {
	return CleanupDeps{Run: RunDeps{Runner: &dockertest.Fake{Handler: d.handle}}, Confirm: confirm}
}

func TestCleanupRunNeedsConfirmationAndRemovesItsPins(t *testing.T) {
	f := newState(t)
	dir := f.run("p", "20261101-100000-aaaa", nil)
	other := f.run("p", "20261102-100000-bbbb", nil)
	d := &delFake{tags: "composesafe-keep/db:20261101-100000-aaaa\ncomposesafe-keep/db:20261102-100000-bbbb\nredis:7\n"}
	o := CleanupOptions{ScanOptions: ScanOptions{Project: "p"}, Target: "20261101-100000-aaaa"}

	// dry run: listed, nothing deleted
	res, err := Cleanup(t.Context(), d.deps(nil), f.cfg, o)
	if err != nil || len(res.Items) != 1 || res.Items[0].Name != "20261101-100000-aaaa" || res.Executed {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Items[0].Warn != "no-rollback" {
		t.Errorf("deleting the snapshot of a good run costs the way back: %+v", res.Items[0])
	}
	o.Execute = true
	if _, err := Cleanup(t.Context(), d.deps(nil), f.cfg, o); err == nil {
		t.Error("without a way to confirm nothing may be deleted")
	}
	// declined
	res, _ = Cleanup(t.Context(), d.deps(func(CleanupItem) (bool, error) { return false, nil }), f.cfg, o)
	if _, err := os.Stat(dir); err != nil || len(res.Declined) != 1 || len(d.rm) != 0 {
		t.Fatalf("declined must keep everything: %+v %v", res, d.rm)
	}
	// confirmed: the directory and only ITS image pin go
	res, err = Cleanup(t.Context(), d.deps(func(CleanupItem) (bool, error) { return true, nil }), f.cfg, o)
	if err != nil || len(res.Removed) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the run directory must be gone")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("another run must be untouched")
	}
	if strings.Join(d.rm, "|") != "rmi composesafe-keep/db:20261101-100000-aaaa" {
		t.Errorf("deleted: %v", d.rm)
	}
}

func TestRemoveRunRefusesPathsOutsideTheStateDirectory(t *testing.T) {
	f := newState(t)
	victim := t.TempDir()
	d := &delFake{}
	for _, it := range []CleanupItem{
		{Kind: ItemSnapshot, Name: filepath.Base(victim), Path: victim},
		{Kind: ItemSnapshot, Name: "p", Path: filepath.Join(f.root, "p")},           // a project, not a run
		{Kind: ItemSnapshot, Name: "x", Path: filepath.Join(f.root, "p", "x", "y")}, // too deep
		{Kind: ItemSnapshot, Name: "..", Path: filepath.Join(f.root, "p", "..")},    // the state root
	} {
		os.MkdirAll(it.Path, 0o700)
		if err := removeRun(t.Context(), RunDeps{Runner: &dockertest.Fake{Handler: d.handle}}, f.root, it); err == nil {
			t.Errorf("must refuse %s", it.Path)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Error("the directory outside the state must survive")
	}
}

const oldVolModel = `{"name":"p","services":{"db":{"image":"postgres:17","volumes":[{"type":"volume","source":"pgdata","target":"/var/lib/postgresql/data"}]}},
 "volumes":{"pgdata":{"name":"p_current"}}}`

func TestCleanupOldVolumesOffersOnlyWhatIsLeftBehind(t *testing.T) {
	f := newState(t)
	mk := func(id string, mut func(*snapshot.Manifest)) { f.run("p", id, mut) }
	// a data rollback left the failed state in p_failed (restored copy is in use)
	mk("20261101-100000-aaaa", func(m *snapshot.Manifest) {
		upgradeOf(m)
		m.Rollback = &snapshot.RollbackInfo{Complete: true, Kind: RollbackData,
			Restored: []snapshot.RestoredMount{{Service: "db", Type: "volume", Old: "p_failed", New: "p_failed_rb_x"}}}
	})
	// a successful major upgrade left the old volume
	mk("20261102-100000-bbbb", func(m *snapshot.Manifest) {
		upgradeOf(m)
		m.PostUpgrade = &snapshot.PostUpgrade{ToImage: snapshot.ImageInfo{ImageID: "sha256:n"}}
		m.Upgrade.DataMoves = []snapshot.RestoredMount{{Service: "db", Type: "volume", Old: "p_old", New: "p_old_pg17"}}
	})
	// a switched-back upgrade left the migrated copy
	mk("20261103-100000-cccc", func(m *snapshot.Manifest) {
		upgradeOf(m)
		m.Rollback = &snapshot.RollbackInfo{Complete: true, Kind: RollbackSwitchBack}
		m.Upgrade.DataMoves = []snapshot.RestoredMount{{Service: "db", Type: "volume", Old: "p_orig", New: "p_copy"}}
	})
	// candidates that must NOT be offered
	mk("20261104-100000-dddd", func(m *snapshot.Manifest) {
		upgradeOf(m)
		m.PostUpgrade = &snapshot.PostUpgrade{ToImage: snapshot.ImageInfo{ImageID: "sha256:n"}}
		m.Upgrade.DataMoves = []snapshot.RestoredMount{{Old: "p_current"}, {Old: "p_busy"}, {Old: "/srv/pgdata"}, {Old: "p_gone"}}
	})
	mk("20261105-100000-eeee", func(m *snapshot.Manifest) { // failed upgrade, rollback not complete: nothing is known to be spare
		upgradeOf(m)
		m.Rollback = &snapshot.RollbackInfo{Complete: false, Restored: []snapshot.RestoredMount{{Old: "p_unsure"}}}
		m.Upgrade.DataMoves = []snapshot.RestoredMount{{Old: "p_unsure2", New: "p_unsure3"}}
	})
	d := &delFake{model: oldVolModel, mounted: map[string]bool{"p_busy": true},
		existing: map[string]bool{"p_failed": true, "p_old": true, "p_copy": true, "p_current": true, "p_busy": true, "p_unsure": true, "p_unsure2": true, "p_unsure3": true}}

	o := CleanupOptions{ScanOptions: ScanOptions{Files: []string{"/x/compose.yml"}, Project: "p"}, Target: "old-volumes"}
	res, err := Cleanup(t.Context(), d.deps(nil), f.cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range res.Items {
		got[it.Name] = it.Why
	}
	want := map[string]string{"p_failed": "failed-state", "p_old": "pre-upgrade", "p_copy": "migrated-copy"}
	if len(got) != 3 || got["p_failed"] != want["p_failed"] || got["p_old"] != want["p_old"] || got["p_copy"] != want["p_copy"] {
		t.Fatalf("items: %v", got)
	}
	skipped := strings.Join(res.Skipped, "\n")
	for _, w := range []string{"p_current", "current compose files use it", "p_busy", "a container still mounts it", "/srv/pgdata", "host directory"} {
		if !strings.Contains(skipped, w) {
			t.Errorf("skipped lacks %q:\n%s", w, skipped)
		}
	}
	for _, it := range res.Items {
		if it.Name == "p_old" && it.Size != 5_000_000 {
			t.Errorf("size of p_old: %d", it.Size)
		}
	}

	// executing: each volume only after its own confirmation
	o.Execute = true
	var asked []string
	_, err = Cleanup(t.Context(), d.deps(func(it CleanupItem) (bool, error) {
		asked = append(asked, it.Name)
		return it.Name != "p_copy", nil
	}), f.cfg, o)
	if err != nil || len(asked) != 3 {
		t.Fatalf("%v asked=%v", err, asked)
	}
	rm := strings.Join(d.rm, "|")
	if !strings.Contains(rm, "volume rm p_failed") || !strings.Contains(rm, "volume rm p_old") || strings.Contains(rm, "p_copy") ||
		strings.Contains(rm, "p_current") || strings.Contains(rm, "p_busy") {
		t.Errorf("removed: %v", d.rm)
	}
}

func TestCleanupOldVolumesNeedsToKnowTheCurrentModel(t *testing.T) {
	f := newState(t)
	f.run("p", "20261101-100000-aaaa", func(m *snapshot.Manifest) {
		upgradeOf(m)
		m.PostUpgrade = &snapshot.PostUpgrade{ToImage: snapshot.ImageInfo{ImageID: "x"}}
		m.Upgrade.DataMoves = []snapshot.RestoredMount{{Old: "p_old"}}
	})
	d := &delFake{existing: map[string]bool{"p_old": true}}
	d.model = "" // compose cannot be read
	inner := d.handle
	fk := &dockertest.Fake{Handler: func(a []string, o docker.Opts) (dockertest.Response, bool) {
		if a[0] == "compose" && contains(a, "config") {
			return dockertest.Response{Err: errors.New("exit 1"), Stderr: "broken compose"}, true
		}
		return inner(a, o)
	}}
	res, err := Cleanup(t.Context(), CleanupDeps{Run: RunDeps{Runner: fk}}, f.cfg,
		CleanupOptions{ScanOptions: ScanOptions{Files: []string{"/x/compose.yml"}, Project: "p"}, Target: "old-volumes", Execute: true})
	if err == nil || len(res.Items) != 0 {
		t.Fatalf("a volume may only be offered when the project's current use is known: %v %+v", err, res)
	}
}

func TestCleanupRehearsalsRemovesOnlyThoseNamed(t *testing.T) {
	f := newState(t)
	name := rhPrefix + "20261106-101010-ab12"
	os.MkdirAll(filepath.Join(f.root, name, "work"), 0o700)
	os.MkdirAll(filepath.Join(f.root, "p"), 0o700) // a real project is not a rehearsal
	d := &delFake{volumes: name + "_pgdata\n" + name + "_data_pg18\ncomposesafe-rh-unrelated-volume\n"}
	// destroyRehearsal asks the daemon for volumes by name and label; the fake answers the same list
	res, err := Cleanup(t.Context(), d.deps(nil), f.cfg, CleanupOptions{Target: "rehearsals"})
	if err != nil || len(res.Items) != 1 || res.Items[0].Name != name {
		t.Fatalf("%v %+v", err, res)
	}
	o := CleanupOptions{Target: "rehearsals", Execute: true}
	if r, err := Cleanup(t.Context(), d.deps(func(CleanupItem) (bool, error) { return true, nil }), f.cfg, o); err != nil {
		t.Fatalf("%v %v", err, r.Failed)
	}
	rm := strings.Join(d.rm, "|")
	if !strings.Contains(rm, "volume rm "+name+"_pgdata") || !strings.Contains(rm, "volume rm "+name+"_data_pg18") {
		t.Errorf("the rehearsal's volumes must go: %v", d.rm)
	}
	if strings.Contains(rm, "unrelated") {
		t.Errorf("a volume that merely starts with the prefix is not the rehearsal's: %v", d.rm)
	}
	if _, err := os.Stat(filepath.Join(f.root, name)); err == nil {
		t.Error("the rehearsal's state must be removed")
	}
	if _, err := os.Stat(filepath.Join(f.root, "p")); err != nil {
		t.Error("a project directory must survive")
	}
}
