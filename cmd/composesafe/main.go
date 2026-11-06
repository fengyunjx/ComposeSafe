package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"composesafe/internal/cli"
	"composesafe/internal/docker"
	"composesafe/internal/errs"
)

func main() {
	// Ctrl-C cancels the context; running docker commands are killed and the
	// run log keeps the unfinished step so `rollback --run` can pick up.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := cli.NewRoot(os.Stdout, os.Stderr, docker.ExecRunner{})
	if err := root.ExecuteContext(ctx); err != nil {
		if !errs.IsQuiet(err) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(errs.ExitCode(err))
	}
}
