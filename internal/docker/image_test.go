package docker_test

import (
	"errors"
	"testing"

	"composesafe/internal/docker"
	"composesafe/internal/docker/dockertest"
)

func TestParseSize(t *testing.T) {
	tests := map[string]int64{
		"0B": 0, "512B": 512, "1.5kB": 1500, "934.7MB": 934_700_000, "2.401GB": 2_401_000_000,
		"1TB": 1_000_000_000_000, " 3MB ": 3_000_000, "N/A": -1, "": -1, "12XB": -1,
	}
	for in, want := range tests {
		if got := docker.ParseSize(in); got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestInspectImage(t *testing.T) {
	key := "image inspect --format {{json .}} postgres:16"
	f := &dockertest.Fake{Responses: map[string]dockertest.Response{
		key: {Stdout: `{"Id":"sha256:abc","RepoDigests":["postgres@sha256:1"],"Config":{"Labels":{"org.opencontainers.image.source":"https://github.com/o/r"}}}`},
	}}
	info, err := docker.InspectImage(t.Context(), f, "postgres:16")
	if err != nil || info.ID != "sha256:abc" || len(info.RepoDigests) != 1 || info.Labels["org.opencontainers.image.source"] == "" {
		t.Fatalf("%+v %v", info, err)
	}

	// not present locally: (nil, nil), never an error and never a pull
	f = &dockertest.Fake{Responses: map[string]dockertest.Response{
		key: {Stderr: "Error response from daemon: No such image: postgres:16", Err: errors.New("exit 1")},
	}}
	if info, err := docker.InspectImage(t.Context(), f, "postgres:16"); info != nil || err != nil {
		t.Errorf("missing image: %v %v", info, err)
	}

	// any other failure is reported
	f = &dockertest.Fake{Responses: map[string]dockertest.Response{
		key: {Stderr: "Cannot connect to the Docker daemon", Err: errors.New("exit 1")},
	}}
	if _, err := docker.InspectImage(t.Context(), f, "postgres:16"); err == nil {
		t.Error("daemon errors must surface")
	}
}

func TestVolumeSizes(t *testing.T) {
	f := &dockertest.Fake{Responses: map[string]dockertest.Response{
		"system df -v --format json": {Stdout: `{"Images":[],"Volumes":[{"Name":"a","Size":"1.5GB"},{"Name":"b","Size":"N/A"}]}`},
	}}
	got, err := docker.VolumeSizes(t.Context(), f)
	if err != nil || got["a"] != 1_500_000_000 || got["b"] != -1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestRegistryMirrors(t *testing.T) {
	key := "info --format {{json .RegistryConfig.Mirrors}}"
	f := &dockertest.Fake{Responses: map[string]dockertest.Response{
		key: {Stdout: `["https://docker.m.daocloud.io/","http://10.0.0.5:5000"]`},
	}}
	got := docker.RegistryMirrors(t.Context(), f)
	if len(got) != 2 || got[0] != "docker.m.daocloud.io" || got[1] != "10.0.0.5:5000" {
		t.Errorf("got %v", got)
	}
	f.Responses[key] = dockertest.Response{Stdout: "null"}
	if got := docker.RegistryMirrors(t.Context(), f); len(got) != 0 {
		t.Errorf("null: %v", got)
	}
	f.Responses[key] = dockertest.Response{Err: errors.New("x")}
	if got := docker.RegistryMirrors(t.Context(), f); got != nil {
		t.Errorf("error must give no mirrors, got %v", got)
	}
}
