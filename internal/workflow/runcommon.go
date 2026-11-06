package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"composesafe/internal/compose"
	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/i18n"
	"composesafe/internal/pg"
	"composesafe/internal/report"
	"composesafe/internal/smoke"
)

// RunDeps are the collaborators shared by upgrade and rollback.
type RunDeps struct {
	Runner   docker.Runner
	Now      func() time.Time
	Sleep    func(time.Duration)
	Progress func(key string, args ...any)
	// Smoke runs the checks; nil means smoke.Run.
	Smoke func(context.Context, smoke.Env, []config.Smoke) []smoke.Result
	HTTP  *http.Client
	// StableWindow is how long a service without a healthcheck must stay up
	// and not restart to count as healthy; 20 seconds when zero.
	StableWindow time.Duration
	UID, GID     int
	Version      string
}

func (d *RunDeps) fill() {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = time.Sleep
	}
	if d.Progress == nil {
		d.Progress = func(string, ...any) {}
	}
	if d.Smoke == nil {
		d.Smoke = smoke.Run
	}
	if d.StableWindow == 0 {
		d.StableWindow = 20 * time.Second
	}
}

// VerifyResult is the outcome of checking a service after a change.
type VerifyResult struct {
	Healthy bool           `json:"healthy"`
	Detail  string         `json:"detail,omitempty"`
	Smoke   []smoke.Result `json:"smoke,omitempty"`
}

// OK reports whether the service is healthy and every smoke check passed.
func (v VerifyResult) OK() bool { return v.Healthy && smoke.AllOK(v.Smoke) }

// Failure describes what failed, for logs and reports.
func (v VerifyResult) Failure() string {
	if !v.Healthy {
		return "not healthy: " + v.Detail
	}
	for _, s := range v.Smoke {
		if !s.OK {
			return fmt.Sprintf("smoke check %q failed: %s", s.Name, s.Detail)
		}
	}
	return ""
}

// verifyService decides whether a service came up properly: healthy (by its
// compose healthcheck, or by staying up and not restarting) and passing the
// smoke checks configured for it.
func verifyService(ctx context.Context, rd RunDeps, cfg *config.Config, cc docker.ComposeCtx, p *compose.Project, name string) VerifyResult {
	var v VerifyResult
	res, err := cc.Run(ctx, rd.Runner, docker.Opts{}, "ps", "-q", "--status", "running", name)
	ids := strings.Fields(string(res.Stdout))
	switch {
	case err != nil:
		v.Detail = err.Error()
		return v
	case len(ids) == 0:
		v.Detail = "no running container"
		return v
	}
	// A compose healthcheck was already waited for by `up --wait`.
	if svc := p.Services[name]; svc == nil || !svc.HasHealthcheck {
		if err := observeStable(ctx, rd, ids[0]); err != nil {
			v.Detail = err.Error()
			return v
		}
	}
	v.Healthy = true

	sc := cfg.Service(name)
	if len(sc.Smoke) == 0 {
		return v
	}
	env := smoke.Env{
		Service: name, Vars: cfg.Vars, Dir: cc.Dir, HTTP: rd.HTTP, Sleep: rd.Sleep,
		Addr: func(service string, port int) (string, error) { return publishedAddr(ctx, rd, cc, service, port) },
		Exec: func(ctx context.Context, service string, cmd []string) (string, error) {
			r, err := cc.Run(ctx, rd.Runner, docker.Opts{}, append([]string{"exec", "-T", service}, cmd...)...)
			return string(r.Stdout), err
		},
	}
	v.Smoke = rd.Smoke(ctx, env, sc.Smoke)
	return v
}

// observeStable watches a container for StableWindow: it has to stay running
// and must not restart.
func observeStable(ctx context.Context, rd RunDeps, container string) error {
	const step = 2 * time.Second
	baseline := -1
	for waited := time.Duration(0); ; waited += step {
		res, err := rd.Runner.Run(ctx, []string{"inspect", "--format", "{{.State.Running}} {{.RestartCount}}", container}, docker.Opts{})
		if err != nil {
			return fmt.Errorf("inspect container: %w", err)
		}
		f := strings.Fields(string(res.Stdout))
		if len(f) != 2 {
			return fmt.Errorf("inspect container: unexpected output %q", res.Stdout)
		}
		restarts, _ := strconv.Atoi(f[1])
		if f[0] != "true" {
			return errors.New("the container stopped")
		}
		if baseline < 0 {
			baseline = restarts
		} else if restarts > baseline {
			return fmt.Errorf("the container restarted %d time(s) in %s", restarts-baseline, waited)
		}
		if waited >= rd.StableWindow {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rd.Sleep(step)
	}
}

// publishedAddr resolves a service's container port to a host address using
// `docker compose port`.
func publishedAddr(ctx context.Context, rd RunDeps, cc docker.ComposeCtx, service string, port int) (string, error) {
	res, err := cc.Run(ctx, rd.Runner, docker.Opts{}, "port", service, strconv.Itoa(port))
	if err != nil {
		return "", fmt.Errorf("port %d of %s is not published: %w", port, service, err)
	}
	line := strings.TrimSpace(strings.SplitN(string(res.Stdout), "\n", 2)[0])
	i := strings.LastIndex(line, ":")
	if line == "" || i < 0 {
		return "", fmt.Errorf("port %d of %s is not published", port, service)
	}
	return "127.0.0.1" + line[i:], nil
}

// fingerprint hashes the given files (path and content) so that a later
// rollback can tell whether anyone edited them since the upgrade.
func fingerprint(paths []string) (string, error) {
	h := sha256.New()
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n%d\n", p, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// composeExec returns a pg.Exec that runs scripts in a compose service.
func composeExec(cc docker.ComposeCtx, r docker.Runner, service string) pg.Exec {
	return func(ctx context.Context, script string, in io.Reader, out io.Writer) (string, error) {
		res, err := cc.Run(ctx, r, docker.Opts{Stdin: in, Stdout: out}, "exec", "-T", service, "sh", "-c", script)
		return string(res.Stderr), err
	}
}

// reportT is the translator for reports in the configured language.
func reportT(cfg *config.Config) report.T { return i18n.T(cfg.Lang) }

// writeFile writes a private file.
func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }

// intSeconds renders a timeout for `--wait-timeout`.
func intSeconds(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }
