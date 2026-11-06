package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"composesafe/internal/docker/dockertest"
	"composesafe/internal/errs"
)

const composeJSON = `{"name":"toy","services":{
 "db":{"image":"postgres:16-alpine","volumes":[{"type":"volume","source":"pgdata","target":"/var/lib/postgresql/data"}]},
 "web":{"image":"nginx:latest"}},
 "volumes":{"pgdata":{"name":"toy_pgdata"}}}`

func setup(t *testing.T, ctxJSON string) (file string, f *dockertest.Fake) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("COMPOSESAFE_LANG", "")
	dir := t.TempDir()
	file = filepath.Join(dir, "compose.yml")
	os.WriteFile(file, []byte("x"), 0o600)
	f = &dockertest.Fake{Responses: map[string]dockertest.Response{
		"context inspect":                                     {Stdout: ctxJSON},
		"version --format {{.Server.Version}}":                {Stdout: "29.0.0"},
		"compose version --short":                             {Stdout: "5.0.0"},
		"compose -p toy -f " + file + " config --format json": {Stdout: composeJSON},
	}}
	return file, f
}

const local = `[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`

func run(t *testing.T, f *dockertest.Fake, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewRoot(&out, &errOut, f)
	root.SetArgs(args)
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

func TestScanText(t *testing.T) {
	file, f := setup(t, local)
	out, err := run(t, f, "scan", "-f", file, "-p", "toy")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Project: toy", "postgres:16-alpine", "postgres ", "toy"[:0] + "pgdata→/var/lib/postgresql/data",
		`image tag "latest" is not a version`, "web:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestScanChinese(t *testing.T) {
	file, f := setup(t, local)
	out, err := run(t, f, "scan", "-f", file, "-p", "toy", "--lang", "zh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "项目:toy") || !strings.Contains(out, "不是版本号") {
		t.Errorf("expected Chinese output:\n%s", out)
	}
}

func TestScanJSON(t *testing.T) {
	file, f := setup(t, local)
	out, err := run(t, f, "scan", "-f", file, "-p", "toy", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Project struct {
			Services map[string]struct{ Kind string }
		}
		Env struct {
			ComposeVersion string `json:"compose_version"`
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Project.Services["db"].Kind != "postgres" || res.Env.ComposeVersion != "5.0.0" {
		t.Errorf("unexpected JSON: %s", out)
	}
}

func TestScanRefusesRemoteDocker(t *testing.T) {
	file, f := setup(t, `[{"Name":"prod","Endpoints":{"docker":{"Host":"ssh://root@prod"}}}]`)
	_, err := run(t, f, "scan", "-f", file, "-p", "toy")
	if err == nil || !strings.Contains(err.Error(), "not local") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.Calls {
		if c[0] == "compose" && len(c) > 2 && c[len(c)-1] == "json" {
			t.Error("compose project must not be loaded against a remote daemon")
		}
	}
}

func TestInvalidConfigIsReported(t *testing.T) {
	file, f := setup(t, local)
	cfgPath := filepath.Join(t.TempDir(), "composesafe.yaml")
	os.WriteFile(cfgPath, []byte("lang: fr\n"), 0o600)
	_, err := run(t, f, "scan", "-f", file, "-p", "toy", "-c", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "lang") {
		t.Fatalf("err = %v", err)
	}
}
func TestExitCodes(t *testing.T) {
	tests := map[error]int{
		nil:                    0,
		errs.ErrBlocked:        2,
		errs.ErrRolledBack:     3,
		errs.ErrRollbackFailed: 4,
		errors.New("boom"):     1,
	}
	for err, want := range tests {
		if got := errs.ExitCode(err); got != want {
			t.Errorf("ExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}

func TestVersionNeedsNoDocker(t *testing.T) {
	out, err := run(t, &dockertest.Fake{}, "version")
	if err != nil || !strings.HasPrefix(out, "composesafe ") {
		t.Fatalf("%q, %v", out, err)
	}
}

func TestHistoryReportAndCleanupGuards(t *testing.T) {
	t.Setenv("COMPOSESAFE_STATE_DIR", t.TempDir())
	_, f := setup(t, local)

	out, err := run(t, f, "history")
	if err != nil || !strings.Contains(out, "No runs yet") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := run(t, f, "report", "latest"); err == nil {
		t.Error("report of nothing")
	}
	// cleanup never takes --yes
	if _, err := run(t, f, "cleanup", "rehearsals", "--yes"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("--yes must be refused: %v", err)
	}
}
