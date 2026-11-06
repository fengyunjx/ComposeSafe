package report

import (
	"strings"
	"testing"
	"time"

	"composesafe/internal/i18n"
	"composesafe/internal/risk"
	"composesafe/internal/runlog"
	"composesafe/internal/smoke"
	"composesafe/internal/snapshot"
)

func manifest() *snapshot.Manifest {
	return &snapshot.Manifest{
		RunID: "20261106-101055-df9e", Created: time.Date(2026, 11, 6, 10, 10, 55, 0, time.UTC),
		Project: snapshot.ProjectInfo{Name: "myapp"},
		Services: []snapshot.Service{
			{Name: "db", PGUser: "appuser", Image: snapshot.ImageInfo{Ref: "postgres:16", KeepTag: "composesafe-keep/db:r1"}},
			{Name: "app", Image: snapshot.ImageInfo{Ref: "x/app:1", KeepTag: "composesafe-keep/app:r1"}},
		},
		Artifacts: []snapshot.Artifact{
			{Kind: snapshot.KindCompose, Path: "config/compose.yml", Source: "/srv/myapp/compose.yml"},
			{Kind: snapshot.KindEnv, Path: "config/.env", Source: "/srv/myapp/.env"},
			{Kind: snapshot.KindVolume, Service: "db", Volume: "myapp_pgdata", Path: "myapp_pgdata.tgz"},
			{Kind: snapshot.KindVolume, Service: "db", Volume: "/srv/bind/data", Path: "bind-data-1234.tgz"},
			{Kind: snapshot.KindPGDump, Service: "db", Path: "db.sql.gz"},
		},
	}
}

func TestRecoveryGuideIsSelfContained(t *testing.T) {
	g := Recovery(manifest(), "/state/myapp/r1")
	for _, want := range []string{
		"docker compose -p myapp stop db app",
		`cp "/state/myapp/r1/config/compose.yml" "/srv/myapp/compose.yml"`,
		`cp "/state/myapp/r1/config/.env" "/srv/myapp/.env"`,
		"docker volume create myapp_pgdata_restored",
		"tar xzf /backup/myapp_pgdata.tgz -C /data",
		`mkdir "/srv/bind/data_restored"`,
		"gunzip -c \"/state/myapp/r1/db.sql.gz\" | docker compose -p myapp exec -T db psql -U appuser -d postgres",
		"docker tag composesafe-keep/db:r1 postgres:16",
		"docker compose -p myapp up -d",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("guide lacks %q:\n%s", want, g)
		}
	}
	// it must never tell anyone to delete or overwrite existing volumes
	for _, bad := range []string{"volume rm", "rm -rf", " down"} {
		if strings.Contains(g, bad) {
			t.Errorf("the recovery guide must not contain %q", bad)
		}
	}
}

func TestStepTable(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	ev := func(step, status string, sec int) runlog.Event {
		return runlog.Event{Step: step, Status: status, TS: t0.Add(time.Duration(sec) * time.Second)}
	}
	rows := stepTable([]runlog.Event{
		ev("snapshot.dump", runlog.Start, 0), ev("snapshot.dump", runlog.OK, 3),
		ev("upgrade.up", runlog.Start, 4), ev("upgrade.up", runlog.Fail, 10),
		ev("rollback.stop", runlog.Start, 11),
	})
	if len(rows) != 3 || rows[0].dur != "3s" || rows[1].status != "FAILED" || rows[2].status != "INTERRUPTED" {
		t.Errorf("rows: %+v", rows)
	}
}

func TestUpgradeReportPerOutcome(t *testing.T) {
	base := UpgradeData{Project: "myapp", Service: "app", From: "x/app:1", To: "x/app:2", RunID: "r1", Dir: "/state/r1",
		Created: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), Manifest: manifest(),
		Risks: []risk.Risk{{ID: "major-bump", Level: risk.High, Args: []string{"1", "2"}}}}
	en := i18n.T("en")

	d := base
	d.Outcome = "success"
	d.Smoke = []smoke.Result{{Name: "health", Type: "http", OK: true}}
	md := Upgrade(en, d)
	for _, want := range []string{"# Upgrade report · myapp · 2026-11-06 10:00", "Result: the upgrade succeeded", "`x/app:1` → `x/app:2`", "| HIGH | `major-bump` |", "| health (http) | ok |"} {
		if !strings.Contains(md, want) {
			t.Errorf("success report lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "## Rollback") || strings.Contains(md, "Manual recovery") {
		t.Error("no rollback section on success")
	}

	d = base
	d.Outcome, d.Failure = "rolled-back", "smoke check \"health\" failed: answered 500"
	d.RollbackKind = "data"
	d.Restored = []snapshot.RestoredMount{{Service: "app", Old: "myapp_data", New: "myapp_data_rb_r1"}}
	d.DataLossFrom, d.DataLossTo = time.Date(2026, 11, 6, 10, 0, 5, 0, time.UTC), time.Date(2026, 11, 6, 10, 2, 0, 0, time.UTC)
	md = Upgrade(en, d)
	for _, want := range []string{"FAILED and was rolled back", "> smoke check", "Data rollback", "data restored into myapp_data_rb_r1", "failed state stays untouched in myapp_data",
		"Data written between 2026-11-06 10:00:05 (the snapshot) and 2026-11-06 10:02:00 (the rollback) is NOT in the restored data.",
		"## Cleanup (nothing was deleted)", "myapp_data holds the failed state"} {
		if !strings.Contains(md, want) {
			t.Errorf("rollback report lacks %q:\n%s", want, md)
		}
	}

	d = base
	d.Outcome, d.Recovery = "rollback-failed", "docker compose -p myapp up -d"
	md = Upgrade(en, d)
	if !strings.Contains(md, "manual recovery is needed") || !strings.Contains(md, "## Manual recovery") || !strings.Contains(md, "```\ndocker compose -p myapp up -d\n```") {
		t.Errorf("rollback-failed report:\n%s", md)
	}

	zh := Upgrade(i18n.T("zh"), func() UpgradeData { x := base; x.Outcome = "reverted"; return x }())
	if !strings.Contains(zh, "升级报告") || !strings.Contains(zh, "文件已还原") {
		t.Errorf("zh:\n%s", zh)
	}
}
