package report

import (
	"strings"
	"testing"
	"time"

	"composesafe/internal/compose"
	"composesafe/internal/i18n"
	"composesafe/internal/notes"
	"composesafe/internal/plan"
	"composesafe/internal/risk"
)

// Every risk the rules can emit must be translatable in both languages;
// otherwise the user would see a raw key.
func TestEveryRiskHasATranslation(t *testing.T) {
	keys := []string{
		"unversioned-tag.current", "unversioned-tag.target", "downgrade", "major-bump", "pg-major-bump",
		"pg-derivative-major", "pg-skip-versions", "pg18-datadir-layout", "pg18-datadir-layout.anonymous",
		"image-variant-change", "image-variant-change.release", "pg-eol-source.past", "pg-eol-source.soon",
		"pg-eol-target", "forward-only-migration", "new-required-env", "notes-unavailable.nosource",
		"notes-unavailable.failed", "no-healthcheck", "digest-unpinned", "bind-mount-data", "low-disk",
	}
	for _, lang := range []string{"en", "zh"} {
		tr := i18n.T(lang)
		for _, k := range keys {
			id, variant, _ := strings.Cut(k, ".")
			r := risk.Risk{ID: id, Variant: variant, Args: []string{"a", "b", "c"}}
			// Args beyond the template's verbs show up as %!(EXTRA ...); we only
			// care that the key resolved, i.e. the text is not the key itself.
			if got := RiskText(tr, r); got == "risk."+k {
				t.Errorf("%s: no translation for risk.%s", lang, k)
			}
		}
	}
}

func TestBytes(t *testing.T) {
	tests := map[int64]string{-1: "?", 0: "0 B", 999: "999 B", 1500: "1.5 kB", 1_500_000_000: "1.5 GB"}
	for in, want := range tests {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func samplePlan() *plan.Plan {
	return &plan.Plan{
		Project: "myapp", Service: "db", Kind: compose.KindPostgres, Path: plan.PathPGDumpRestore,
		From: compose.ParseRef("postgres:14.5"), To: compose.ParseRef("postgres:16.4"),
		CreatedAt: time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC),
		DataSize:  1_500_000_000, BackupFree: -1,
		Risks: []risk.Risk{
			{ID: "pg-major-bump", Level: risk.Critical, Args: []string{"14", "16"}, Evidence: "14 -> 16"},
			{ID: "forward-only-migration", Level: risk.High, Args: []string{"Release 16"}, Evidence: "a | b", Source: "https://x/y"},
		},
		NotesSource: notes.Source{Kind: "postgres"},
		Notes:       []notes.ReleaseNote{{Title: "Release 16", URL: "https://x/16", Truncated: true}},
		Warnings:    []string{"something minor"},
	}
}

func TestAssessmentMarkdown(t *testing.T) {
	md := Assessment(i18n.T("en"), samplePlan())
	for _, want := range []string{
		"# Upgrade assessment · myapp · 2026-11-06 09:30",
		"`postgres:14.5` → `postgres:16.4`",
		"| CRITICAL | `pg-major-bump` |",
		"[source](https://x/y)",
		`a \| b`, // pipes in evidence must not break the table
		"[Release 16](https://x/16) (truncated)",
		"Nothing was changed",
		"## LLM summary",
		"- something minor",
		"free space for backups: ?",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report missing %q:\n%s", want, md)
		}
	}
	if zh := Assessment(i18n.T("zh"), samplePlan()); !strings.Contains(zh, "仅为评估") || !strings.Contains(zh, "升级评估") {
		t.Errorf("zh report:\n%s", zh)
	}
}

func TestAssessmentWithoutRisksOrNotes(t *testing.T) {
	p := samplePlan()
	p.Risks, p.Notes, p.Warnings = nil, nil, nil
	md := Assessment(i18n.T("en"), p)
	if !strings.Contains(md, "No risks found.") || !strings.Contains(md, "No release notes were retrieved") || strings.Contains(md, "## Warnings") {
		t.Errorf("report:\n%s", md)
	}
}

func TestVerdict(t *testing.T) {
	tr := i18n.T("en")
	p := samplePlan()
	if v := Verdict(tr, p); !strings.Contains(v, "HIGH/CRITICAL") {
		t.Errorf("confirm verdict: %s", v)
	}
	p.Risks = []risk.Risk{{ID: "x", Level: risk.Medium}}
	if v := Verdict(tr, p); v != "Result: no blockers" {
		t.Errorf("ok verdict: %s", v)
	}
	p.Risks = []risk.Risk{{ID: "a", Level: risk.Block}, {ID: "b", Level: risk.Block}}
	p.Blocked = true
	if v := Verdict(tr, p); !strings.Contains(v, "BLOCKED by 2") {
		t.Errorf("blocked verdict: %s", v)
	}
}
