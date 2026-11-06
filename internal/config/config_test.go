package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsWithoutFile(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Lang != "en" || c.HelperImage != "alpine:3" || c.Retention.Snapshots != 5 || c.LLM.Enabled {
		t.Errorf("unexpected defaults: %+v", c)
	}
	s := c.Service("db")
	if s.Snapshot.Mode != "logical+volume" || !*s.Snapshot.RestoreCheck || s.HealthTimeout != 120*time.Second {
		t.Errorf("unexpected service defaults: %+v", s)
	}
}

func TestLoadFull(t *testing.T) {
	p := write(t, `
version: 1
project: myapp
lang: zh
services:
  db:
    kind: postgres
    snapshot: {mode: logical, restore_check: false, quiesce: [app]}
  app:
    snapshot_with: [db]
    health_timeout: 90s
    smoke:
      - {name: ok, type: http, url: "http://{{addr \"app\" 8080}}/healthz"}
      - {name: shell, type: exec, cmd: ["true"], retries: 2}
llm:
  enabled: true
  provider: deepseek
  model: some-model
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Lang != "zh" || c.Project != "myapp" || c.Path != p {
		t.Errorf("basic fields wrong: %+v", c)
	}
	db := c.Service("db")
	if db.Kind != "postgres" || *db.Snapshot.RestoreCheck || db.Snapshot.Mode != "logical" {
		t.Errorf("db: %+v", db)
	}
	app := c.Service("app")
	if app.HealthTimeout != 90*time.Second || len(app.SnapshotWith) != 1 {
		t.Errorf("app: %+v", app)
	}
	h := app.Smoke[0]
	if h.ExpectStatus != 200 || h.Retries != 10 || h.Interval != 3*time.Second || h.Timeout != 10*time.Second {
		t.Errorf("smoke defaults not applied: %+v", h)
	}
	if app.Smoke[1].Retries != 2 {
		t.Error("explicit retries overwritten")
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"unknown key (typo)", "lagn: zh\n", "lagn"},
		{"plaintext secret has no field", "llm:\n  api_key: sk-123\n", "api_key"},
		{"bad version", "version: 2\n", "version"},
		{"bad lang", "lang: fr\n", "lang"},
		{"bad kind", "services:\n  db:\n    kind: mysql\n", "services.db.kind"},
		{"bad mode", "services:\n  db:\n    snapshot: {mode: fast}\n", "snapshot.mode"},
		{"bad smoke type", "services:\n  a:\n    smoke: [{type: ping}]\n", "services.a.smoke[0].type"},
		{"http without url", "services:\n  a:\n    smoke: [{type: http}]\n", "smoke[0].url"},
		{"exec without cmd", "services:\n  a:\n    smoke: [{type: exec}]\n", "smoke[0].cmd"},
		{"llm enabled without provider", "llm: {enabled: true, model: m}\n", "llm.provider"},
		{"llm enabled without model", "llm: {enabled: true, provider: openai}\n", "llm.model"},
		{"custom needs endpoint", "llm: {enabled: true, provider: custom, model: m}\n", "base_url"},
		{"unknown provider", "llm: {provider: gemini}\n", "llm.provider"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestApplyEnv(t *testing.T) {
	c, _ := Load("")
	env := map[string]string{"COMPOSESAFE_LANG": "zh", "COMPOSESAFE_STATE_DIR": "/data/cs", "COMPOSESAFE_LLM_PROVIDER": "qwen", "COMPOSESAFE_LLM_MODEL": "m"}
	if err := c.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if c.Lang != "zh" || c.StateDir != "/data/cs" || c.LLM.Provider != "qwen" || c.LLM.Model != "m" {
		t.Errorf("env not applied: %+v", c)
	}
	bad := map[string]string{"COMPOSESAFE_LANG": "fr"}
	if err := c.ApplyEnv(func(k string) string { return bad[k] }); err == nil {
		t.Error("invalid env value must be rejected")
	}
}

func TestFindPrecedence(t *testing.T) {
	composeDir, cwd := t.TempDir(), t.TempDir()
	for _, d := range []string{composeDir, cwd} {
		os.WriteFile(filepath.Join(d, FileName), []byte("version: 1\n"), 0o600)
	}
	got, _ := Find("", composeDir, cwd)
	if got != filepath.Join(composeDir, FileName) {
		t.Errorf("compose dir must win over cwd, got %s", got)
	}
	os.Remove(filepath.Join(composeDir, FileName))
	got, _ = Find("", composeDir, cwd)
	if got != filepath.Join(cwd, FileName) {
		t.Errorf("fallback to cwd failed, got %s", got)
	}
	if got, _ := Find("", t.TempDir(), t.TempDir()); got != "" {
		t.Errorf("no file should give empty path, got %s", got)
	}
	if _, err := Find(filepath.Join(cwd, "missing.yaml"), "", ""); err == nil {
		t.Error("explicit missing file must error")
	}
}

func TestResolvedStateDir(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	c, _ := Load("")
	d, err := c.ResolvedStateDir()
	if err != nil || d != "/home/u/.local/state/composesafe" {
		t.Errorf("got %q, %v", d, err)
	}
}

func TestLLMDefaults(t *testing.T) {
	base, key := LLMDefaults("deepseek")
	if !strings.HasPrefix(base, "https://") || key != "DEEPSEEK_API_KEY" {
		t.Errorf("got %q %q", base, key)
	}
	if b, k := LLMDefaults("custom"); b != "" || k != "" {
		t.Error("custom has no defaults")
	}
}
