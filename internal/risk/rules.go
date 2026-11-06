package risk

import (
	"fmt"
	"strings"
	"time"

	"composesafe/internal/compose"
	"composesafe/internal/version"
)

// ---- version rules ---------------------------------------------------------

func bothVersions(in Input) (from, to *version.Version, ok bool) {
	return in.From.Version, in.To.Version, in.From.Version != nil && in.To.Version != nil
}

func unversionedTag(in Input) []Risk {
	var rs []Risk
	if in.From.Version == nil && in.From.Digest == "" {
		rs = append(rs, Risk{ID: "unversioned-tag", Level: Medium, Variant: "current", Args: []string{in.From.Tag}})
	}
	if in.To.Version == nil {
		rs = append(rs, Risk{ID: "unversioned-tag", Level: Medium, Variant: "target", Args: []string{in.To.Tag}})
	}
	return rs
}

func downgrade(in Input) []Risk {
	from, to, ok := bothVersions(in)
	if !ok || version.Compare(*to, *from) >= 0 {
		return nil
	}
	lvl := High
	if isPG(in.Svc.Kind) {
		lvl = Block // Postgres data directories cannot be opened by an older server
	}
	return []Risk{{ID: "downgrade", Level: lvl, Args: []string{in.To.Tag, in.From.Tag}}}
}

func majorBump(in Input) []Risk {
	from, to, ok := bothVersions(in)
	if !ok || isPG(in.Svc.Kind) || to.Major <= from.Major {
		return nil
	}
	return []Risk{{ID: "major-bump", Level: High, Args: []string{from.Raw, to.Raw}}}
}

func pgMajorBump(in Input) []Risk {
	from, to, ok := bothVersions(in)
	if !ok || in.Svc.Kind != compose.KindPostgres || to.Major <= from.Major {
		return nil
	}
	return []Risk{{ID: "pg-major-bump", Level: Critical, Args: []string{fmt.Sprint(from.Major), fmt.Sprint(to.Major)},
		Evidence: fmt.Sprintf("%d -> %d", from.Major, to.Major)}}
}

func pgDerivativeMajor(in Input) []Risk {
	from, to, ok := bothVersions(in)
	if !ok || in.Svc.Kind != compose.KindPostgresDerivative || to.Major == from.Major {
		return nil
	}
	return []Risk{{ID: "pg-derivative-major", Level: Block, Args: []string{in.From.Repo},
		Evidence: fmt.Sprintf("%d -> %d", from.Major, to.Major)}}
}

func pgSkipVersions(in Input) []Risk {
	from, to, ok := bothVersions(in)
	if !ok || !isPG(in.Svc.Kind) || to.Major-from.Major <= 1 {
		return nil
	}
	return []Risk{{ID: "pg-skip-versions", Level: High, Args: []string{fmt.Sprint(to.Major - from.Major - 1)},
		Evidence: fmt.Sprintf("%d -> %d", from.Major, to.Major)}}
}

const pgLegacyData = "/var/lib/postgresql/data"

// pg18DataDir: images from Postgres 18 keep data in
// /var/lib/postgresql/18/docker and declare VOLUME /var/lib/postgresql
// (verified against postgres:18-alpine). A mount at the old path would hide
// that layout, so the major-upgrade path moves the mount. Whether the compose
// file can be rewritten is only known when the edit is attempted (M5), which
// then escalates to BLOCK; here only the anonymous-volume case is certain.
func pg18DataDir(in Input) []Risk {
	from, to, ok := bothVersions(in)
	if !ok || in.Svc.Kind != compose.KindPostgres || to.Major < 18 || from.Major >= 18 {
		return nil
	}
	for _, m := range in.Svc.Mounts {
		if m.Target != pgLegacyData {
			continue
		}
		if m.Type == "volume" && m.Source == "" {
			return []Risk{{ID: "pg18-datadir-layout", Level: Block, Variant: "anonymous", Args: []string{pgLegacyData}}}
		}
		return []Risk{{ID: "pg18-datadir-layout", Level: Info, Args: []string{pgLegacyData},
			Evidence: pgLegacyData + " -> /var/lib/postgresql"}}
	}
	return nil
}

