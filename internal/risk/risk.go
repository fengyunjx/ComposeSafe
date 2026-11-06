// Package risk evaluates an upgrade with deterministic rules. Each rule is a
// small function returning the risks it finds; Evaluate runs them in order.
// There is no rule engine or configuration language on purpose: adding a
// rule means adding a function and a line in the rules list.
//
// A Risk carries an ID plus arguments, not prose, so the CLI and the report
// can render it in any language ("risk.<ID>[.<Variant>]" in package i18n).
package risk

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"composesafe/internal/compose"
	"composesafe/internal/notes"
)

type Level int

const (
	Info Level = iota
	Medium
	High
	Critical
	Block
)

func (l Level) String() string {
	return [...]string{"INFO", "MEDIUM", "HIGH", "CRITICAL", "BLOCK"}[l]
}

func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// Risk is one finding.
type Risk struct {
	ID       string   `json:"id"`
	Level    Level    `json:"level"`
	Variant  string   `json:"variant,omitempty"`  // i18n key suffix when one ID has several wordings
	Args     []string `json:"args,omitempty"`     // values for the message template
	Evidence string   `json:"evidence,omitempty"` // what triggered it: versions, a release-note line
	Source   string   `json:"source,omitempty"`   // URL of the evidence
}

// Unknown marks a number the caller could not determine.
const Unknown int64 = -1

// Input is everything the rules look at. It is built by the plan workflow.
type Input struct {
	Project  *compose.Project
	Svc      *compose.Service
	From, To compose.ImageRef

	Notes         []notes.ReleaseNote
	NotesState    string   // "ok" | "nosource" | "failed" | "skipped"
	NotesWarnings []string // why fetching failed, if it did

	DigestKnown bool  // the running image's digest could be resolved
	HasSmoke    bool  // the user configured smoke checks for the service
	DataSize    int64 // bytes of data to protect, or Unknown
	BackupFree  int64 // free bytes where snapshots will be written, or Unknown

	Now time.Time
}

// Rule inspects an upgrade and returns zero or more risks.
type Rule func(Input) []Risk

var rules = []Rule{
	unversionedTag, downgrade, majorBump, pgMajorBump, pgDerivativeMajor, pgSkipVersions,
	pg18DataDir, pgEOL, variantChange, forwardOnlyKeywords, newEnvKeywords,
	notesUnavailable, noHealthcheck, digestUnpinned, bindMountData, lowDisk,
}

// Evaluate runs every rule. Results are de-duplicated and sorted with the
// most severe first.
func Evaluate(in Input) []Risk {
	var out []Risk
	seen := map[string]bool{}
	for _, rule := range rules {
		for _, r := range rule(in) {
			k := r.ID + "|" + r.Variant + "|" + r.Evidence
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Level > out[j].Level })
	return out
}

// Blocked reports whether any risk is BLOCK level.
func Blocked(rs []Risk) bool {
	for _, r := range rs {
		if r.Level == Block {
			return true
		}
	}
	return false
}

// Max returns the highest level among rs (Info for none).
func Max(rs []Risk) Level {
	m := Info
	for _, r := range rs {
		if r.Level > m {
			m = r.Level
		}
	}
	return m
}

func isPG(k compose.Kind) bool {
	return k == compose.KindPostgres || k == compose.KindPostgresDerivative
}

// excerpt shortens a release-note line to something readable.
func excerpt(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "- "))
	const max = 200
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
