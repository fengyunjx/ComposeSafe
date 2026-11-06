//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"composesafe/internal/docker"
	"composesafe/internal/errs"
	"composesafe/internal/workflow"
)

// Postgres 16 -> 18 crosses the layout change of 18 (data under
// /var/lib/postgresql/18/docker) and skips 17: the hardest realistic case.
const (
	pgFrom = "postgres:16-alpine"
	pgTo   = "18-alpine"
)

var pgMajorAllow = []string{"pg-major-bump", "pg-skip-versions", "notes-unavailable", "major-bump", "unversioned-tag", "image-variant-change"}

// dbToyCompose is the toy stack with a Postgres service the app depends on.
func dbToyCompose(dataMount, volumes string) string {
	c := strings.Replace(toyCompose, "services:\n", `services:
  db:
    image: `+pgFrom+`
    environment:
      POSTGRES_PASSWORD: pw
      POSTGRES_USER: appuser
    volumes:
      - `+dataMount+`
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U appuser"]
      interval: 2s
      retries: 30
`, 1)
	c = strings.Replace(c, "    image: cs-toy:1   # pinned on purpose\n", "    image: cs-toy:1   # pinned on purpose\n    depends_on:\n      db: {condition: service_healthy}\n", 1)
	return strings.Replace(c, "volumes:\n  data:\n", "volumes:\n  data:\n"+volumes, 1)
}

func (y *toy) waitDB() {
	y.t.Helper()
	for i := 0; i < 90; i++ {
		if exec.Command("docker", "compose", "-p", y.project, "-f", y.file, "exec", "-T", "db", "pg_isready", "-q", "-h", "127.0.0.1", "-U", "appuser").Run() == nil {
			return
		}
		time.Sleep(time.Second)
	}
	y.t.Fatal("db not ready")
}

func (y *toy) psql(db, sql string) string {
	y.t.Helper()
	return y.compose("exec", "-T", "db", "psql", "-U", "appuser", "-d", db, "-tAc", sql)
}

func (y *toy) upgradeDB(r docker.Runner) (*workflow.UpgradeResult, error) {
	return workflow.Upgrade(context.Background(), y.deps(r), y.cfg, workflow.UpgradeOptions{
		ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project},
		Service:     "db", Target: pgTo, Execute: true, Yes: true, AllowRisk: pgMajorAllow, SkipRestoreCheck: true,
	})
}

// seed fills the old cluster: two databases, a role, an index, and a late
// write that happens after the snapshot (while the upgrade is in flight).
func (y *toy) seedAndLateWrite(t *testing.T) *recorder {
	y.waitDB()
	y.psql("postgres", "create table orders(id int primary key, note text)")
	y.psql("postgres", "insert into orders select g, 'row '||g from generate_series(1,1000) g")
	y.psql("postgres", "create database shop")
	y.psql("shop", "create table items(id serial primary key, name text)")
	y.psql("shop", "insert into items(name) values ('a'),('b')")
	y.psql("postgres", "create role reporter login password 'x'")
	return &recorder{Runner: y.runner, pullAt: " pull db", onPull: func() {
		// after the snapshot, before the writers stop: must survive the migration
		if out, err := exec.Command("docker", "compose", "-p", y.project, "-f", y.file, "exec", "-T", "db",
			"psql", "-U", "appuser", "-d", "postgres", "-c", "insert into orders values (1001, 'late')").CombinedOutput(); err != nil {
			t.Errorf("late write: %v %s", err, out)
		}
	}}
}

// treeHash is a hash over the names and contents of every file in a volume or
// host directory, read through a container with the source mounted read-only.
func treeHash(t *testing.T, source string) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "--network", "none", "-v", source+":/d:ro", helperImage(), "sh", "-c",
		"cd /d && find . -type f -exec sha256sum {} + | sort -k2 | sha256sum").CombinedOutput()
	if err != nil {
		t.Fatalf("hash %s: %v %s", source, err, out)
	}
	return strings.TrimSpace(string(out))
}

// assertOldDataStaysUntouched: once the upgrade is over nothing may write to
// the old location, however busy the new database is. The new server is made
// to write and restart; the old tree must hash the same before and after.
func (y *toy) assertOldDataStaysUntouched(t *testing.T, source string) {
	t.Helper()
	before := treeHash(t, source)
	y.psql("postgres", "insert into orders values (2001, 'after the upgrade')")
	y.psql("postgres", "checkpoint")
	y.compose("restart", "db")
	y.waitDB()
	if after := treeHash(t, source); after != before {
		t.Errorf("the old data at %s changed while the new database ran", source)
	}
}

func (y *toy) dbMounts() string {
	y.t.Helper()
	id := y.compose("ps", "-q", "db")
	out, err := exec.Command("docker", "inspect", "--format", `{{range .Mounts}}{{.Name}}{{.Source}} {{end}}`, id).Output()
	if err != nil {
		y.t.Fatal(err)
	}
	return string(out)
}