func variantChange(in Input) []Risk {
	if in.From.Tag == "" || in.To.Tag == "" || in.From.Version == nil || in.To.Version == nil {
		return nil
	}
	a, b := in.From.Variant, in.To.Variant
	if a == b {
		return nil
	}
	// The default variant of the images we handle is Debian: default <->
	// bookworm is not a change.
	if (a == "" && debianNames[b]) || (b == "" && debianNames[a]) {
		return nil
	}
	fa, fb := variantFamily(a), variantFamily(b)
	if fa == fb { // alpine3.19 -> alpine3.20: same distro, newer release
		return []Risk{{ID: "image-variant-change", Level: Info, Variant: "release", Args: []string{variantName(a), variantName(b)}}}
	}
	return []Risk{{ID: "image-variant-change", Level: High, Args: []string{variantName(a), variantName(b)},
		Evidence: variantName(a) + " -> " + variantName(b)}}
}

var debianNames = map[string]bool{"": true, "slim": true, "bookworm": true, "bullseye": true, "buster": true,
	"trixie": true, "slim-bookworm": true, "slim-bullseye": true, "slim-trixie": true}

// variantFamily reduces a variant to its base distribution so that a bump
// within one distro is not mistaken for a switch of C library. An empty
// variant is the image's default, which for the images we handle is Debian.
func variantFamily(v string) string {
	if debianNames[v] {
		return "debian"
	}
	return strings.ToLower(strings.TrimRight(v, "0123456789."))
}

func variantName(v string) string {
	if v == "" {
		return "default"
	}
	return v
}

// ---- Postgres lifecycle ----------------------------------------------------

