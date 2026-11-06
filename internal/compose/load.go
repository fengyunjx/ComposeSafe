package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"composesafe/internal/docker"
)

// LoadOptions selects the Compose project to load.
type LoadOptions struct {
	Dir     string   // working directory; defaults to the directory of the first file, else cwd
	Files   []string // -f files; when empty the default file names are discovered in Dir
	Project string   // -p; empty lets Compose choose
	// KindOverride returns the `kind` set in composesafe.yaml for a service.
	KindOverride func(service string) string
}

var defaultNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// DiscoverFiles mirrors Compose's own lookup: the first default-named file,
// plus its override file when present. Returning explicit paths lets later
// stages back the files up.
func DiscoverFiles(dir string) ([]string, error) {
	for _, name := range defaultNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		files := []string{p}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		for _, ext := range []string{".yaml", ".yml"} {
			ov := filepath.Join(dir, base+".override"+ext)
			if _, err := os.Stat(ov); err == nil {
				files = append(files, ov)
				break
			}
		}
		return files, nil
	}
	return nil, fmt.Errorf("no compose file found in %s (looked for %s); use -f", dir, strings.Join(defaultNames, ", "))
}

// Load runs `docker compose config --format json` and builds the Project.
func Load(ctx context.Context, r docker.Runner, o LoadOptions) (*Project, error) {
	files := make([]string, 0, len(o.Files))
	for _, f := range o.Files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		files = append(files, abs)
	}
	dir := o.Dir
	switch {
	case dir != "":
	case len(files) > 0:
		dir = filepath.Dir(files[0])
	default:
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	dir, _ = filepath.Abs(dir)
	if len(files) == 0 {
		var err error
		if files, err = DiscoverFiles(dir); err != nil {
			return nil, err
		}
	}

	args := docker.Compose(o.Project, files, "config", "--format", "json")
	res, err := r.Run(ctx, args, docker.Opts{Dir: dir})
	if err != nil {
		return nil, fmt.Errorf("load compose project: %w", err)
	}
	p, err := parse(res.Stdout, o)
	if err != nil {
		return nil, err
	}
	p.Dir, p.Files, p.Raw = dir, files, res.Stdout
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		p.EnvFiles = []string{filepath.Join(dir, ".env")}
	}
	if p.Name == "" {
		p.Name = o.Project
	}
	return p, nil
}

type rawConfig struct {
	Name     string                `json:"name"`
	Services map[string]rawService `json:"services"`
	Volumes  map[string]struct {
		Name     string          `json:"name"`
		External json.RawMessage `json:"external"`
	} `json:"volumes"`
}

type rawService struct {
	Image         string                     `json:"image"`
	Build         json.RawMessage            `json:"build"`
	ContainerName string                     `json:"container_name"`
	DependsOn     map[string]json.RawMessage `json:"depends_on"`
	Healthcheck   *struct {
		Test    []string `json:"test"`
		Disable bool     `json:"disable"`
	} `json:"healthcheck"`
	Ports []struct {
		Target    int      `json:"target"`
		Published flexText `json:"published"`
		Protocol  string   `json:"protocol"`
	} `json:"ports"`
	Volumes []struct {
		Type     string `json:"type"`
		Source   string `json:"source"`
		Target   string `json:"target"`
		ReadOnly bool   `json:"read_only"`
	} `json:"volumes"`
}

// flexText accepts a JSON string or number (Compose versions differ).
type flexText string

func (f *flexText) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		s = ""
	}
	*f = flexText(s)
	return nil
}

// FromJSON builds a Project from the output of `docker compose config
// --format json` that was saved earlier (a snapshot keeps it). Rollback uses
// it so that it never depends on the compose files still being valid.
func FromJSON(data []byte, kindOverride func(service string) string) (*Project, error) {
	p, err := parse(data, LoadOptions{KindOverride: kindOverride})
	if err != nil {
		return nil, err
	}
	p.Raw = data
	return p, nil
}

func parse(data []byte, o LoadOptions) (*Project, error) {
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse `docker compose config` output: %w", err)
	}
	if len(raw.Services) == 0 {
		return nil, errors.New("compose project defines no services")
	}
	p := &Project{Name: raw.Name, Services: map[string]*Service{}, Volumes: map[string]VolumeDef{}}
	for k, v := range raw.Volumes {
		ext := len(v.External) > 0 && string(v.External) != "null" && string(v.External) != "false"
		name := v.Name
		if name == "" {
			name = k
		}
		p.Volumes[k] = VolumeDef{Name: name, External: ext}
	}

	for name, rs := range raw.Services {
		s := &Service{
			Name:          name,
			Image:         rs.Image,
			HasBuild:      len(rs.Build) > 0 && string(rs.Build) != "null",
			ContainerName: rs.ContainerName,
		}
		if rs.Image != "" {
			s.Ref = ParseRef(rs.Image)
		}
		for dep := range rs.DependsOn {
			s.DependsOn = append(s.DependsOn, dep)
		}
		sort.Strings(s.DependsOn)
		if h := rs.Healthcheck; h != nil && !h.Disable && !(len(h.Test) > 0 && h.Test[0] == "NONE") {
			s.HasHealthcheck = true
		}
		for _, pt := range rs.Ports {
			s.Ports = append(s.Ports, Port{Target: pt.Target, Published: string(pt.Published), Protocol: pt.Protocol})
		}
		for _, m := range rs.Volumes {
			mt := Mount{Type: m.Type, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly}
			if m.Type == "volume" && m.Source != "" {
				if vd, ok := p.Volumes[m.Source]; ok {
					mt.VolumeName, mt.External = vd.Name, vd.External
				} else {
					mt.VolumeName = m.Source
				}
			}
			s.Mounts = append(s.Mounts, mt)
		}

		override := ""
		if o.KindOverride != nil {
			override = o.KindOverride(name)
		}
		s.Kind, s.KindReason = DetectKind(s, override)
		if s.Kind == KindNone {
			for _, m := range s.Mounts {
				if m.Type == "volume" && !m.ReadOnly {
					s.MaybeStateful = true // unknown purpose: ask before upgrading
				}
			}
		}
		p.Services[name] = s
	}
	return p, nil
}

// portString renders a published port mapping for display.
func (p Port) String() string {
	t := strconv.Itoa(p.Target)
	if p.Published != "" {
		t = p.Published + ":" + t
	}
	if p.Protocol != "" && p.Protocol != "tcp" {
		t += "/" + p.Protocol
	}
	return t
}
