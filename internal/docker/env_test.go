package docker_test

import (
	"errors"
	"strings"
	"testing"

	"composesafe/internal/docker"
	"composesafe/internal/docker/dockertest"
)

const localCtx = `[{"Name":"desktop-linux","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`
const remoteCtx = `[{"Name":"prod","Endpoints":{"docker":{"Host":"ssh://root@prod"}}}]`

func fake(ctxJSON, engine, compose string) *dockertest.Fake {
	return &dockertest.Fake{Responses: map[string]dockertest.Response{
		"context inspect":                      {Stdout: ctxJSON},
		"version --format {{.Server.Version}}": {Stdout: engine + "\n"},
		"compose version --short":              {Stdout: compose + "\n"},
	}}
}

func TestCheckEnvOK(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	info, err := docker.CheckEnv(t.Context(), fake(localCtx, "29.8.2", "5.5.0"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerVersion != "29.8.2" || info.ComposeVersion != "5.5.0" || info.Context != "desktop-linux" {
		t.Errorf("unexpected info %+v", info)
	}
}

func TestCheckEnvRejects(t *testing.T) {
	tests := []struct {
		name, host string
		f          *dockertest.Fake
		want       string
	}{
		{"remote context", "", fake(remoteCtx, "29.8.2", "5.5.0"), "not local"},
		{"DOCKER_HOST overrides local context", "tcp://10.0.0.5:2375", fake(localCtx, "29.8.2", "5.5.0"), "not local"},
		{"old engine", "", fake(localCtx, "26.1.0", "2.30.0"), "too old"},
		{"old compose", "", fake(localCtx, "27.0.1", "2.23.3"), "too old"},
		{"v-prefixed compose is parsed", "", fake(localCtx, "27.0.1", "v2.20.0-desktop.1"), "too old"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tc.host)
			_, err := docker.CheckEnv(t.Context(), tc.f)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCheckEnvRemoteIsCheckedBeforeDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	f := fake(remoteCtx, "", "")
	f.Responses["version --format {{.Server.Version}}"] = dockertest.Response{Err: errors.New("unreachable")}
	_, err := docker.CheckEnv(t.Context(), f)
	if err == nil || !strings.Contains(err.Error(), "not local") {
		t.Fatalf("remote must be reported first, got %v", err)
	}
	for _, c := range f.Calls {
		if c[0] == "version" {
			t.Error("daemon must not be contacted for a remote endpoint")
		}
	}
}
