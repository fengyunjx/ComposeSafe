// Package config loads composesafe.yaml (see DESIGN 8.1).
//
// Precedence: command-line flags > environment > composesafe.yaml > defaults.
// Flags are applied by the caller after Load.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const FileName = "composesafe.yaml"

type Config struct {
	Version          int                 `yaml:"version"`
	Project          string              `yaml:"project"`
	StateDir         string              `yaml:"state_dir"`
	Lang             string              `yaml:"lang"`
	HelperImage      string              `yaml:"helper_image"`
	SmokeHelperImage string              `yaml:"smoke_helper_image"`
	Retention        Retention           `yaml:"retention"`
	Services         map[string]*Service `yaml:"services"`
	Vars             map[string]string   `yaml:"vars"`
	LLM              LLM                 `yaml:"llm"`

	// Path is the file the config was read from ("" when defaults only).
	Path string `yaml:"-"`
}

type Retention struct {
	Snapshots int `yaml:"snapshots"`
}

type Service struct {
	Kind          string        `yaml:"kind"` // postgres | unknown | none | "" (auto)
	SourceRepo    string        `yaml:"source_repo"`
	PGPasswordEnv string        `yaml:"pg_password_env"`
	Snapshot      Snapshot      `yaml:"snapshot"`
	SnapshotWith  []string      `yaml:"snapshot_with"`
	HealthTimeout time.Duration `yaml:"health_timeout"`
	Smoke         []Smoke       `yaml:"smoke"`
}

type Snapshot struct {
	Mode         string   `yaml:"mode"` // logical | volume | logical+volume
	Quiesce      []string `yaml:"quiesce"`
	RestoreCheck *bool    `yaml:"restore_check"`
}

type Smoke struct {
	Name               string        `yaml:"name"`
	Type               string        `yaml:"type"` // http | tcp | command | exec
	URL                string        `yaml:"url"`
	Addr               string        `yaml:"addr"`
	Cmd                []string      `yaml:"cmd"`
	ExpectStatus       int           `yaml:"expect_status"`
	ExpectBodyContains string        `yaml:"expect_body_contains"`
	Retries            int           `yaml:"retries"`
	Interval           time.Duration `yaml:"interval"`
	Timeout            time.Duration `yaml:"timeout"`
}

type LLM struct {
	Enabled   bool          `yaml:"enabled"`
	Provider  string        `yaml:"provider"`
	Model     string        `yaml:"model"`
	BaseURL   string        `yaml:"base_url"`
	APIKeyEnv string        `yaml:"api_key_env"`
	Timeout   time.Duration `yaml:"timeout"`
}

var (
	validKinds     = set("", "postgres", "unknown", "none")
	validModes     = set("", "logical", "volume", "logical+volume")
	validSmoke     = set("http", "tcp", "command", "exec")
	validLangs     = set("en", "zh")
	validProviders = set("claude", "openai", "deepseek", "qwen", "custom")
)

// llmDefaults are the documented default endpoints and key variables.
// Verify against each vendor's docs before release (DESIGN V4).
var llmDefaults = map[string][2]string{
	"claude":   {"https://api.anthropic.com", "ANTHROPIC_API_KEY"},
	"openai":   {"https://api.openai.com/v1", "OPENAI_API_KEY"},
	"deepseek": {"https://api.deepseek.com", "DEEPSEEK_API_KEY"},
	"qwen":     {"https://dashscope.aliyuncs.com/compatible-mode/v1", "DASHSCOPE_API_KEY"},
}

// LLMDefaults returns the default base URL and API-key variable of a provider.
func LLMDefaults(provider string) (baseURL, keyEnv string) {
	d := llmDefaults[provider]
	return d[0], d[1]
}

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		Version:          1,
		StateDir:         "~/.local/state/composesafe",
		Lang:             "en",
		HelperImage:      "alpine:3",
		SmokeHelperImage: "curlimages/curl",
		Retention:        Retention{Snapshots: 5},
		LLM:              LLM{Timeout: 60 * time.Second},
	}
}

// Find locates the config file: explicit path, then the compose directory,
// then the current directory. It returns "" when none exists.
func Find(explicit, composeDir, cwd string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file: %w", err)
		}
		return explicit, nil
	}
	for _, dir := range []string{composeDir, cwd} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, FileName)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", nil
}

