package report

import (
	"fmt"
	"strings"

	"composesafe/internal/snapshot"
)

// Snapshot renders the report of a plain snapshot from its manifest: what was
// saved, where, and how it was verified.
func Snapshot(t T, m *snapshot.Manifest, dir string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# %s", t("snapr.title", m.Project.Name, m.Created.Format("2006-01-02 15:04")))
	w("")
	w("- %s: `%s`", t("up.run"), m.RunID)
	w("- %s", t("snap.location", "`"+dir+"`"))
	w("- %s: %s", t("snapr.mode"), m.Mode)
	state := t("snapr.complete")
	if !m.Complete {
		state = t("snapr.incomplete")
	}
	w("- %s", state)
	w("- %s: checksums=%v dump-trailer=%v restore-check=%v", t("snapr.verified"), m.Verified.Checksums, m.Verified.DumpTrailer, m.Verified.RestoreCheck)
	if len(m.Quiesced) > 0 {
		w("- %s: %s", t("snapr.stopped"), strings.Join(m.Quiesced, ", "))
	}
	w("")
	w("## %s", t("snapr.artifacts"))
	w("")
	w("| kind | service | file | size |")
	w("|---|---|---|---|")
	for _, a := range m.Artifacts {
		w("| %s | %s | `%s` | %d |", a.Kind, a.Service, a.Path, a.Size)
	}
	if len(m.Warnings) > 0 {
		w("")
		w("## %s", t("snapr.warnings"))
		w("")
		for _, x := range m.Warnings {
			w("- %s", x)
		}
	}
	return b.String()
}
