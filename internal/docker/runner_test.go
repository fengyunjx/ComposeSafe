package docker_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"composesafe/internal/docker"
)

// ExecRunner is exercised with `sh` standing in for the docker binary, so
// the test needs no Docker daemon.
func TestExecRunner(t *testing.T) {
	r := docker.ExecRunner{Bin: "sh"}

	res, err := r.Run(t.Context(), []string{"-c", "echo out; echo err >&2"}, docker.Opts{})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "out" || strings.TrimSpace(string(res.Stderr)) != "err" {
		t.Fatalf("buffered: %q %q %v", res.Stdout, res.Stderr, err)
	}

	var sink bytes.Buffer
	res, err = r.Run(t.Context(), []string{"-c", "echo streamed"}, docker.Opts{Stdout: &sink})
	if err != nil || strings.TrimSpace(sink.String()) != "streamed" || len(res.Stdout) != 0 {
		t.Fatalf("streaming must bypass the buffer: sink=%q buf=%q err=%v", sink.String(), res.Stdout, err)
	}

	res, err = r.Run(t.Context(), []string{"-c", "echo $CS_TEST_VAR"}, docker.Opts{Env: []string{"CS_TEST_VAR=hello"}})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "hello" {
		t.Fatalf("env: %q %v", res.Stdout, err)
	}

	res, err = r.Run(t.Context(), []string{"-c", "echo boom >&2; exit 3"}, docker.Opts{})
	var ce *docker.CmdError
	if !errors.As(err, &ce) || ce.ExitCode != 3 || res.ExitCode != 3 || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("exit error: %v", err)
	}
}

func TestStderrKeepsLastLinesOnly(t *testing.T) {
	r := docker.ExecRunner{Bin: "sh"}
	_, err := r.Run(t.Context(), []string{"-c", "for i in $(seq 1 50); do echo line$i >&2; done; exit 1"}, docker.Opts{})
	msg := err.Error()
	if strings.Contains(msg, "line1\n") || !strings.Contains(msg, "line50") {
		t.Errorf("expected only the tail of stderr:\n%s", msg)
	}
}

func TestMissingBinary(t *testing.T) {
	_, err := docker.ExecRunner{Bin: "definitely-not-a-binary"}.Run(t.Context(), nil, docker.Opts{})
	var ce *docker.CmdError
	if !errors.As(err, &ce) || ce.ExitCode != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestComposeArgs(t *testing.T) {
	got := strings.Join(docker.Compose("p", []string{"a.yml", "b.yml"}, "config"), " ")
	if got != "compose -p p -f a.yml -f b.yml config" {
		t.Errorf("got %q", got)
	}
	if got := strings.Join(docker.Compose("", nil, "ps"), " "); got != "compose ps" {
		t.Errorf("got %q", got)
	}
}
