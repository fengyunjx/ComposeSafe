package report

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"composesafe/internal/compose"
	"composesafe/internal/risk"
	"composesafe/internal/runlog"
	"composesafe/internal/smoke"
	"composesafe/internal/snapshot"
)

// UpgradeData is everything the report of an upgrade run shows.
type UpgradeData struct {
	Project, Service, From, To string
	Outcome, Failure           string
	RunID                      string
	Created                    time.Time
	Risks                      []risk.Risk
	Events                     []runlog.Event
	Smoke                      []smoke.Result
	Manifest                   *snapshot.Manifest
	Dir                        string

	RollbackKind             string
	Restored                 []snapshot.RestoredMount
	DataLossFrom, DataLossTo time.Time
	Recovery                 string
	// Moves: a Postgres major upgrade restored the data into a new location
	// and left the old one untouched.
	Moves []snapshot.RestoredMount

	// Rehearsal is set when the report is of a rehearsal; UpgradeOutcome is
	// then the outcome of the upgrade run on the copy.
	Rehearsal      *RehearsalData
	UpgradeOutcome string
}

// RehearsalData is what a rehearsal report adds.
type RehearsalData struct {
	Project string
	Stage   string // where it failed; empty when it passed
	Kept    bool
	Egress  bool
	File    string
	Notes   []compose.RehearsalNote
}

// Upgrade renders the Markdown report of an upgrade (PRD section 11).
func Upgrade(t T, d UpgradeData) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	if d.Rehearsal != nil {
		w("# %s", t("rh.title", d.Project, d.Created.Format("2006-01-02 15:04")))
		w("")
		w("**%s: %s**", t("up.result"), t("rh.outcome."+d.Outcome))
	} else {
		w("# %s", t("up.title", d.Project, d.Created.Format("2006-01-02 15:04")))
		w("")
		w("**%s: %s**", t("up.result"), t("up.outcome."+d.Outcome))
	}
	if d.Failure != "" {
		w("")
		w("> %s", strings.ReplaceAll(d.Failure, "\n", "\n> "))
	}
	w("")
	w("## %s", t("report.summary"))
	w("")
	w("- %s: `%s`", t("report.service"), d.Service)
	w("- %s: `%s` → `%s`", t("report.image"), d.From, d.To)
	w("- %s: `%s`", t("up.run"), d.RunID)
	w("- %s", t("snap.location", "`"+d.Dir+"`"))
	w("")

	if rh := d.Rehearsal; rh != nil {
		w("## %s", t("rh.section"))
		w("")
		w("- %s", t("rh.project", rh.Project))
		if rh.Stage != "" {
			w("- %s", t("rh.stage."+rh.Stage))
		}
		if d.UpgradeOutcome != "" {
			w("- %s", t("rh.upgrade", t("up.outcome."+d.UpgradeOutcome)))
		}
		if rh.Egress {
			w("- %s", t("rh.egress.on"))
		} else {
			w("- %s", t("rh.egress.off"))
		}
		for _, n := range rh.Notes {
			w("- %s", t("rh.note."+n.Code, n.Service, n.Arg))
		}
		if rh.Kept {
			w("- %s", t("rh.kept", rh.Project, rh.File))
		}
		w("")
		w("> %s", t("rh.disclaimer"))
		w("")
	}

	if len(d.Risks) > 0 {
		w("## %s", t("plan.risks"))
		w("")
		w("| %s | ID | %s |", t("report.level"), t("report.finding"))
		w("|---|---|---|")
		for _, r := range d.Risks {
			w("| %s | `%s` | %s |", r.Level, r.ID, strings.ReplaceAll(RiskText(t, r), "|", "\\|"))
		}
		w("")
	}

	if steps := stepTable(d.Events); len(steps) > 0 {
		w("## %s", t("up.steps"))
		w("")
		w("| %s | %s | %s |", t("up.step"), t("up.status"), t("up.time"))
		w("|---|---|---|")
		for _, s := range steps {
			w("| `%s` | %s | %s |", s.name, s.status, s.dur)
		}
		w("")
	}

	w("## %s", t("up.verify"))
	w("")
	if len(d.Smoke) == 0 {
		w("%s", t("up.smoke.none"))
	} else {
		w("| %s | %s | %s |", "check", t("up.status"), t("report.evidence"))
		w("|---|---|---|")
		for _, s := range d.Smoke {
			st := "ok"
			if !s.OK {
				st = "FAILED"
			}
			w("| %s (%s) | %s | %s |", s.Name, s.Type, st, strings.ReplaceAll(s.Detail, "|", "\\|"))
		}
	}
	w("")

	if d.RollbackKind != "" {
		w("## %s", t("up.rollback"))
		w("")
		w("- %s", t("up.rollback.kind."+d.RollbackKind))
		for _, r := range d.Restored {
			w("- %s", t("up.restored", r.Service, r.New, r.Old))
		}
		if d.RollbackKind == "data" && !d.DataLossFrom.IsZero() {
			w("")
			w("**%s**", t("up.dataloss", d.DataLossFrom.Format("2006-01-02 15:04:05"), d.DataLossTo.Format("2006-01-02 15:04:05")))
		}
		w("")
	}

	if d.Recovery != "" {
		w("## %s", t("up.recovery"))
		w("")
		w("```")
		w("%s", strings.TrimRight(d.Recovery, "\n"))
		w("```")
		w("")
	}

	if d.Rehearsal != nil {
		return b.String() // the copy and its volumes are gone (or kept on request)
	}
	if d.RollbackKind == "switch-back" {
		for _, m := range d.Moves {
			w("- %s", t("up.switchback", m.Service, m.Old, m.New))
		}
		w("")
	}

	if len(d.Restored) > 0 || len(d.Moves) > 0 {
		w("## %s", t("up.cleanup"))
		w("")
		for _, r := range d.Restored {
			w("- %s", t("up.cleanup.old", r.Old))
		}
		for _, m := range d.Moves {
			switch {
			case d.RollbackKind == "switch-back":
				w("- %s", t("up.cleanup.migrated", m.New))
			case d.Outcome == "success":
				w("- %s", t("up.cleanup.preupgrade", m.Old))
			}
		}
	}
	return b.String()
}