func pgEOL(in Input) []Risk {
	if !isPG(in.Svc.Kind) {
		return nil
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	var rs []Risk
	if from := in.From.Version; from != nil {
		if d, ok := pgEOLDate(from.Major); ok {
			switch {
			case now.After(d):
				rs = append(rs, Risk{ID: "pg-eol-source", Level: Info, Variant: "past", Args: []string{fmt.Sprint(from.Major), date(d)}})
			case d.Sub(now) <= 90*24*time.Hour:
				rs = append(rs, Risk{ID: "pg-eol-source", Level: Info, Variant: "soon", Args: []string{fmt.Sprint(from.Major), date(d)}})
			}
		}
	}
	if to := in.To.Version; to != nil {
		if d, ok := pgEOLDate(to.Major); ok && (now.After(d) || d.Sub(now) <= 180*24*time.Hour) {
			rs = append(rs, Risk{ID: "pg-eol-target", Level: Medium, Args: []string{fmt.Sprint(to.Major), date(d)}})
		}
	}
	return rs
}

func date(t time.Time) string { return t.Format("2006-01-02") }

// ---- release-note rules ----------------------------------------------------

// forwardOnly are phrases that signal a migration that cannot be undone or an
// incompatible change. They are deliberately specific: a bare "migration"
// appears in every Postgres release page ("Migration to Version N").
var forwardOnly = []string{
	"irreversible", "cannot downgrade", "can't downgrade", "cannot be downgraded", "no way to downgrade",
	"downgrade is not supported", "downgrades are not supported", "one-way", "cannot be rolled back",
	"backward incompatible", "backwards incompatible", "not backward compatible", "not backwards compatible",
	"breaking change", "# breaking", "database migration", "schema migration", "migration is required",
	"requires a migration", "migration guide", "upgrade guide", "upgrading guide",
	"不可回退", "不可逆", "破坏性", "不向下兼容", "不向后兼容",
}

var newEnv = []string{
	"new environment variable", "new required environment", "environment variable is now required",
	"now requires the environment", "must set the environment", "required environment variable",
	"新增环境变量", "新增必填环境变量",
}

const maxHitsPerRule = 3

func forwardOnlyKeywords(in Input) []Risk {
	return keywordRisks(in, forwardOnly, Risk{ID: "forward-only-migration", Level: High})
}

func newEnvKeywords(in Input) []Risk {
	return keywordRisks(in, newEnv, Risk{ID: "new-required-env", Level: Medium})
}

func keywordRisks(in Input, words []string, tmpl Risk) []Risk {
	var rs []Risk
	for _, n := range in.Notes {
		for _, line := range strings.Split(n.Body, "\n") {
			low := strings.ToLower(line)
			for _, w := range words {
				if !strings.Contains(low, w) {
					continue
				}
				r := tmpl
				r.Args = []string{n.Title}
				r.Evidence = excerpt(line)
				r.Source = n.URL
				rs = append(rs, r)
				if len(rs) >= maxHitsPerRule {
					return rs
				}
				break
			}
		}
	}
	return rs
}

func notesUnavailable(in Input) []Risk {
	switch in.NotesState {
	case "nosource":
		return []Risk{{ID: "notes-unavailable", Level: Medium, Variant: "nosource", Args: []string{in.Svc.Name}}}
	case "failed":
		return []Risk{{ID: "notes-unavailable", Level: Medium, Variant: "failed", Evidence: strings.Join(in.NotesWarnings, "; ")}}
	}
	return nil
}

// ---- verification and safety -----------------------------------------------

func noHealthcheck(in Input) []Risk {
	if in.HasSmoke || in.Project == nil {
		return nil
	}
	seen := map[string]bool{}
	var walk func(name string) bool // true when some service on the path has a healthcheck
	walk = func(name string) bool {
		s := in.Project.Services[name]
		if s == nil || seen[name] {
			return false
		}
		seen[name] = true
		if s.HasHealthcheck {
			return true
		}
		for _, d := range s.DependsOn {
			if walk(d) {
				return true
			}
		}
		return false
	}
	if walk(in.Svc.Name) {
		return nil
	}
	return []Risk{{ID: "no-healthcheck", Level: Medium, Args: []string{in.Svc.Name}}}
}

func digestUnpinned(in Input) []Risk {
	if in.DigestKnown {
		return nil
	}
	return []Risk{{ID: "digest-unpinned", Level: Medium, Args: []string{in.From.Raw}}}
}

// bindMountData: bind-mounted data is fully supported for Postgres (the
// archive helper mounts the host path). Other databases get a cold copy only.
func bindMountData(in Input) []Risk {
	if in.Svc.Kind != compose.KindUnknownVolume {
		return nil
	}
	for _, m := range in.Svc.Mounts {
		if m.Type == "bind" && !m.ReadOnly {
			return []Risk{{ID: "bind-mount-data", Level: Medium, Args: []string{in.Svc.Name, m.Source},
				Evidence: m.Source + " -> " + m.Target}}
		}
	}
	return nil
}

// backupSpaceFactor: the volume archive plus the logical dump, both counted
// uncompressed, plus 10% headroom (DESIGN 5.5). The Docker data area is
// checked at snapshot time, when a helper container is run anyway.
const backupSpaceFactor = 2.2

func lowDisk(in Input) []Risk {
	if in.DataSize < 0 || in.BackupFree < 0 {
		return nil
	}
	need := int64(float64(in.DataSize) * backupSpaceFactor)
	if in.BackupFree >= need {
		return nil
	}
	return []Risk{{ID: "low-disk", Level: Block, Args: []string{humanBytes(in.BackupFree), humanBytes(need)},
		Evidence: fmt.Sprintf("data %s, free %s, needed %s", humanBytes(in.DataSize), humanBytes(in.BackupFree), humanBytes(need))}}
}

func humanBytes(n int64) string {
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
