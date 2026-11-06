// Package dockertest provides a fake docker.Runner for unit tests.
package dockertest

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"composesafe/internal/docker"
)

// Response is a canned result for one command line.
type Response struct {
	Stdout string
	Stderr string
	Err    error
}

// Fake answers commands from a map keyed by the space-joined arguments, or
// from Handler for commands that need to look at the arguments or stdin. It
// records every call so tests can assert on the command sequence.
type Fake struct {
	Responses map[string]Response
	// Handler is consulted first; it returns handled=false to fall through
	// to Responses.
	Handler func(args []string, o docker.Opts) (resp Response, handled bool)

	mu    sync.Mutex
	Calls [][]string
}

func (f *Fake) Run(_ context.Context, args []string, o docker.Opts) (docker.Result, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, args)
	f.mu.Unlock()

	var r Response
	handled := false
	if f.Handler != nil {
		r, handled = f.Handler(args, o)
	}
	if !handled {
		var ok bool
		key := strings.Join(args, " ")
		if r, ok = f.Responses[key]; !ok {
			return docker.Result{}, fmt.Errorf("dockertest: unexpected command: docker %s", key)
		}
	}
	if o.Stdout != nil {
		_, _ = o.Stdout.Write([]byte(r.Stdout))
	}
	res := docker.Result{Stdout: []byte(r.Stdout), Stderr: []byte(r.Stderr)}
	if r.Err != nil {
		return res, &docker.CmdError{Args: args, ExitCode: 1, Stderr: r.Stderr, Err: r.Err}
	}
	return res, nil
}

// Lines returns the recorded calls as space-joined strings.
func (f *Fake) Lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}
