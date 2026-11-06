package risk

import (
	"testing"
	"time"

	"composesafe/internal/compose"
	"composesafe/internal/notes"
)

var now = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func svc(name, image string, kind compose.Kind, mounts ...compose.Mount) *compose.Service {
	return &compose.Service{Name: name, Image: image, Ref: compose.ParseRef(image), Kind: kind, Mounts: mounts, HasHealthcheck: true}
}

// in builds a rule input for an upgrade of s to the given tag.
func in(s *compose.Service, to string) Input {
	return Input{
		Svc: s, From: s.Ref, To: compose.ParseRef(compose.WithTag(s.Image, to)),
		Project:     &compose.Project{Services: map[string]*compose.Service{s.Name: s}},
		DigestKnown: true, DataSize: Unknown, BackupFree: Unknown, NotesState: "ok", Now: now,
	}
}

func ids(rs []Risk) map[string]Level {
	m := map[string]Level{}
	for _, r := range rs {
		k := r.ID
		if r.Variant != "" {
			k += "." + r.Variant
		}
		m[k] = r.Level
	}
	return m
}

func want(t *testing.T, rs []Risk, id string, lvl Level) {
	t.Helper()
	got, ok := ids(rs)[id]
	if !ok {
		t.Errorf("missing %s; have %v", id, ids(rs))
	} else if got != lvl {
		t.Errorf("%s level = %v, want %v", id, got, lvl)
	}
}

func wantNot(t *testing.T, rs []Risk, id string) {
	t.Helper()
	if _, ok := ids(rs)[id]; ok {
		t.Errorf("unexpected %s", id)
	}
}

func TestPostgresMajorUpgrade(t *testing.T) {
	s := svc("db", "postgres:14.5-alpine", compose.KindPostgres,
		compose.Mount{Type: "volume", Source: "pgdata", Target: "/var/lib/postgresql/data"})
	rs := Evaluate(in(s, "18.0-alpine"))
	want(t, rs, "pg-major-bump", Critical)
	want(t, rs, "pg-skip-versions", High)
	want(t, rs, "pg18-datadir-layout", Info) // moved automatically
	want(t, rs, "pg-eol-source.soon", Info)  // 14 ends 2026-11-12, six days away
	wantNot(t, rs, "major-bump")             // Postgres has its own rule
	if Blocked(rs) {
		t.Errorf("a plain major upgrade must not be blocked: %v", ids(rs))
	}
	if rs[0].Level != Critical {
		t.Errorf("results must be sorted by severity, first is %v", rs[0].Level)
	}
}

func TestPostgresSameMajorIsQuiet(t *testing.T) {
	s := svc("db", "postgres:16.3-alpine", compose.KindPostgres)
	rs := Evaluate(in(s, "16.4-alpine"))
	for _, id := range []string{"pg-major-bump", "pg-skip-versions", "major-bump", "pg18-datadir-layout", "image-variant-change"} {
		wantNot(t, rs, id)
	}
}

func TestPG18LayoutOnlyWhenCrossingIntoIt(t *testing.T) {
	m := compose.Mount{Type: "volume", Source: "pgdata", Target: "/var/lib/postgresql/data"}
	wantNot(t, Evaluate(in(svc("db", "postgres:18.0", compose.KindPostgres, m), "18.1")), "pg18-datadir-layout") // already on 18
	// anonymous volume cannot be switched
	anon := compose.Mount{Type: "volume", Source: "", Target: "/var/lib/postgresql/data"}
	rs := Evaluate(in(svc("db", "postgres:17", compose.KindPostgres, anon), "18"))
	want(t, rs, "pg18-datadir-layout.anonymous", Block)
	// data already at the new location needs nothing
	ok := compose.Mount{Type: "volume", Source: "v", Target: "/var/lib/postgresql"}
	wantNot(t, Evaluate(in(svc("db", "postgres:17", compose.KindPostgres, ok), "18")), "pg18-datadir-layout")
}

func TestDerivativeMajorIsBlocked(t *testing.T) {
	rs := Evaluate(in(svc("db", "postgis/postgis:15-3.4", compose.KindPostgresDerivative), "16-3.4"))
	want(t, rs, "pg-derivative-major", Block)
	if !Blocked(rs) {
		t.Error("expected Blocked")
	}
	// same major with newer PostGIS is allowed
	wantNot(t, Evaluate(in(svc("db", "postgis/postgis:16-3.3", compose.KindPostgresDerivative), "16-3.4")), "pg-derivative-major")
}

