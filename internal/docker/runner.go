// Package docker is the only place that executes the docker CLI.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Opts customises one command invocation.
type Opts struct {
	Dir    string    // working directory
	Stdin  io.Reader // optional
	Stdout io.Writer // optional: stream stdout here instead of buffering it
	Env    []string  // extra KEY=VALUE entries appended to the process environment
}

// Result is the outcome of a finished command.
type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
}

// Runner runs docker with the given arguments. Production code uses
// ExecRunner; tests substitute a fake and assert on the arguments.
type Runner interface {
	Run(ctx context.Context, args []string, o Opts) (Result, error)
}

// CmdError is returned when the command could not run or exited non-zero.
type CmdError struct {
	Args     []string
	ExitCode int
	Stderr   string // last lines only
	Err      error
}

func (e *CmdError) Error() string {
	msg := fmt.Sprintf("docker %s: ", strings.Join(e.Args, " "))
	if e.ExitCode > 0 {
		msg += fmt.Sprintf("exit status %d", e.ExitCode)
	} else if e.Err != nil {
		msg += e.Err.Error()
	}
	if e.Stderr != "" {
		msg += "\n" + e.Stderr
	}
	return msg
}

func (e *CmdError) Unwrap() error { return e.Err }

// ExecRunner runs the real docker binary.
type ExecRunner struct {
	Bin string // defaults to "docker"
}

func (r ExecRunner) Run(ctx context.Context, args []string, o Opts) (Result, error) {
	bin := r.Bin
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = o.Dir
	cmd.Stdin = o.Stdin
	if len(o.Env) > 0 {
		cmd.Env = append(cmd.Environ(), o.Env...)
	}
	var stdout, stderr bytes.Buffer
	if o.Stdout != nil {
		cmd.Stdout = o.Stdout
	} else {
		cmd.Stdout = &stdout
	}
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return res, nil
	}
	ce := &CmdError{Args: args, Stderr: tailLines(stderr.String(), 20), Err: err}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		ce.ExitCode = res.ExitCode
	}
	return res, ce
}

// Compose builds the arguments for a `docker compose` invocation.
func Compose(project string, files []string, rest ...string) []string {
	args := []string{"compose"}
	if project != "" {
		args = append(args, "-p", project)
	}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	return append(args, rest...)
}

func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
