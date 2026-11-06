package smoke

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"composesafe/internal/config"
)

func env() Env { return Env{Sleep: func(time.Duration) {}, Service: "app", Dir: "."} }

func one(t *testing.T, e Env, c config.Smoke) Result {
	t.Helper()
	rs := Run(t.Context(), e, []config.Smoke{c})
	if len(rs) != 1 {
		t.Fatalf("results: %v", rs)
	}
	return rs[0]
}

func TestHTTP(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("hello world"))
		case "/flaky":
			if atomic.AddInt32(&hits, 1) < 3 {
				w.WriteHeader(503)
				return
			}
			w.Write([]byte("up"))
		case "/login-redirect":
			http.Redirect(w, r, "/login", http.StatusFound)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	if r := one(t, env(), config.Smoke{Name: "ok", Type: "http", URL: srv.URL + "/ok", ExpectStatus: 200, Retries: 1}); !r.OK || r.Attempts != 1 {
		t.Errorf("ok: %+v", r)
	}
	if r := one(t, env(), config.Smoke{Type: "http", URL: srv.URL + "/ok", ExpectBodyContains: "world", Retries: 1}); !r.OK {
		t.Errorf("body: %+v", r)
	}
	r := one(t, env(), config.Smoke{Type: "http", URL: srv.URL + "/ok", ExpectBodyContains: "nope", Retries: 1})
	if r.OK || !strings.Contains(r.Detail, `does not contain "nope"`) {
		t.Errorf("body mismatch: %+v", r)
	}
	r = one(t, env(), config.Smoke{Type: "http", URL: srv.URL + "/missing", ExpectStatus: 200, Retries: 1})
	if r.OK || !strings.Contains(r.Detail, "answered 500") {
		t.Errorf("status: %+v", r)
	}
	if r := one(t, env(), config.Smoke{Type: "http", URL: srv.URL + "/login-redirect", ExpectStatus: 302, Retries: 1}); !r.OK {
		t.Errorf("redirects are not followed, so 302 can be expected: %+v", r)
	}
	// retries: fails twice, passes on the third attempt
	r = one(t, env(), config.Smoke{Type: "http", URL: srv.URL + "/flaky", ExpectStatus: 200, Retries: 5, Interval: time.Second})
	if !r.OK || r.Attempts != 3 {
		t.Errorf("flaky: %+v", r)
	}
	r = one(t, env(), config.Smoke{Type: "http", URL: srv.URL + "/missing", Retries: 2, Interval: time.Second})
	if r.OK || r.Attempts != 2 {
		t.Errorf("gives up after Retries attempts: %+v", r)
	}
	if r.Name != "http-1" {
		t.Errorf("unnamed checks get a generated name, got %q", r.Name)
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if r := one(t, env(), config.Smoke{Type: "tcp", Addr: ln.Addr().String(), Retries: 1}); !r.OK {
		t.Errorf("%+v", r)
	}
	ln.Close()
	if r := one(t, env(), config.Smoke{Type: "tcp", Addr: ln.Addr().String(), Retries: 1}); r.OK {
		t.Errorf("closed port must fail: %+v", r)
	}
}

func TestCommandNeverUsesAShell(t *testing.T) {
	if r := one(t, env(), config.Smoke{Type: "command", Cmd: []string{"true"}, Retries: 1}); !r.OK {
		t.Errorf("%+v", r)
	}
	r := one(t, env(), config.Smoke{Type: "command", Cmd: []string{"sh", "-c", "echo broken >&2; exit 3"}, Retries: 1})
	if r.OK || !strings.Contains(r.Detail, "broken") || !strings.Contains(r.Detail, "exit status 3") {
		t.Errorf("%+v", r)
	}
	// shell metacharacters in an argument are data
	r = one(t, env(), config.Smoke{Type: "command", Cmd: []string{"test", "x;y", "=", "x;y"}, Retries: 1})
	if !r.OK {
		t.Errorf("args must reach the program untouched: %+v", r)
	}
	if r := one(t, env(), config.Smoke{Type: "command", Cmd: []string{"definitely-not-installed-zzz"}, Retries: 1}); r.OK {
		t.Error("missing program")
	}
}

func TestExec(t *testing.T) {
	var gotSvc string
	var gotCmd []string
	e := env()
	e.Exec = func(_ context.Context, svc string, cmd []string) (string, error) {
		gotSvc, gotCmd = svc, cmd
		return "", nil
	}
	if r := one(t, e, config.Smoke{Type: "exec", Cmd: []string{"app", "--check", "${MODE}"}, Retries: 1}); r.OK {
		t.Error("undefined variable must fail the check")
	}
	e.Vars = map[string]string{"MODE": "full"}
	if r := one(t, e, config.Smoke{Type: "exec", Cmd: []string{"app", "--check", "${MODE}"}, Retries: 1}); !r.OK || gotSvc != "app" || strings.Join(gotCmd, " ") != "app --check full" {
		t.Errorf("svc=%q cmd=%v", gotSvc, gotCmd)
	}
	e.Exec = func(context.Context, string, []string) (string, error) { return "", errors.New("exit 1") }
	if r := one(t, e, config.Smoke{Type: "exec", Cmd: []string{"x"}, Retries: 1}); r.OK || r.Detail != "exit 1" {
		t.Errorf("%+v", r)
	}
	e.Exec = nil
	if r := one(t, e, config.Smoke{Type: "exec", Cmd: []string{"x"}, Retries: 1}); r.OK {
		t.Error("exec without a runner must fail, not pass")
	}
}

func TestAddrAndVars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	e := env()
	e.Addr = func(svc string, port int) (string, error) {
		if svc != "app" || port != 8080 {
			return "", errors.New("unexpected")
		}
		return host, nil
	}
	e.Vars = map[string]string{"PATH_PART": "health"}
	t.Setenv("SMOKE_TEST_ENV_VAR", "")
	c := config.Smoke{Type: "http", URL: `http://{{addr "app" 8080}}/${PATH_PART}`, ExpectStatus: 200, Retries: 1}
	if r := one(t, e, c); !r.OK {
		t.Errorf("%+v", r)
	}
	// the environment is the fallback for variables
	t.Setenv("SMOKE_TEST_PATH", "health")
	c.URL = `http://{{addr "app" 8080}}/${SMOKE_TEST_PATH}`
	if r := one(t, e, c); !r.OK {
		t.Errorf("env fallback: %+v", r)
	}
	// a resolver error is a failed check with a reason
	c.URL = `http://{{addr "other" 1}}/`
	if r := one(t, e, c); r.OK || !strings.Contains(r.Detail, "unexpected") {
		t.Errorf("%+v", r)
	}
	c.URL = `http://{{addr "app"`
	if r := one(t, e, c); r.OK || !strings.Contains(r.Detail, "template") {
		t.Errorf("bad template: %+v", r)
	}
	e.Addr = nil
	c.URL = `http://{{addr "app" 8080}}/`
	if r := one(t, e, c); r.OK {
		t.Error("addr without a resolver must fail")
	}
}

func TestAllOKAndCancellation(t *testing.T) {
	if !AllOK(nil) || AllOK([]Result{{OK: true}, {OK: false}}) {
		t.Error("AllOK")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rs := Run(ctx, env(), []config.Smoke{{Type: "command", Cmd: []string{"true"}, Retries: 50, Interval: time.Hour}})
	if rs[0].OK && rs[0].Attempts > 1 {
		t.Errorf("a cancelled context must stop retrying: %+v", rs[0])
	}
}