func TestPGMajorUpgradeOnANamedVolume(t *testing.T) {
	y := newToy(t, dbToyCompose("pgdata:/var/lib/postgresql/data", "  pgdata:\n"))
	original := y.composeFile()
	rec := y.seedAndLateWrite(t)

	res, err := y.upgradeDB(rec)
	if err != nil || res.Outcome != workflow.OutcomeSuccess {
		t.Fatalf("err=%v outcome=%s failure=%s", err, res.Outcome, res.Failure)
	}
	y.waitDB()

	if v := y.psql("postgres", "show server_version"); !strings.HasPrefix(v, "18") {
		t.Errorf("server version %s", v)
	}
	if n := y.psql("postgres", "select count(*) from orders"); n != "1001" {
		t.Errorf("orders: %s, want 1001 (1000 + the write made during the upgrade)", n)
	}
	if n := y.psql("shop", "select count(*) from items"); n != "2" {
		t.Errorf("second database: %s items", n)
	}
	if n := y.psql("postgres", "select count(*) from pg_roles where rolname = 'reporter'"); n != "1" {
		t.Errorf("role reporter: %s", n)
	}
	if n := y.psql("postgres", "select count(*) from pg_stat_user_tables where relname = 'orders' and last_analyze is not null"); n != "1" {
		t.Errorf("statistics were not refreshed (%s)", n)
	}

	// the compose file: image, mount point and volume changed, nothing else
	f := y.composeFile()
	vol := y.volume("pgdata")
	for _, want := range []string{"image: postgres:18-alpine", "pgdata_pg18:/var/lib/postgresql\n", "name: " + vol + "_pg18"} {
		if !strings.Contains(f, want) {
			t.Errorf("compose file lacks %q:\n%s", want, f)
		}
	}
	if strings.Contains(f, "/var/lib/postgresql/data") {
		t.Errorf("the old mount point must be gone:\n%s", f)
	}

	// the old volume is exactly as it was; the new server only ever saw the new one
	if got := y.volumeFile(vol, "PG_VERSION"); got != "16" {
		t.Errorf("old volume PG_VERSION = %q, want 16", got)
	}
	if got := y.volumeFile(vol+"_pg18", "18/docker/PG_VERSION"); got != "18" {
		t.Errorf("new volume 18/docker/PG_VERSION = %q", got)
	}
	if m := y.dbMounts(); strings.Contains(m, vol+" ") || !strings.Contains(m, vol+"_pg18") {
		t.Errorf("db container mounts: %s", m)
	}
	y.assertOldDataStaysUntouched(t, vol)
	if y.get("/version") != "v1" {
		t.Error("the app (a writer that was stopped meanwhile) must be serving again")
	}
	if original == f {
		t.Error("compose file unchanged")
	}
	if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "no longer used") {
		t.Errorf("report must name the old volume:\n%s", b)
	}
}

func TestPGMajorFailureSwitchesBackToTheUntouchedVolume(t *testing.T) {
	y := newToy(t, dbToyCompose("pgdata:/var/lib/postgresql/data", "  pgdata:\n"))
	original := y.composeFile()
	rec := y.seedAndLateWrite(t)

	// the migration fails at the last step before the comparison
	res, err := y.upgradeDB(failingRunner{Runner: rec, when: "vacuumdb"})
	if !errors.Is(err, errs.ErrRolledBack) || res.Outcome != workflow.OutcomeRolledBack {
		t.Fatalf("err=%v outcome=%s failure=%s", err, res.Outcome, res.Failure)
	}
	if res.Rollback == nil || res.Rollback.Kind != workflow.RollbackSwitchBack {
		t.Fatalf("rollback: %+v", res.Rollback)
	}
	if y.composeFile() != original {
		t.Errorf("compose file must be back byte for byte:\n%s", y.composeFile())
	}
	y.waitDB()
	if v := y.psql("postgres", "show server_version"); !strings.HasPrefix(v, "16") {
		t.Errorf("server version %s, want the old 16", v)
	}
	// nothing was lost: the old volume was never written by the new version,
	// so even the write made after the snapshot is there
	if n := y.psql("postgres", "select count(*) from orders"); n != "1001" {
		t.Errorf("orders: %s, want 1001", n)
	}
	if m := y.dbMounts(); !strings.Contains(m, y.volume("pgdata")) || strings.Contains(m, "_pg18") {
		t.Errorf("db container mounts: %s", m)
	}
	if got := y.volumeFile(y.volume("pgdata")+"_pg18", "18/docker/PG_VERSION"); got != "18" {
		t.Errorf("the migrated copy must be kept: %q", got)
	}
	if y.get("/version") != "v1" {
		t.Error("the app must be serving again")
	}
	b, _ := os.ReadFile(res.ReportPath)
	if !strings.Contains(string(b), y.volume("pgdata")+"_pg18") {
		t.Errorf("the report must name the kept copy:\n%s", b)
	}
}

