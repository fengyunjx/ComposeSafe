package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"composesafe/internal/config"
	"composesafe/internal/report"
	"composesafe/internal/snapshot"
	"composesafe/internal/state"
)

// Kinds of run.
const (
	RunSnapshot  = "snapshot"
	RunUpgrade   = "upgrade"
	RunRehearsal = "rehearsal"
	RunUnknown   = "unknown"
)

// HistoryRow summarises one run directory.
type HistoryRow struct {
	Project string    `json:"project"`
	RunID   string    `json:"run_id"`
	Kind    string    `json:"kind"`
	Time    time.Time `json:"time"`
	Service string    `json:"service,omitempty"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	// Outcome is a code the CLI translates: ok, incomplete, upgraded,
	// rolled-back, rollback-failed, not-applied, unfinished, passed, failed,
	// unreadable.
	Outcome string `json:"outcome"`
	Size    int64  `json:"size"`
	Detail  string `json:"detail,omitempty"`
}

// HistoryResult lists runs, newest first.
type HistoryResult struct {
	Rows []HistoryRow `json:"rows"`
	// OverRetention is set when there are more snapshots than retention.snapshots.
	OverRetention int `json:"over_retention,omitempty"`
}

// rehearsalMeta is written next to a rehearsal's report so that history can
// summarise it (a rehearsal has no manifest).
type rehearsalMeta struct {
	Outcome string    `json:"outcome"`
	Stage   string    `json:"stage,omitempty"`
	Service string    `json:"service"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Time    time.Time `json:"time"`
}

// History lists the runs of the given projects (all projects when none).
// It reads only the state directory.
func History(cfg *config.Config, projects []string) (*HistoryResult, error) {
	root, err := cfg.ResolvedStateDir()
	if err != nil {
		return nil, err
	}
	var dirs []string
	if len(projects) > 0 {
		for _, p := range projects {
			dirs = append(dirs, state.ProjectDir(root, p))
		}
	} else {
		ents, err := os.ReadDir(root)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, e := range ents {
			// cache/ is not a project; rehearsal projects hold only a copy's own state.
			if e.IsDir() && e.Name() != "cache" && !strings.HasPrefix(e.Name(), rhPrefix) {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	res := &HistoryResult{}
	snapshots := 0
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			row, ok := readRun(filepath.Base(d), filepath.Join(d, e.Name()))
			if !ok {
				continue
			}
			if row.Kind == RunSnapshot || row.Kind == RunUpgrade {
				snapshots++
			}
			res.Rows = append(res.Rows, row)
		}
	}
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i].RunID > res.Rows[j].RunID })
	if cfg.Retention.Snapshots > 0 && snapshots > cfg.Retention.Snapshots {
		res.OverRetention = snapshots
	}
	return res, nil
}

const rhPrefix = "composesafe-rh-"

func readRun(project, dir string) (HistoryRow, bool) {
	row := HistoryRow{Project: project, RunID: filepath.Base(dir), Size: dirSize(dir)}
	if b, err := os.ReadFile(filepath.Join(dir, "rehearsal.json")); err == nil {
		var m rehearsalMeta
		if json.Unmarshal(b, &m) == nil {
			row.Kind, row.Outcome, row.Time = RunRehearsal, m.Outcome, m.Time
			row.Service, row.From, row.To, row.Detail = m.Service, m.From, m.To, m.Stage
			return row, true
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		return row, false // not a run
	}
	m, err := snapshot.Load(dir)
	if err != nil {
		row.Kind, row.Outcome, row.Detail = RunUnknown, "unreadable", err.Error()
		return row, true
	}
	row.Time = m.Created
	switch u := m.Upgrade; {
	case !m.Complete:
		row.Outcome = "incomplete"
	case u == nil:
		row.Outcome = "ok"
	}
	if u := m.Upgrade; u != nil {
		row.Kind, row.Service, row.From, row.To = RunUpgrade, u.Service, u.From, u.To
		switch {
		case m.Rollback != nil && m.Rollback.Complete:
			row.Outcome = "rolled-back"
		case m.Rollback != nil:
			row.Outcome = "rollback-failed"
		case m.PostUpgrade == nil:
			row.Outcome = "not-applied"
		case m.PostUpgrade.ToImage.ImageID == "":
			row.Outcome = "unfinished"
		case row.Outcome == "":
			row.Outcome = "upgraded"
		}
	} else {
		row.Kind = RunSnapshot
	}
	return row, true
}

// ---- report ----------------------------------------------------------------------

// ReportOf returns the Markdown report of a run. Upgrade and rehearsal runs
// wrote theirs when they finished; a plain snapshot gets one rendered from
// its manifest.
func ReportOf(cfg *config.Config, projects []string, runID string) (string, error) {
	row, dir, err := findRun(cfg, projects, runID)
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(filepath.Join(dir, "report.md")); err == nil {
		return string(b), nil
	}
	m, err := snapshot.Load(dir)
	if err != nil {
		return "", fmt.Errorf("run %s has no report and no readable manifest: %w", row.RunID, err)
	}
	return report.Snapshot(reportT(cfg), m, dir), nil
}

// findRun resolves a run id (a unique prefix, or "latest") to its directory.
func findRun(cfg *config.Config, projects []string, runID string) (HistoryRow, string, error) {
	h, err := History(cfg, projects)
	if err != nil {
		return HistoryRow{}, "", err
	}
	root, _ := cfg.ResolvedStateDir()
	dirOf := func(r HistoryRow) string { return filepath.Join(state.ProjectDir(root, r.Project), r.RunID) }
	if runID == "latest" && len(h.Rows) > 0 {
		return h.Rows[0], dirOf(h.Rows[0]), nil
	}
	var hit []HistoryRow
	for _, r := range h.Rows {
		if r.RunID == runID {
			return r, dirOf(r), nil
		}
		if runID != "" && strings.HasPrefix(r.RunID, runID) {
			hit = append(hit, r)
		}
	}
	switch len(hit) {
	case 0:
		return HistoryRow{}, "", fmt.Errorf("no run matches %q (see `composesafe history`)", runID)
	case 1:
		return hit[0], dirOf(hit[0]), nil
	}
	return HistoryRow{}, "", fmt.Errorf("%q matches %d runs; give more of the id", runID, len(hit))
}
