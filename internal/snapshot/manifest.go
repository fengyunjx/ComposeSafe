// Package snapshot defines the on-disk snapshot (manifest.json plus the
// files it lists) and verifies it. Taking a snapshot is orchestrated by
// workflow, which needs Docker; this package only needs the filesystem.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is bumped when the manifest format changes incompatibly;
// readers refuse unknown versions.
const SchemaVersion = 1

// Manifest describes a snapshot (DESIGN 4.1). Everything a rollback needs is
// in this file and the artifacts it lists.
type Manifest struct {
	Schema    int         `json:"schema"`
	RunID     string      `json:"run_id"`
	Created   time.Time   `json:"created_at"`
	Version   string      `json:"composesafe_version"`
	Docker    DockerInfo  `json:"docker"`
	Project   ProjectInfo `json:"project"`
	Mode      string      `json:"mode"` // logical | volume | logical+volume
	Services  []Service   `json:"services"`
	Artifacts []Artifact  `json:"artifacts"`
	Quiesced  []string    `json:"quiesced,omitempty"` // writers stopped during the snapshot
	Verified  Verified    `json:"verified"`
	// Complete is false until every step succeeded; incomplete snapshots are
	// kept for diagnosis but never used for rollback.
	Complete bool     `json:"complete"`
	Warnings []string `json:"warnings,omitempty"`

	// Upgrade is set when the snapshot was taken for an upgrade; it tells a
	// later rollback which service changed and from what.
	Upgrade *UpgradeInfo `json:"upgrade,omitempty"`
	// PostUpgrade is filled once the compose files were changed.
	PostUpgrade *PostUpgrade `json:"post_upgrade,omitempty"`
	// Rollback is filled when this snapshot was used to roll back.
	Rollback *RollbackInfo `json:"rollback,omitempty"`
}

// UpgradeInfo identifies the upgrade a snapshot protects.
type UpgradeInfo struct {
	Service string `json:"service"`
	From    string `json:"from"` // full image reference before
	To      string `json:"to"`   // full image reference after

	// DataMoves is set by a Postgres major-version upgrade: the data was
	// restored into a NEW volume or directory and the compose file points at
	// it. The old one (Old) is never touched, so going back needs no data
	// restore.
	DataMoves []RestoredMount `json:"data_moves,omitempty"`
	// Stopped lists the services the upgrade stopped (the writers of a
	// database being migrated); a rollback starts them again.
	Stopped []string `json:"stopped,omitempty"`
}

// RollbackInfo records how a rollback went.
type RollbackInfo struct {
	At       time.Time       `json:"at"`
	Kind     string          `json:"kind"` // config | data
	Reason   string          `json:"reason,omitempty"`
	Restored []RestoredMount `json:"restored,omitempty"`
	Complete bool            `json:"complete"`
}

// RestoredMount: the data of a mount was unpacked into a NEW volume or
// directory; the previous one was left untouched.
type RestoredMount struct {
	Service string `json:"service"`
	Type    string `json:"type"` // volume | bind
	Old     string `json:"old"`  // volume name or host path that held the failed state
	New     string `json:"new"`
}

type DockerInfo struct {
	Engine  string `json:"engine"`
	Compose string `json:"compose"`
}

type ProjectInfo struct {
	Name     string   `json:"name"`
	Dir      string   `json:"dir"`
	Files    []string `json:"files"`
	EnvFiles []string `json:"env_files,omitempty"`
}

type Service struct {
	Name   string     `json:"name"`
	Kind   string     `json:"kind"`
	Image  ImageInfo  `json:"image"`
	Mounts []MountRef `json:"mounts,omitempty"` // the data that was archived

	PGUser    string         `json:"pg_user,omitempty"`
	PGVersion string         `json:"pg_version,omitempty"`
	PGTables  map[string]int `json:"pg_tables,omitempty"` // user tables per database at dump time
}

type ImageInfo struct {
	Ref         string   `json:"ref"`
	ImageID     string   `json:"image_id,omitempty"`
	RepoDigests []string `json:"repo_digests,omitempty"`
	// KeepTag pins the image locally so neither a prune nor a moved tag can
	// take it away. Empty when the image was not present locally.
	KeepTag string `json:"keep_tag,omitempty"`
}

type MountRef struct {
	Type   string `json:"type"`             // volume | bind
	Name   string `json:"name,omitempty"`   // docker volume name
	Source string `json:"source,omitempty"` // host path for bind mounts
	Target string `json:"target"`
}

// Artifact kinds.
const (
	KindCompose    = "compose"
	KindEnv        = "env"
	KindConfigJSON = "config-json"
	KindPGDump     = "pg_dump"
	// KindPGUpgradeDump is the dump taken after the writers were stopped, just
	// before a major-version migration restores it into the new cluster.
	KindPGUpgradeDump = "pg_upgrade_dump"
	KindVolume        = "volume"
)

type Artifact struct {
	Kind    string `json:"kind"`
	Service string `json:"service,omitempty"`
	Volume  string `json:"volume,omitempty"` // docker volume name or bind path
	Path    string `json:"path"`             // relative to the snapshot directory
	Source  string `json:"source,omitempty"` // original location of copied config files
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	RawSize int64  `json:"raw_size,omitempty"` // dumps: size before compression
}

type Verified struct {
	Checksums    bool `json:"checksums"`
	DumpTrailer  bool `json:"dump_trailer"`
	RestoreCheck bool `json:"restore_check"`
	// RestoreCheckSkipped is set when the check was not run (flag or no dump).
	RestoreCheckSkipped bool `json:"restore_check_skipped,omitempty"`
}

// PostUpgrade is recorded after an upgrade so rollback can notice later edits
// of the compose file (DESIGN 5.9).
type PostUpgrade struct {
	ComposeSHA256 string    `json:"compose_sha256"`
	ToImage       ImageInfo `json:"to_image"`
}

// Save writes manifest.json atomically with mode 0600.
func (m *Manifest) Save(dir string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}

// Load reads manifest.json from a snapshot directory.
func Load(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if m.Schema != SchemaVersion {
		return nil, fmt.Errorf("%s: manifest schema %d is not supported by this composesafe (expects %d); use a newer composesafe",
			dir, m.Schema, SchemaVersion)
	}
	return &m, nil
}

// Find returns the artifacts of a kind (optionally for one service).
func (m *Manifest) Find(kind, service string) []Artifact {
	var out []Artifact
	for _, a := range m.Artifacts {
		if a.Kind == kind && (service == "" || a.Service == service) {
			out = append(out, a)
		}
	}
	return out
}
