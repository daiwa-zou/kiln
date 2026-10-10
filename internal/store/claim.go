package store

import (
	"context"
	"errors"
)

// ErrClaimLost reports that a write made on behalf of a claimed run matched no
// row, because the run is no longer this worker's: it was requeued as stale and
// claimed again, or already finished by someone else.
//
// It is an error rather than a silent no-op because a worker that carries on as
// though its write landed is exactly the failure fencing exists to stop. A
// caller decides what a lost claim means for it -- usually that it should stop
// spending -- but it must never be allowed to look like success.
var ErrClaimLost = errors.New("store: run is no longer claimed by this worker")

// claim marks a context as belonging to the worker that holds a run's claim.
type claim struct{ workerID string }

type claimKey struct{}

// WithClaim returns a context whose run writes are fenced to workerID.
//
// The pipeline reaches the store through jobs.Store, an interface the CLI
// shares and whose RecordRun and MarkRunItem have no notion of a worker. Rather
// than widen it and every fake behind it, the worker states once, at the top of
// the run, who it is; every later write on that context -- including the ones
// made on context.WithoutCancel of it -- carries the fence. A context without
// the mark (the CLI, and tests of the pipeline) writes unfenced, as it always
// did.
func WithClaim(ctx context.Context, workerID string) context.Context {
	return context.WithValue(ctx, claimKey{}, claim{workerID: workerID})
}

// claimedBy returns the fencing worker id, and whether the context has one. The
// presence flag is separate from the id because an empty id is still a claim:
// ClaimNextRun stores it verbatim.
func claimedBy(ctx context.Context) (string, bool) {
	c, ok := ctx.Value(claimKey{}).(claim)
	return c.workerID, ok
}