func TestDowngrade(t *testing.T) {
	want(t, Evaluate(in(svc("db", "postgres:16.3", compose.KindPostgres), "15.8")), "downgrade", Block)
	want(t, Evaluate(in(svc("app", "nginx:1.27", compose.KindNone), "1.25")), "downgrade", High)
}

func TestMajorBumpForApps(t *testing.T) {
	want(t, Evaluate(in(svc("app", "gitea/gitea:1.22.0", compose.KindNone), "2.0.0")), "major-bump", High)
	wantNot(t, Evaluate(in(svc("app", "gitea/gitea:1.22.0", compose.KindNone), "1.23.1")), "major-bump")
}

func TestUnversionedTags(t *testing.T) {
	rs := Evaluate(in(svc("app", "nginx:latest", compose.KindNone), "1.27"))
	want(t, rs, "unversioned-tag.current", Medium)
	wantNot(t, rs, "major-bump")
	want(t, Evaluate(in(svc("app", "nginx:1.27", compose.KindNone), "stable")), "unversioned-tag.target", Medium)
	// pinned by digest is fine
	wantNot(t, Evaluate(in(svc("app", "nginx@sha256:abc", compose.KindNone), "1.27")), "unversioned-tag.current")
}

func TestVariantChange(t *testing.T) {
	tests := []struct {
		from, to string
		id       string
		lvl      Level
		none     bool
	}{
		{"16.3-alpine", "16.4-bookworm", "image-variant-change", High, false},
		{"16.3-alpine3.19", "16.4-alpine3.20", "image-variant-change.release", Info, false},
		{"16.3", "16.4-bookworm", "", 0, true}, // default is Debian
		{"16.3-bookworm", "16.4", "", 0, true},
		{"16.3-alpine", "16.4-alpine", "", 0, true},
		{"1.0-slim", "1.1-alpine", "image-variant-change", High, false},
	}
	for _, tc := range tests {
		s := svc("x", "me/img:"+tc.from, compose.KindNone)
		rs := Evaluate(in(s, tc.to))
		if tc.none {
			wantNot(t, rs, "image-variant-change")
			wantNot(t, rs, "image-variant-change.release")
			continue
		}
		want(t, rs, tc.id, tc.lvl)
	}
}

func TestPGEOL(t *testing.T) {
	rs := Evaluate(in(svc("db", "postgres:13.5", compose.KindPostgres), "14.5"))
	want(t, rs, "pg-eol-source.past", Info)
	want(t, rs, "pg-eol-target", Medium) // 14 is six days from the end
	rs = Evaluate(in(svc("db", "postgres:16.3", compose.KindPostgres), "17.2"))
	wantNot(t, rs, "pg-eol-source.past")
	wantNot(t, rs, "pg-eol-target")
	// not Postgres: no EOL rules
	wantNot(t, Evaluate(in(svc("app", "redis:7.0", compose.KindUnknownVolume), "7.2")), "pg-eol-target")
}

func TestKeywordRules(t *testing.T) {
	s := svc("app", "gitea/gitea:1.22.0", compose.KindNone)
	i := in(s, "1.23.0")
	i.Notes = []notes.ReleaseNote{
		{Title: "v1.23.0", URL: "https://x/1", Body: "## BREAKING   \n  * Remove SHA1\nUpgrade is irreversible once the DB migrates."},
		{Title: "v1.23.1", URL: "https://x/2", Body: "### Breaking changes\nThis release introduces a new environment variable APP_X.\nMigration to Version 16 is fine."},
		{Title: "v1.23.2", URL: "https://x/3", Body: "Please read the migration guide."},
		{Title: "v1.23.3", URL: "https://x/4", Body: "Fixes. Also a breaking change."},
	}
	rs := Evaluate(i)
	n := 0
	for _, r := range rs {
		if r.ID == "forward-only-migration" {
			n++
			if r.Level != High || r.Source == "" || r.Evidence == "" {
				t.Errorf("incomplete risk: %+v", r)
			}
		}
	}
	if n != maxHitsPerRule {
		t.Errorf("forward-only hits = %d, want the cap %d", n, maxHitsPerRule)
	}
	want(t, rs, "new-required-env", Medium)

	// a bare "Migration to Version N" heading (every Postgres page) must not trigger
	pg := svc("db", "postgres:16.3", compose.KindPostgres)
	pi := in(pg, "16.4")
	pi.Notes = []notes.ReleaseNote{{Title: "Release 16.4", Body: "E.12.1. Migration to Version 16.4\nA dump/restore is not required for those running 16.X."}}
	wantNot(t, Evaluate(pi), "forward-only-migration")
}

