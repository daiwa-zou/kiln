// Command kiln builds and maintains knowledge bases from code repos, documents,
// and web sources.
//
// Three roles: `kiln serve` runs the HTTP API and the reading UI, `kiln build`
// runs the generation pipeline against a local directory, and `kiln worker`
// claims queued runs from the database and builds them through the same
// pipeline. `kiln serve --with-worker` runs server and worker in one process
// for single-node deployments.
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
