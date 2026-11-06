// Package plan holds the result types of `composesafe plan`, shared by the
// workflow that builds them and the renderers that print them.
package plan

import (
	"time"

	"composesafe/internal/compose"
	"composesafe/internal/docker"
	"composesafe/internal/notes"
	"composesafe/internal/registry"
	"composesafe/internal/risk"
)

// Path is how the upgrade would be carried out.
type Path string

const (
	// PathSimple replaces the image and restarts the service.
	PathSimple Path = "simple"
	// PathPGDumpRestore dumps a Postgres cluster and restores it into a new
	// volume under the new major version.
	PathPGDumpRestore Path = "pg-dump-restore"
)

// Plan is the assessment of one upgrade.
type Plan struct {
	Project string           `json:"project"`
	Service string           `json:"service"`
	From    compose.ImageRef `json:"from"`
	To      compose.ImageRef `json:"to"`
	Kind    compose.Kind     `json:"kind"`
	Path    Path             `json:"path"`

	Risks   []risk.Risk `json:"risks"`
	Blocked bool        `json:"blocked"`

	NotesSource   notes.Source        `json:"notes_source"`
	NotesState    string              `json:"notes_state"`
	Notes         []notes.ReleaseNote `json:"notes"`
	NotesWarnings []string            `json:"notes_warnings,omitempty"`

	Warnings []string `json:"warnings,omitempty"` // non-fatal problems met while assessing

	DataSize   int64 `json:"data_size"`   // -1 when unknown
	BackupFree int64 `json:"backup_free"` // -1 when unknown

	RunID      string    `json:"run_id,omitempty"`
	ReportPath string    `json:"report_path,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Result is what the plan command returns. Plan is nil when no target was
// given and only candidates are listed.
type Result struct {
	Env        docker.EnvInfo       `json:"env"`
	Service    string               `json:"service"`
	Current    compose.ImageRef     `json:"current"`
	Candidates []registry.Candidate `json:"candidates,omitempty"`
	Plan       *Plan                `json:"plan,omitempty"`

	// Project is the loaded Compose project, for callers that go on to act.
	Project *compose.Project `json:"-"`
}
