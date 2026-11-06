package smoke

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"composesafe/internal/config"
)

func TestHelperTransportDrivesTheHTTPCheck(t *testing.T) {
	var got []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\n\r\nok"), nil
	}
	env := Env{HTTP: &http.Client{Transport: HelperTransport(run)}, Sleep: func(time.Duration) {},
		Addr: func(s string, p int) (string, error) { return s + ":8080", nil }}
	rs := Run(t.Context(), env, []config.Smoke{{Name: "h", Type: "http", URL: `http://{{addr "app" 8080}}/healthz`,
		ExpectBodyContains: "ok", Timeout: 5 * time.Second}})
	if !AllOK(rs) {
		t.Fatalf("%+v", rs)
	}
	if got[len(got)-1] != "http://app:8080/healthz" || got[0] != "-sS" {
		t.Errorf("curl args: %v", got)
	}

	wrong := func(context.Context, ...string) ([]byte, error) {
		return []byte("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n"), nil
	}
	env.HTTP = &http.Client{Transport: HelperTransport(wrong)}
	rs = Run(t.Context(), env, []config.Smoke{{Name: "h", Type: "http", URL: "http://app:8080/", Timeout: time.Second}})
	if AllOK(rs) || !strings.Contains(rs[0].Detail, "503") {
		t.Errorf("a 503 must fail the check: %+v", rs)
	}

	down := func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("curl: (7) connection refused")
	}
	env.HTTP = &http.Client{Transport: HelperTransport(down)}
	if rs = Run(t.Context(), env, []config.Smoke{{Name: "h", Type: "http", URL: "http://app:8080/", Timeout: time.Second}}); AllOK(rs) {
		t.Error("an unreachable service must fail")
	}
}

func TestHelperDial(t *testing.T) {
	var got []string
	env := Env{Sleep: func(time.Duration) {}, Dial: HelperDial(func(_ context.Context, a ...string) ([]byte, error) { got = a; return nil, nil })}
	if rs := Run(t.Context(), env, []config.Smoke{{Name: "t", Type: "tcp", Addr: "db:5432", Timeout: 3 * time.Second}}); !AllOK(rs) {
		t.Fatalf("%+v", rs)
	}
	if got[len(got)-1] != "telnet://db:5432" {
		t.Errorf("args: %v", got)
	}
}
