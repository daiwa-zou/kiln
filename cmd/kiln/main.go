// Command kiln builds and maintains knowledge bases from code repos, documents,
// and web sources.
//
// Two roles exist today: `kiln serve` runs the HTTP API and the reading UI, and
// `kiln build` runs the generation pipeline against a local directory. A
// queue-backed `kiln worker` role is planned but not yet implemented; when it
// lands, builds will also be schedulable server-side.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Every command runs under a signal-aware context, so Ctrl-C during a
	// build cancels the pipeline cleanly -- completed units import, temp
	// directories are removed by their defers -- instead of hard-killing the
	// process mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		// Cobra has already printed the error; exit non-zero without repeating it.
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
}