func TestPGMajorUpgradeOnABindDirectory(t *testing.T) {
	y := newToy(t, dbToyCompose("./pgdata:/var/lib/postgresql/data", ""))
	rec := y.seedAndLateWrite(t)

	res, err := y.upgradeDB(rec)
	if err != nil || res.Outcome != workflow.OutcomeSuccess {
		t.Fatalf("err=%v outcome=%s failure=%s", err, res.Outcome, res.Failure)
	}
	y.waitDB()
	if n := y.psql("postgres", "select count(*) from orders"); n != "1001" {
		t.Errorf("orders: %s", n)
	}
	f := y.composeFile()
	if !strings.Contains(f, "./pgdata_pg18:/var/lib/postgresql") {
		t.Errorf("compose file:\n%s", f)
	}
	// read the directories through a container: their owner is the postgres user
	read := func(dir, name string) string {
		out, err := exec.Command("docker", "run", "--rm", "--network", "none", "-v", dir+":/d:ro", helperImage(), "cat", "/d/"+name).CombinedOutput()
		if err != nil {
			return "<missing: " + strings.TrimSpace(string(out)) + ">"
		}
		return strings.TrimSpace(string(out))
	}
	if got := read(filepath.Join(y.dir, "pgdata"), "PG_VERSION"); got != "16" {
		t.Errorf("old directory PG_VERSION = %q", got)
	}
	if got := read(filepath.Join(y.dir, "pgdata_pg18"), "18/docker/PG_VERSION"); got != "18" {
		t.Errorf("new directory = %q", got)
	}
	y.assertOldDataStaysUntouched(t, filepath.Join(y.dir, "pgdata"))
}

// After a successful major upgrade the old volume is left behind. `cleanup`
// offers exactly that volume, deletes it only after confirmation, and the
// running database and its (new) volume are not touched.
func TestCleanupOldVolumeAfterAPostgresMajorUpgrade(t *testing.T) {
	y := newToy(t, dbToyCompose("pgdata:/var/lib/postgresql/data", "  pgdata:\n"))
	rec := y.seedAndLateWrite(t)
	res, err := y.upgradeDB(rec)
	if err != nil || res.Outcome != workflow.OutcomeSuccess {
		t.Fatalf("err=%v outcome=%s", err, res.Outcome)
	}
	y.waitDB()
	oldVol, newVol := y.volume("pgdata"), y.volume("pgdata")+"_pg18"

	deps := workflow.CleanupDeps{Run: workflow.RunDeps{Runner: y.runner, Now: time.Now}}
	o := workflow.CleanupOptions{ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project}, Target: "old-volumes"}
	list, err := workflow.Cleanup(context.Background(), deps, y.cfg, o)
	if err != nil || len(list.Items) != 1 || list.Items[0].Name != oldVol || list.Items[0].Why != "pre-upgrade" {
		t.Fatalf("only the volume the upgrade left behind may be offered: %v %+v", err, list)
	}
	if list.Items[0].Size <= 0 {
		t.Errorf("size: %d", list.Items[0].Size)
	}

	// declined: still there
	o.Execute = true
	deps.Confirm = func(workflow.CleanupItem) (bool, error) { return false, nil }
	if _, err := workflow.Cleanup(context.Background(), deps, y.cfg, o); err != nil {
		t.Fatal(err)
	}
	if got := y.volumeFile(oldVol, "PG_VERSION"); got != "16" {
		t.Fatalf("declining must keep the volume: %q", got)
	}

	// confirmed: the old one goes, the live one stays
	deps.Confirm = func(it workflow.CleanupItem) (bool, error) { return it.Name == oldVol, nil }
	r, err := workflow.Cleanup(context.Background(), deps, y.cfg, o)
	if err != nil || len(r.Removed) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	if out, _ := exec.Command("docker", "volume", "inspect", oldVol).CombinedOutput(); !strings.Contains(strings.ToLower(string(out)), "no such volume") {
		t.Errorf("the old volume must be gone: %s", out)
	}
	if got := y.volumeFile(newVol, "18/docker/PG_VERSION"); got != "18" {
		t.Errorf("the live volume must be untouched: %q", got)
	}
	if n := y.psql("postgres", "select count(*) from orders"); n != "1001" {
		t.Errorf("the running database lost data: %s", n)
	}

	// nothing is left to offer, and the snapshot of the run can be deleted by its id
	again, _ := workflow.Cleanup(context.Background(), deps, y.cfg, workflow.CleanupOptions{ScanOptions: o.ScanOptions, Target: "old-volumes"})
	if len(again.Items) != 0 {
		t.Errorf("offered again: %+v", again.Items)
	}
	deps.Confirm = func(workflow.CleanupItem) (bool, error) { return true, nil }
	if r, err := workflow.Cleanup(context.Background(), deps, y.cfg, workflow.CleanupOptions{ScanOptions: o.ScanOptions, Target: res.RunID, Execute: true}); err != nil || len(r.Removed) != 1 {
		t.Fatalf("delete the run: %v %+v", err, r)
	}
	if _, err := os.Stat(res.Dir); err == nil {
		t.Error("the run directory must be gone")
	}
	tags, _ := exec.Command("docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "composesafe-keep/*").Output()
	if strings.Contains(string(tags), ":"+res.RunID) {
		t.Errorf("image pins of the run remain: %s", tags)
	}
	h, err := workflow.History(y.cfg, []string{y.project})
	if err != nil || len(h.Rows) != 0 {
		t.Errorf("history after deleting the only run: %v %+v", err, h)
	}
}
