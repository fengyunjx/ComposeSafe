package cli

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"composesafe/internal/docker"
	"composesafe/internal/docker/dockertest"
	"composesafe/internal/errs"
	"composesafe/internal/registry"
	"composesafe/internal/workflow"
)

func upgradeSetup(t *testing.T) (file string, f *dockertest.Fake) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("COMPOSESAFE_LANG", "")
	t.Setenv("COMPOSESAFE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	upgradeDepsHook = func(d *workflow.UpgradeDeps) {
		d.Plan.Registry = registry.Client{AttemptTimeout: 20 * time.Millisecond, Opts: []remote.Option{remote.WithTransport(offline{})}}
		d.Plan.FreeBytes = func(string) (int64, error) { return 1 << 40, nil }
		d.Snapshot.FreeBytes = d.Plan.FreeBytes
	}
	t.Cleanup(func() { upgradeDepsHook = nil; planDepsHook = nil })
	planDepsHook = func(d *workflow.PlanDeps) {
		d.Registry = registry.Client{AttemptTimeout: 20 * time.Millisecond, Opts: []remote.Option{remote.WithTransport(offline{})}}
	}
	dir := t.TempDir()
	file = filepath.Join(dir, "compose.yml")
	os.WriteFile(file, []byte("services:\n  web:\n    image: x/web:1.0   # pin\n"), 0o644)
	f = &dockertest.Fake{Handler: func(args []string, _ docker.Opts) (dockertest.Response, bool) {
		line := strings.Join(args, " ")
		switch {
		case line == "context inspect":
			return dockertest.Response{Stdout: local}, true
		case line == "version --format {{.Server.Version}}":
			return dockertest.Response{Stdout: "29.0.0"}, true
		case line == "compose version --short":
			return dockertest.Response{Stdout: "5.0.0"}, true
		case line == "system df -v --format json":
			return dockertest.Response{Stdout: `{"Volumes":[]}`}, true
		case line == "info --format {{json .RegistryConfig.Mirrors}}":
			return dockertest.Response{Stdout: "null"}, true
		case strings.HasPrefix(line, "image inspect"):
			return dockertest.Response{Stdout: `{"Id":"x","RepoDigests":["a@sha256:1"],"Config":{}}`}, true
		case strings.HasPrefix(line, "compose -p p -f") && strings.HasSuffix(line, "config --format json"):
			return dockertest.Response{Stdout: `{"name":"p","services":{"web":{"image":"x/web:1.0","healthcheck":{"test":["CMD","true"]}}}}`}, true
		}
		return dockertest.Response{}, false
	}}
	return file, f
}

func TestUpgradeDryRunShowsTheChangeAndTouchesNothing(t *testing.T) {
	file, f := upgradeSetup(t)
	before, _ := os.ReadFile(file)
	out, err := run(t, f, "upgrade", "web=1.1", "-f", file, "-p", "p")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Upgrade web: x/web:1.0 → x/web:1.1", "File changes", "services.web.image x/web:1.0 → x/web:1.1",
		"Before changing anything a snapshot is taken", "Dry run: nothing was changed. Add --execute to upgrade."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Error("dry run changed the compose file")
	}
	for _, c := range f.Lines() {
		for _, bad := range []string{" pull", " up ", " stop", " start"} {
			if strings.Contains(c, bad) {
				t.Errorf("dry run ran %q", c)
			}
		}
	}
}

func TestUpgradeArgumentsAndRefusals(t *testing.T) {
	file, f := upgradeSetup(t)
	for _, arg := range []string{"web", "=1.1", "web="} {
		if _, err := run(t, f, "upgrade", arg, "-f", file, "-p", "p"); err == nil {
			t.Errorf("%q must be rejected", arg)
		}
	}
	// without --yes and without a terminal an execution cannot be confirmed
	_, err := run(t, f, "upgrade", "web=1.1", "-f", file, "-p", "p", "--execute")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// a major bump is refused with --yes alone and exits with the blocked code
	_, err = run(t, f, "upgrade", "web=2.0", "-f", file, "-p", "p", "--execute", "--yes")
	if !errors.Is(err, errs.ErrBlocked) || errs.ExitCode(err) != 2 || !strings.Contains(err.Error(), "--allow-risk=major-bump") {
		t.Errorf("err = %v", err)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), "x/web:1.0") {
		t.Error("the file must not change when the upgrade is refused")
	}
}

func TestUpgradeEditThatCannotBeAutomatic(t *testing.T) {
	file, f := upgradeSetup(t)
	os.WriteFile(file, []byte("x-img: &img x/web:1.0\nservices:\n  web:\n    image: *img\n"), 0o644)
	_, err := run(t, f, "upgrade", "web=1.1", "-f", file, "-p", "p")
	if err == nil || !strings.Contains(err.Error(), "cannot edit the compose file automatically") {
		t.Errorf("err = %v", err)
	}
}

func TestRollbackNeedsASnapshot(t *testing.T) {
	file, f := upgradeSetup(t)
	f.Responses = nil
	_, err := run(t, f, "rollback", "-f", file, "-p", "p")
	if err == nil || !strings.Contains(err.Error(), "no snapshots found") {
		t.Errorf("err = %v", err)
	}
	if errs.ExitCode(err) != 1 {
		t.Errorf("exit code %d", errs.ExitCode(err))
	}
}

var _ = http.StatusOK
