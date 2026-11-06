package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"composesafe/internal/docker/dockertest"
	"composesafe/internal/errs"
	"composesafe/internal/registry"
	"composesafe/internal/workflow"
)

type offline struct{}

func (offline) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }

// planSetup returns a fake Docker for a project whose registry is
// unreachable (so tests stay offline) and puts state under a temp dir.
func planSetup(t *testing.T, image string) (file string, f *dockertest.Fake) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("COMPOSESAFE_LANG", "")
	t.Setenv("COMPOSESAFE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	planDepsHook = func(d *workflow.PlanDeps) {
		// Hermetic: every registry request fails immediately, as if offline.
		d.Registry = registry.Client{AttemptTimeout: 100 * time.Millisecond,
			Opts: []remote.Option{remote.WithTransport(offline{})}}
		d.FreeBytes = func(string) (int64, error) { return 1 << 40, nil }
	}
	t.Cleanup(func() { planDepsHook = nil })

	dir := t.TempDir()
	file = filepath.Join(dir, "compose.yml")
	os.WriteFile(file, []byte("x"), 0o600)
	js := `{"name":"p","services":{"db":{"image":"` + image + `","healthcheck":{"test":["CMD","true"]},
	  "volumes":[{"type":"volume","source":"pg","target":"/var/lib/postgresql/data"}]}},"volumes":{"pg":{"name":"p_pg"}}}`
	f = &dockertest.Fake{Responses: map[string]dockertest.Response{
		"context inspect":                                   {Stdout: local},
		"version --format {{.Server.Version}}":              {Stdout: "29.0.0"},
		"compose version --short":                           {Stdout: "5.0.0"},
		"compose -p p -f " + file + " config --format json": {Stdout: js},
		"image inspect --format {{json .}} " + image:        {Stdout: `{"Id":"x","RepoDigests":["a@sha256:1"],"Config":{}}`},
		"system df -v --format json":                        {Stdout: `{"Volumes":[{"Name":"p_pg","Size":"1MB"}]}`},
		"info --format {{json .RegistryConfig.Mirrors}}":    {Stdout: "null"},
	}}
	return file, f
}

func TestPlanBlockedExitsWithCode2(t *testing.T) {
	file, f := planSetup(t, "postgis/postgis:15-3.4")
	out, err := run(t, f, "plan", "db=16-3.4", "-f", file, "-p", "p")
	if !errors.Is(err, errs.ErrBlocked) || errs.ExitCode(err) != 2 {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"Plan for db: postgis/postgis:15-3.4 → postgis/postgis:16-3.4", "BLOCK", "Postgres derivative image", "Result: BLOCKED by 1 risk(s)", "Report: "} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPlanChineseAndJSON(t *testing.T) {
	file, f := planSetup(t, "postgis/postgis:15-3.4")
	out, _ := run(t, f, "plan", "db=16-3.4", "-f", file, "-p", "p", "--lang", "zh")
	for _, want := range []string{"db 的升级计划", "风险", "Postgres 衍生镜像", "结论:被 1 项风险阻断"} {
		if !strings.Contains(out, want) {
			t.Errorf("zh output missing %q:\n%s", want, out)
		}
	}

	out, err := run(t, f, "plan", "db=16-3.4", "-f", file, "-p", "p", "--json")
	if !errors.Is(err, errs.ErrBlocked) {
		t.Fatalf("err = %v", err)
	}
	var res struct {
		Plan struct {
			Blocked bool
			Risks   []struct{ ID, Level string }
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !res.Plan.Blocked || res.Plan.Risks[0].ID != "pg-derivative-major" || res.Plan.Risks[0].Level != "BLOCK" {
		t.Errorf("json: %s", out)
	}
}

func TestPlanBadArguments(t *testing.T) {
	file, f := planSetup(t, "postgres:16.3")
	if _, err := run(t, f, "plan", "=16", "-f", file, "-p", "p"); err == nil {
		t.Error("empty service must be rejected")
	}
	if _, err := run(t, f, "plan", "-f", file, "-p", "p"); err == nil {
		t.Error("missing argument must be rejected")
	}
	if _, err := run(t, f, "plan", "nope=1", "-f", file, "-p", "p"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("unknown service: %v", err)
	}
}
