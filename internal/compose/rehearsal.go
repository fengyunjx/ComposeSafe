package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// RehearsalOptions describes the copy of a project that a rehearsal runs.
type RehearsalOptions struct {
	// Name is the project name of the copy, "composesafe-rh-<id>"; every
	// volume of the copy is called "<Name>_<key>".
	Name string
	ID   string
	// BindDir is where copies of writable bind-mounted directories go.
	BindDir string
	// AllowEgress leaves the network open; by default the copy has no route
	// out, so it cannot send mail or call the real services production uses.
	AllowEgress bool
}

// VolumeCopy: a named volume of production and the private volume that takes
// its place in the rehearsal. The copy is declared external so that Compose
// neither creates nor deletes it: the rehearsal does, by label.
type VolumeCopy struct {
	Key     string `json:"key"`
	Prod    string `json:"prod"` // the Docker volume name production uses
	Copy    string `json:"copy"`
	Service string `json:"service"` // first service that mounts it
}

// BindCopy: a writable host directory production mounts and its private copy.
type BindCopy struct {
	Service string `json:"service"`
	Prod    string `json:"prod"`
	Copy    string `json:"copy"`
}

// RehearsalNote is an observation for the report; Code is an i18n key suffix.
type RehearsalNote struct {
	Service string `json:"service,omitempty"`
	Code    string `json:"code"`
	Arg     string `json:"arg,omitempty"`
}

// Rehearsal is the generated project.
type Rehearsal struct {
	YAML    []byte          `json:"-"`
	Volumes []VolumeCopy    `json:"volumes"`
	Binds   []BindCopy      `json:"binds"`
	Notes   []RehearsalNote `json:"notes"`
}

var rhName = regexp.MustCompile(`^composesafe-rh-[a-z0-9-]+$`)

// BuildRehearsal turns the expanded model of production (the output of
// `docker compose config --format json`) into a project that is safe to run
// beside it: no container names, no published ports, no external networks or
// volumes, private copies of writable data, no route out unless asked, no
// restart policy. The result is a single block-style YAML file, so the usual
// text edits of an upgrade work on it unchanged.
func BuildRehearsal(raw []byte, o RehearsalOptions) (*Rehearsal, error) {
	if !rhName.MatchString(o.Name) || o.ID == "" || !strings.Contains(o.Name, o.ID) {
		return nil, fmt.Errorf("refusing to build a rehearsal named %q", o.Name)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("read the expanded model: %w", err)
	}
	r := &Rehearsal{}
	note := func(svc, code, arg string) {
		r.Notes = append(r.Notes, RehearsalNote{Service: svc, Code: code, Arg: arg})
	}

	doc["name"] = o.Name
	prodVolumes, _ := doc["volumes"].(map[string]any)
	used := map[string]string{} // volume key -> first service
	services, _ := doc["services"].(map[string]any)
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		svc, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := svc["container_name"]; ok {
			note(name, "container_name", fmt.Sprint(v))
			delete(svc, "container_name")
		}
		if v, ok := svc["ports"].([]any); ok && len(v) > 0 {
			note(name, "ports", fmt.Sprint(len(v)))
		}
		delete(svc, "ports")
		delete(svc, "external_links")
		svc["restart"] = "no"

		mode, _ := svc["network_mode"].(string)
		switch {
		case mode == "host" || strings.HasPrefix(mode, "container:"):
			return nil, fmt.Errorf("service %s uses network_mode %q, which would reach production: it cannot be rehearsed", name, mode)
		case mode == "bridge" && !o.AllowEgress:
			return nil, fmt.Errorf("service %s uses network_mode bridge, which has a route out: use --allow-egress or change it", name)
		case mode == "":
			svc["networks"] = mergedNetworks(svc["networks"])
		}

		labels, _ := svc["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels["composesafe.rehearsal"] = o.ID
		svc["labels"] = labels

		if vols, ok := svc["volumes"].([]any); ok {
			for _, e := range vols {
				m, ok := e.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := m["type"].(string)
				src, _ := m["source"].(string)
				switch typ {
				case "bind":
					if base := filepath.Base(src); base == "docker.sock" || base == "podman.sock" {
						return nil, fmt.Errorf("service %s mounts %s: a copy could control production containers, so it cannot be rehearsed", name, src)
					}
					if ro, _ := m["read_only"].(bool); ro {
						continue
					}
					fi, err := os.Stat(src)
					if err == nil && !fi.IsDir() {
						return nil, fmt.Errorf("service %s mounts the file %s read-write; mount it read-only (:ro) or take it out of the rehearsal", name, src)
					}
					dest := filepath.Join(o.BindDir, name, sanitize(src))
					r.Binds = append(r.Binds, BindCopy{Service: name, Prod: src, Copy: dest})
					m["source"] = dest
					note(name, "bind-copied", src)
				case "volume":
					if src == "" {
						continue // anonymous: Compose makes a fresh one
					}
					if _, seen := used[src]; !seen {
						used[src] = name
					}
				}
			}
		}
	}

	// Volumes: every one in use becomes a private external volume.
	newVols := map[string]any{}
	keys := make([]string, 0, len(used))
	for k := range used {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		prod := k
		if def, ok := prodVolumes[k].(map[string]any); ok {
			if n, ok := def["name"].(string); ok && n != "" {
				prod = n
			}
			if ext, _ := def["external"].(bool); ext {
				note(used[k], "external-volume", prod)
			}
		}
		cp := o.Name + "_" + k
		newVols[k] = map[string]any{"external": true, "name": cp}
		r.Volumes = append(r.Volumes, VolumeCopy{Key: k, Prod: prod, Copy: cp, Service: used[k]})
	}
	if len(newVols) > 0 {
		doc["volumes"] = newVols
	} else {
		delete(doc, "volumes")
	}

	net := map[string]any{"labels": map[string]any{"composesafe.rehearsal": o.ID}}
	if !o.AllowEgress {
		net["internal"] = true
	}
	doc["networks"] = map[string]any{"default": net}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	r.YAML = append([]byte("# generated by composesafe for a rehearsal; it holds expanded values and is private\n"), out...)
	return r, nil
}

// mergedNetworks puts a service on the one private network, keeping the
// aliases it had on the networks it used to be on.
func mergedNetworks(old any) map[string]any {
	var aliases []any
	seen := map[string]bool{}
	if m, ok := old.(map[string]any); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cfg, _ := m[k].(map[string]any)
			list, _ := cfg["aliases"].([]any)
			for _, a := range list {
				if s := fmt.Sprint(a); !seen[s] {
					seen[s] = true
					aliases = append(aliases, a)
				}
			}
		}
	}
	if len(aliases) == 0 {
		return map[string]any{"default": nil}
	}
	return map[string]any{"default": map[string]any{"aliases": aliases}}
}

var unsafePath = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitize(p string) string {
	s := strings.Trim(unsafePath.ReplaceAllString(p, "_"), "._")
	if s == "" {
		return "dir"
	}
	return s
}
