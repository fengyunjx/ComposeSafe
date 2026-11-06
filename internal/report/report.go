// Package report renders plans as text for the terminal and Markdown for
// report.md. All wording comes from package i18n.
package report

import (
	"fmt"
	"strings"

	"composesafe/internal/plan"
	"composesafe/internal/risk"
)

// T is a translator from package i18n.
type T = func(key string, args ...any) string

// RiskText renders a risk's message in the translator's language. The key is
// risk.<ID> or risk.<ID>.<Variant>.
func RiskText(t T, r risk.Risk) string {
	key := "risk." + r.ID
	if r.Variant != "" {
		key += "." + r.Variant
	}
	args := make([]any, len(r.Args))
	for i, a := range r.Args {
		args[i] = a
	}
	return t(key, args...)
}

// Bytes formats a byte count in decimal units; negative means unknown.
func Bytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTP"[exp])
}

// Assessment renders the Markdown report of a plan (PRD section 11,
// assessment part).
func Assessment(t T, p *plan.Plan) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("# %s", t("report.title", p.Project, p.CreatedAt.Format("2006-01-02 15:04")))
	w("")
	w("**%s**", t("report.conclusion"))
	w("")
	w("## %s", t("report.summary"))
	w("")
	w("- %s: `%s`", t("report.service"), p.Service)
	w("- %s: `%s` → `%s`", t("report.image"), p.From.Raw, p.To.Raw)
	w("- %s", t("plan.path."+string(p.Path)))
	w("- %s", t("plan.disk", Bytes(p.DataSize), Bytes(p.BackupFree)))
	w("")

	w("## %s", t("plan.risks"))
	w("")
	if len(p.Risks) == 0 {
		w("%s", t("plan.risks.none"))
	} else {
		w("| %s | ID | %s | %s |", t("report.level"), t("report.finding"), t("report.evidence"))
		w("|---|---|---|---|")
		for _, r := range p.Risks {
			ev := ""
			if r.Evidence != "" {
				ev = "`" + strings.ReplaceAll(r.Evidence, "|", "\\|") + "`"
				if r.Source != "" {
					ev += fmt.Sprintf(" ([%s](%s))", t("report.source"), r.Source)
				}
			}
			w("| %s | `%s` | %s | %s |", r.Level, r.ID, strings.ReplaceAll(RiskText(t, r), "|", "\\|"), ev)
		}
	}
	w("")

	w("## %s", t("plan.notes"))
	w("")
	if len(p.Notes) == 0 {
		w("%s", t("plan.notes.none"))
	} else {
		w("%s", t("plan.source", p.NotesSource.String(), len(p.Notes)))
		w("")
		for _, n := range p.Notes {
			line := fmt.Sprintf("- [%s](%s)", n.Title, n.URL)
			if n.Truncated {
				line += " " + t("report.truncated")
			}
			w("%s", line)
		}
	}
	for _, x := range p.NotesWarnings {
		w("")
		w("> %s", x)
	}
	w("")
	w("## %s", t("report.llm"))
	w("")
	w("%s", t("report.llm.off"))

	if len(p.Warnings) > 0 {
		w("")
		w("## %s", t("plan.warnings"))
		w("")
		for _, x := range p.Warnings {
			w("- %s", x)
		}
	}
	return b.String()
}

// Verdict is the one-line outcome of a plan.
func Verdict(t T, p *plan.Plan) string {
	switch {
	case p.Blocked:
		n := 0
		for _, r := range p.Risks {
			if r.Level == risk.Block {
				n++
			}
		}
		return t("plan.result.blocked", n)
	case risk.Max(p.Risks) >= risk.High:
		return t("plan.result.confirm")
	}
	return t("plan.result.ok")
}