type stepRow struct{ name, status, dur string }

// stepTable pairs each step's start with its end event.
func stepTable(events []runlog.Event) []stepRow {
	started := map[string]time.Time{}
	var rows []stepRow
	for _, e := range events {
		switch e.Status {
		case runlog.Start:
			started[e.Step] = e.TS
		case runlog.OK, runlog.Fail, runlog.Skip:
			dur := ""
			if st, ok := started[e.Step]; ok {
				dur = e.TS.Sub(st).Round(10 * time.Millisecond).String()
			}
			status := e.Status
			if e.Status == runlog.Fail {
				status = "FAILED"
			}
			rows = append(rows, stepRow{e.Step, status, dur})
			delete(started, e.Step)
		}
	}
	if step := runlog.Interrupted(events); step != "" {
		rows = append(rows, stepRow{step, "INTERRUPTED", ""})
	}
	return rows
}

// Recovery returns copy-and-paste commands that restore a snapshot by hand.
// It is shown when the automatic rollback failed, and must be usable without
// composesafe: only Docker and the snapshot directory are needed.
func Recovery(m *snapshot.Manifest, dir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Snapshot: %s\n", dir)
	fmt.Fprintf(&b, "# Taken:    %s\n\n", m.Created.Format(time.RFC3339))

	b.WriteString("# 1. Stop the affected services (adjust the names to your project)\n")
	var names []string
	for _, s := range m.Services {
		names = append(names, s.Name)
	}
	fmt.Fprintf(&b, "docker compose -p %s stop %s\n\n", m.Project.Name, strings.Join(names, " "))

	if m.Upgrade != nil && len(m.Upgrade.DataMoves) > 0 {
		b.WriteString("# NOTE: this was a Postgres major upgrade. The original data was never written to by the new version:\n")
		for _, mv := range m.Upgrade.DataMoves {
			fmt.Fprintf(&b, "#   %s is intact (the migrated copy is %s). Putting the compose files and the old image back (steps 2 and 4) is enough;\n", mv.Old, mv.New)
		}
		b.WriteString("#   skip step 3 unless you want the pre-upgrade snapshot instead.\n\n")
	}

	b.WriteString("# 2. Put the compose files back\n")
	for _, a := range m.Artifacts {
		if (a.Kind == snapshot.KindCompose || a.Kind == snapshot.KindEnv) && a.Source != "" {
			fmt.Fprintf(&b, "cp %q %q\n", filepath.Join(dir, a.Path), a.Source)
		}
	}
	b.WriteString("\n# 3. Restore the data into NEW volumes (never into the existing ones)\n")
	for _, a := range m.Artifacts {
		if a.Kind != snapshot.KindVolume {
			continue
		}
		dest := a.Volume + "_restored"
		if strings.HasPrefix(a.Volume, "/") {
			fmt.Fprintf(&b, "mkdir %q && docker run --rm -v %q:/data -v %q:/backup:ro alpine tar xzf /backup/%s -C /data\n", dest, dest, dir, a.Path)
		} else {
			fmt.Fprintf(&b, "docker volume create %s && docker run --rm -v %s:/data -v %q:/backup:ro alpine tar xzf /backup/%s -C /data\n", dest, dest, dir, a.Path)
		}
	}
	for _, a := range m.Artifacts {
		if a.Kind != snapshot.KindPGDump {
			continue
		}
		user := "postgres"
		for _, s := range m.Services {
			if s.Name == a.Service && s.PGUser != "" {
				user = s.PGUser
			}
		}
		fmt.Fprintf(&b, "# or, from the dump, into a freshly initialised %s:\n", a.Service)
		fmt.Fprintf(&b, "gunzip -c %q | docker compose -p %s exec -T %s psql -U %s -d postgres\n", filepath.Join(dir, a.Path), m.Project.Name, a.Service, user)
	}
	b.WriteString("\n# 4. Point the compose file at the restored volumes (or rename them), then start\n")
	for _, s := range m.Services {
		if s.Image.KeepTag != "" {
			fmt.Fprintf(&b, "docker tag %s %s   # the old image of %s\n", s.Image.KeepTag, s.Image.Ref, s.Name)
		}
	}
	fmt.Fprintf(&b, "docker compose -p %s up -d\n", m.Project.Name)
	return b.String()
}
