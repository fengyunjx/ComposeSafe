// Package compose loads a Compose project through `docker compose config`
// and classifies its services. Rewriting compose files arrives in M4.
package compose

import (
	"sort"

	"composesafe/internal/version"
)

// Kind says how composesafe treats a service's data.
type Kind int

const (
	KindNone               Kind = iota // stateless as far as we can tell
	KindPostgres                       // official postgres image: full support
	KindPostgresDerivative             // postgis, timescaledb...: same-major only
	KindUnknownVolume                  // other databases / data directories: cold backup
)

func (k Kind) String() string {
	switch k {
	case KindPostgres:
		return "postgres"
	case KindPostgresDerivative:
		return "postgres-derivative"
	case KindUnknownVolume:
		return "volume-data"
	}
	return "none"
}

func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// Project is the part of the expanded Compose model composesafe uses.
type Project struct {
	Name     string               `json:"name"`
	Dir      string               `json:"dir"`
	Files    []string             `json:"files"`
	EnvFiles []string             `json:"env_files"`
	Services map[string]*Service  `json:"services"`
	Volumes  map[string]VolumeDef `json:"volumes"`

	// Raw is the expanded `docker compose config --format json` output, kept
	// for the snapshot's audit copy. It contains interpolated variable values
	// and must be treated as a secret.
	Raw []byte `json:"-"`
}

// ServiceNames returns the service names in alphabetical order.
func (p *Project) ServiceNames() []string {
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// VolumeDef is a top-level volume declaration.
type VolumeDef struct {
	Name     string `json:"name"` // the real Docker volume name
	External bool   `json:"external"`
}

type Service struct {
	Name           string   `json:"name"`
	Image          string   `json:"image"` // as written; empty for build-only services
	Ref            ImageRef `json:"ref"`
	HasBuild       bool     `json:"has_build"`
	Mounts         []Mount  `json:"mounts"`
	DependsOn      []string `json:"depends_on"`
	HasHealthcheck bool     `json:"has_healthcheck"` // declared in compose (not in the image)
	ContainerName  string   `json:"container_name,omitempty"`
	Ports          []Port   `json:"ports,omitempty"`

	Kind          Kind   `json:"kind"`
	KindReason    string `json:"kind_reason,omitempty"`
	MaybeStateful bool   `json:"maybe_stateful,omitempty"` // named volume of unknown purpose
}

// Mount is one entry of a service's volumes.
type Mount struct {
	Type       string `json:"type"`                  // volume | bind | tmpfs
	Source     string `json:"source,omitempty"`      // volume key or host path
	VolumeName string `json:"volume_name,omitempty"` // resolved Docker volume name
	External   bool   `json:"external,omitempty"`
	Target     string `json:"target"`
	ReadOnly   bool   `json:"read_only,omitempty"`
}

type Port struct {
	Target    int    `json:"target"`
	Published string `json:"published,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
}

// ImageRef is a parsed image reference.
type ImageRef struct {
	Raw      string           `json:"raw"`
	Registry string           `json:"registry"` // "docker.io" when omitted
	Repo     string           `json:"repo"`     // "library/postgres"
	Tag      string           `json:"tag"`
	Digest   string           `json:"digest,omitempty"`
	Version  *version.Version `json:"version,omitempty"`
	Variant  string           `json:"variant,omitempty"`
}