// Load reads path (may be "") on top of the defaults and validates the result.
// Unknown keys are rejected, which also guarantees that secrets cannot be
// written into the file: there is no field for them.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		cfg.Path = path
	}
	if err := cfg.validate(); err != nil {
		if path != "" {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return nil, err
	}
	return cfg, nil
}

// ApplyEnv applies COMPOSESAFE_* environment overrides.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	if v := getenv("COMPOSESAFE_STATE_DIR"); v != "" {
		c.StateDir = v
	}
	if v := getenv("COMPOSESAFE_LANG"); v != "" {
		c.Lang = v
	}
	if v := getenv("COMPOSESAFE_LLM_PROVIDER"); v != "" {
		c.LLM.Provider = v
	}
	if v := getenv("COMPOSESAFE_LLM_MODEL"); v != "" {
		c.LLM.Model = v
	}
	return c.validate()
}

// Service returns the effective settings of a service with defaults filled in.
// It never returns nil.
func (c *Config) Service(name string) Service {
	var s Service
	if p := c.Services[name]; p != nil {
		s = *p
	}
	if s.Snapshot.Mode == "" {
		s.Snapshot.Mode = "logical+volume"
	}
	if s.Snapshot.RestoreCheck == nil {
		t := true
		s.Snapshot.RestoreCheck = &t
	}
	if s.HealthTimeout == 0 {
		s.HealthTimeout = 120 * time.Second
	}
	return s
}

// ResolvedStateDir expands a leading "~" in StateDir.
func (c *Config) ResolvedStateDir() (string, error) {
	d := c.StateDir
	if d == "~" || strings.HasPrefix(d, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, strings.TrimPrefix(d, "~"))
	}
	return filepath.Abs(d)
}

func (c *Config) validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version: unsupported value %d (expected 1)", c.Version)
	}
	if !validLangs[c.Lang] {
		return fmt.Errorf("lang: %q is not one of %s", c.Lang, keys(validLangs))
	}
	for _, name := range sortedKeys(c.Services) {
		s := c.Services[name]
		if s == nil {
			continue
		}
		p := "services." + name
		if !validKinds[s.Kind] {
			return fmt.Errorf("%s.kind: %q is not one of postgres, unknown, none", p, s.Kind)
		}
		if !validModes[s.Snapshot.Mode] {
			return fmt.Errorf("%s.snapshot.mode: %q is not one of logical, volume, logical+volume", p, s.Snapshot.Mode)
		}
		for i := range s.Smoke {
			if err := s.Smoke[i].normalize(fmt.Sprintf("%s.smoke[%d]", p, i)); err != nil {
				return err
			}
		}
	}
	return c.LLM.validate()
}

func (m *Smoke) normalize(p string) error {
	if !validSmoke[m.Type] {
		return fmt.Errorf("%s.type: %q is not one of http, tcp, command, exec", p, m.Type)
	}
	switch m.Type {
	case "http":
		if m.URL == "" {
			return fmt.Errorf("%s.url: required for type http", p)
		}
	case "tcp":
		if m.Addr == "" {
			return fmt.Errorf("%s.addr: required for type tcp", p)
		}
	case "command", "exec":
		if len(m.Cmd) == 0 {
			return fmt.Errorf("%s.cmd: required for type %s", p, m.Type)
		}
	}
	if m.Type == "http" && m.ExpectStatus == 0 {
		m.ExpectStatus = 200
	}
	if m.Retries == 0 {
		m.Retries = 10
	}
	if m.Interval == 0 {
		m.Interval = 3 * time.Second
	}
	if m.Timeout == 0 {
		m.Timeout = 10 * time.Second
	}
	return nil
}

func (l *LLM) validate() error {
	if l.Provider != "" && !validProviders[l.Provider] {
		return fmt.Errorf("llm.provider: %q is not one of %s", l.Provider, keys(validProviders))
	}
	if !l.Enabled {
		return nil
	}
	if l.Provider == "" {
		return errors.New("llm.provider: required when llm.enabled is true")
	}
	if l.Model == "" {
		return errors.New("llm.model: required when llm.enabled is true (no built-in default: model names go stale)")
	}
	if l.Provider == "custom" && (l.BaseURL == "" || l.APIKeyEnv == "") {
		return errors.New("llm.base_url and llm.api_key_env: both required for provider custom")
	}
	return nil
}

func set(vals ...string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

func keys(m map[string]bool) string {
	var ks []string
	for k := range m {
		if k != "" {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
