// Package smoke runs the user's post-upgrade checks: an HTTP request, a TCP
// connection, a command on the host or a command inside a container. A
// check passes when one attempt succeeds; it fails when all attempts fail.
package smoke

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"composesafe/internal/config"
)

// Result is the outcome of one check.
type Result struct {
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	OK       bool          `json:"ok"`
	Attempts int           `json:"attempts"`
	Detail   string        `json:"detail,omitempty"` // why it failed (last attempt)
	Duration time.Duration `json:"duration"`
}

// Env is everything a check needs from its surroundings; tests replace it.
type Env struct {
	// Addr resolves {{addr "service" port}} to host:port.
	Addr func(service string, port int) (string, error)
	// Exec runs a command inside a service's container.
	Exec func(ctx context.Context, service string, cmd []string) (string, error)
	// Service is the service the checks belong to (used by exec checks).
	Service string
	// Vars are available as ${NAME}; the process environment is the fallback.
	Vars map[string]string
	// Dir is the working directory of command checks.
	Dir   string
	HTTP  *http.Client
	Sleep func(time.Duration)
	// Dial, when set, replaces the TCP connection attempt of tcp checks. A
	// rehearsal uses it to connect from inside the rehearsal network.
	Dial func(ctx context.Context, addr string, timeout time.Duration) error
}

// AllOK reports whether every result passed (vacuously true for none).
func AllOK(rs []Result) bool {
	for _, r := range rs {
		if !r.OK {
			return false
		}
	}
	return true
}

// Run runs the checks in order and returns one result for each.
func Run(ctx context.Context, env Env, checks []config.Smoke) []Result {
	if env.Sleep == nil {
		env.Sleep = time.Sleep
	}
	out := make([]Result, 0, len(checks))
	for i, c := range checks {
		name := c.Name
		if name == "" {
			name = fmt.Sprintf("%s-%d", c.Type, i+1)
		}
		r := Result{Name: name, Type: c.Type}
		start := time.Now()
		attempts := c.Retries
		if attempts < 1 {
			attempts = 1
		}
		var last error
		for a := 1; a <= attempts; a++ {
			r.Attempts = a
			if last = runOnce(ctx, env, c); last == nil {
				r.OK = true
				break
			}
			if ctx.Err() != nil {
				last = ctx.Err()
				break
			}
			if a < attempts {
				env.Sleep(c.Interval)
			}
		}
		if !r.OK && last != nil {
			r.Detail = last.Error()
		}
		r.Duration = time.Since(start)
		out = append(out, r)
	}
	return out
}

func runOnce(ctx context.Context, env Env, c config.Smoke) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	switch c.Type {
	case "http":
		u, err := expand(env, c.URL)
		if err != nil {
			return err
		}
		return httpCheck(ctx, env, u, c, timeout)
	case "tcp":
		a, err := expand(env, c.Addr)
		if err != nil {
			return err
		}
		if env.Dial != nil {
			return env.Dial(ctx, a, timeout)
		}
		conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", a)
		if err != nil {
			return err
		}
		return conn.Close()
	case "command", "exec":
		args := make([]string, len(c.Cmd))
		for i, a := range c.Cmd {
			x, err := expand(env, a)
			if err != nil {
				return err
			}
			args[i] = x
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if c.Type == "exec" {
			if env.Exec == nil {
				return errors.New("exec checks are not available here")
			}
			if _, err := env.Exec(cctx, env.Service, args); err != nil {
				return err
			}
			return nil
		}
		cmd := exec.CommandContext(cctx, args[0], args[1:]...) // never through a shell
		cmd.Dir = env.Dir
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, tail(buf.String()))
		}
		return nil
	}
	return fmt.Errorf("unknown check type %q", c.Type)
}

func httpCheck(ctx context.Context, env Env, url string, c config.Smoke, timeout time.Duration) error {
	client := env.HTTP
	if client == nil {
		client = &http.Client{}
	}
	// Do not follow redirects: a 302 to a login page is an answer worth
	// reporting as such, and expect_status can name it.
	cl := *client
	cl.Timeout = timeout
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	want := c.ExpectStatus
	if want == 0 {
		want = 200
	}
	if resp.StatusCode != want {
		return fmt.Errorf("%s answered %d, expected %d", url, resp.StatusCode, want)
	}
	if c.ExpectBodyContains != "" {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if !strings.Contains(string(b), c.ExpectBodyContains) {
			return fmt.Errorf("%s: the response does not contain %q", url, c.ExpectBodyContains)
		}
	}
	return nil
}

// expand resolves {{addr "svc" port}} and then ${VAR}.
func expand(env Env, s string) (string, error) {
	if strings.Contains(s, "{{") {
		t, err := template.New("").Option("missingkey=error").Funcs(template.FuncMap{
			"addr": func(service string, port int) (string, error) {
				if env.Addr == nil {
					return "", errors.New("addr is not available here")
				}
				return env.Addr(service, port)
			},
		}).Parse(s)
		if err != nil {
			return "", fmt.Errorf("template %q: %w", s, err)
		}
		var b bytes.Buffer
		if err := t.Execute(&b, nil); err != nil {
			return "", err
		}
		s = b.String()
	}
	var missing []string
	out := os.Expand(s, func(name string) string {
		if v, ok := env.Vars[name]; ok {
			return v
		}
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		missing = append(missing, name)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("variable ${%s} is not defined (vars: in composesafe.yaml, or the environment)", missing[0])
	}
	return out, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = "…" + s[len(s)-300:]
	}
	return s
}
