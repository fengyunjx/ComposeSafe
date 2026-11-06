// Package workflow orchestrates the commands. Each command is a plain
// function that composes the lower-level packages.
package workflow

import (
	"context"

	"composesafe/internal/compose"
	"composesafe/internal/config"
	"composesafe/internal/docker"
)

// ScanOptions selects the project to scan.
type ScanOptions struct {
	Dir     string
	Files   []string
	Project string
}

// Note is a machine-readable observation about a service. The CLI turns it
// into a translated sentence.
type Note struct {
	Service string `json:"service"`
	Code    string `json:"code"` // key suffix under "note." in i18n
	Arg     string `json:"arg,omitempty"`
}

// ScanResult is what `composesafe scan` reports (also its --json output).
type ScanResult struct {
	Env     docker.EnvInfo   `json:"env"`
	Project *compose.Project `json:"project"`
	Notes   []Note           `json:"notes"`
}

// Scan checks the Docker environment, loads the Compose project and
// classifies its services. It changes nothing.
func Scan(ctx context.Context, r docker.Runner, cfg *config.Config, o ScanOptions) (*ScanResult, error) {
	env, err := docker.CheckEnv(ctx, r)
	if err != nil {
		return nil, err
	}
	project := o.Project
	if project == "" {
		project = cfg.Project
	}
	p, err := compose.Load(ctx, r, compose.LoadOptions{
		Dir:          o.Dir,
		Files:        o.Files,
		Project:      project,
		KindOverride: func(s string) string { return cfg.Service(s).Kind },
	})
	if err != nil {
		return nil, err
	}
	return &ScanResult{Env: env, Project: p, Notes: scanNotes(p)}, nil
}

func scanNotes(p *compose.Project) []Note {
	var out []Note
	for _, name := range p.ServiceNames() {
		s := p.Services[name]
		add := func(code, arg string) { out = append(out, Note{Service: name, Code: code, Arg: arg}) }

		if s.Image != "" && s.Ref.Version == nil && s.Ref.Digest == "" {
			add("unversioned", s.Ref.Tag)
		}
		if s.MaybeStateful {
			add("maybe", "")
		}
		if s.Kind == compose.KindPostgresDerivative {
			add("derivative", "")
		}
		if s.Kind == compose.KindUnknownVolume {
			add("cold", s.KindReason)
		}
		if s.ContainerName != "" {
			add("container", s.ContainerName)
		}
		if s.Kind != compose.KindNone && !s.HasHealthcheck {
			add("nohealth", "")
		}
		for _, m := range s.Mounts {
			if m.External && s.Kind != compose.KindNone {
				add("external", m.VolumeName)
			}
		}
	}
	return out
}