func TestKeywordRulesChinese(t *testing.T) {
	i := in(svc("app", "x/app:1.0", compose.KindNone), "2.0")
	i.Notes = []notes.ReleaseNote{{Title: "2.0", Body: "此版本包含不可回退的数据库升级。"}}
	want(t, Evaluate(i), "forward-only-migration", High)
}

func TestNotesUnavailable(t *testing.T) {
	s := svc("app", "x/app:1.0", compose.KindNone)
	i := in(s, "1.1")
	i.NotesState = "nosource"
	want(t, Evaluate(i), "notes-unavailable.nosource", Medium)
	i.NotesState, i.NotesWarnings = "failed", []string{"rate limited"}
	rs := Evaluate(i)
	want(t, rs, "notes-unavailable.failed", Medium)
	for _, r := range rs {
		if r.ID == "notes-unavailable" && r.Evidence != "rate limited" {
			t.Errorf("evidence = %q", r.Evidence)
		}
	}
	i.NotesState = "skipped"
	wantNot(t, Evaluate(i), "notes-unavailable.failed")
}

func TestNoHealthcheckLooksAtDependencies(t *testing.T) {
	db := &compose.Service{Name: "db", Image: "postgres:16", HasHealthcheck: true}
	app := &compose.Service{Name: "app", Image: "x/app:1", Ref: compose.ParseRef("x/app:1"), DependsOn: []string{"db"}}
	proj := &compose.Project{Services: map[string]*compose.Service{"db": db, "app": app}}
	i := Input{Project: proj, Svc: app, From: app.Ref, To: compose.ParseRef("x/app:2"), DigestKnown: true, DataSize: Unknown, BackupFree: Unknown, NotesState: "ok", Now: now}
	wantNot(t, Evaluate(i), "no-healthcheck") // the dependency has one

	db.HasHealthcheck = false
	want(t, Evaluate(i), "no-healthcheck", Medium)

	i.HasSmoke = true
	wantNot(t, Evaluate(i), "no-healthcheck") // a smoke check is enough

	// dependency cycles must not hang
	db.DependsOn = []string{"app"}
	i.HasSmoke = false
	want(t, Evaluate(i), "no-healthcheck", Medium)
}

func TestDigestUnpinned(t *testing.T) {
	i := in(svc("app", "x/app:1", compose.KindNone), "2")
	i.DigestKnown = false
	want(t, Evaluate(i), "digest-unpinned", Medium)
}

func TestBindMountData(t *testing.T) {
	bind := compose.Mount{Type: "bind", Source: "/srv/mysql", Target: "/var/lib/mysql"}
	want(t, Evaluate(in(svc("db", "mysql:8.0", compose.KindUnknownVolume, bind), "8.4")), "bind-mount-data", Medium)
	// Postgres supports bind mounts: no warning
	pgBind := compose.Mount{Type: "bind", Source: "/srv/pg", Target: "/var/lib/postgresql/data"}
	wantNot(t, Evaluate(in(svc("db", "postgres:16.3", compose.KindPostgres, pgBind), "16.4")), "bind-mount-data")
}

func TestLowDisk(t *testing.T) {
	s := svc("db", "postgres:16.3", compose.KindPostgres)
	i := in(s, "16.4")
	i.DataSize, i.BackupFree = 10_000_000_000, 21_000_000_000 // needs 22 GB
	want(t, Evaluate(i), "low-disk", Block)
	i.BackupFree = 22_000_000_000
	wantNot(t, Evaluate(i), "low-disk")
	i.BackupFree = Unknown
	wantNot(t, Evaluate(i), "low-disk") // unknown never blocks
}

func TestEvaluateDeduplicatesAndSorts(t *testing.T) {
	i := in(svc("app", "x/app:1.0", compose.KindNone), "2.0")
	i.Notes = []notes.ReleaseNote{{Title: "a", URL: "u", Body: "breaking change here"}, {Title: "a", URL: "u", Body: "breaking change here"}}
	rs := Evaluate(i)
	n := 0
	for _, r := range rs {
		if r.ID == "forward-only-migration" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("identical findings must collapse, got %d", n)
	}
	for k := 1; k < len(rs); k++ {
		if rs[k].Level > rs[k-1].Level {
			t.Fatalf("not sorted by severity: %v", rs)
		}
	}
	if Max(rs) != rs[0].Level || Max(nil) != Info {
		t.Error("Max wrong")
	}
}
