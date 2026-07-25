// Command kiln builds and maintains knowledge bases from code repos, documents,
// and web sources.
//
// The same binary serves three roles: `kiln serve` runs the HTTP API and webhook
// receivers, `kiln worker` executes ingest and generation jobs from the queue, and
// the remaining commands are clients that talk to a server over its REST API.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed the error; exit non-zero without repeating it.
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
}
