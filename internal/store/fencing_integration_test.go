package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// claimedRun enqueues and claims one run for workerID, returning its id.
func claimedRun(t *testing.T, s *WikiStore, ws, workerID string) string {
	t.Helper()
	ctx := context.Background()
	id, _, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimNextRun(ctx, workerID)
	if err != nil || got == nil || got.ID != id {
		t.Fatalf("claim by %s: %+v err=%v", workerID, got, err)
	}
	return id
}

func backdateClaim(t *testing.T, pool *pgxpool.Pool, runID string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE runs SET claimed_at = now() - make_interval(secs => $2) WHERE id = $1`,
		runID, age.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatKeepsALongRunFromGoingStale(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	runID := claimedRun(t, s, ws, "w1")

	// The build has been going two hours without a refresh: past the deadline.
	backdateClaim(t, pool, runID, 2*time.Hour)

	if err := s.HeartbeatRun(ctx, runID, "w1"); err != nil {
		t.Fatalf("heartbeat by the owner: %v", err)
	}
	// Alive, so the sweep must leave it for its worker. This is the double-build
	// the heartbeat exists to prevent.
	stale, err := s.RequeueStaleRuns(ctx, time.Hour)
	if err != nil || len(stale) != 0 {
		t.Fatalf("a heartbeating run was swept: %+v err=%v", stale, err)
	}
	if got, err := s.ClaimNextRun(ctx, "w2"); err != nil || got != nil {
		t.Fatalf("a second worker claimed a live run: %+v err=%v", got, err)
	}
}

func TestHeartbeatByANonOwnerChangesNothing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	runID := claimedRun(t, s, ws, "w1")
	backdateClaim(t, pool, runID, 2*time.Hour)

	if err := s.HeartbeatRun(ctx, runID, "w2"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("heartbeat by a non-owner = %v, want ErrClaimLost", err)
	}
	// The non-owner must not have kept the owner's claim alive on its behalf.
	stale, err := s.RequeueStaleRuns(ctx, time.Hour)
	if err != nil || len(stale) != 1 {
		t.Fatalf("a non-owner's heartbeat refreshed the claim: %+v err=%v", stale, err)
	}
	// And once the run is back in the queue the old owner cannot beat it either.
	if err := s.HeartbeatRun(ctx, runID, "w1"); !errors.Is(err, ErrClaimLost) {
		t.Errorf("heartbeat on a requeued run = %v, want ErrClaimLost", err)
	}
}

func TestRequeueStaleReportsWhichRunsAndWhoHeldThem(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	runID := claimedRun(t, s, ws, "w-dead")
	backdateClaim(t, pool, runID, 2*time.Hour)

	stale, err := s.RequeueStaleRuns(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != runID || stale[0].WorkspaceID != ws ||
		stale[0].PreviousClaimedBy != "w-dead" {
		t.Errorf("stale = %+v, want the run, its workspace and w-dead", stale)
	}
	var status string
	var owner *string
	if err := pool.QueryRow(ctx,
		`SELECT status, claimed_by FROM runs WHERE id = $1`, runID).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || owner != nil {
		t.Errorf("requeued row: status=%q claimed_by=%v", status, owner)
	}
}

// The scenario the whole change is about: w1 is cut off, its run goes stale and
// w2 takes it over, then w1 comes back and tries to finish what it started.
func TestFencedWritesFromANonOwnerDoNotChangeTheRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	runID := claimedRun(t, s, ws, "w1")
	backdateClaim(t, pool, runID, 2*time.Hour)
	if stale, err := s.RequeueStaleRuns(ctx, time.Hour); err != nil || len(stale) != 1 {
		t.Fatalf("setup sweep: %+v err=%v", stale, err)
	}
	if got, err := s.ClaimNextRun(ctx, "w2"); err != nil || got == nil || got.ID != runID {
		t.Fatalf("setup takeover: %+v err=%v", got, err)
	}

	type rowState struct {
		status, owner string
		cost          float64
		finished      bool
	}
	snapshot := func() rowState {
		var r rowState
		if err := pool.QueryRow(ctx, `
			SELECT status, coalesce(claimed_by, ''), cost_usd, finished_at IS NOT NULL
			FROM runs WHERE id = $1`, runID).Scan(&r.status, &r.owner, &r.cost, &r.finished); err != nil {
			t.Fatal(err)
		}
		return r
	}
	want := rowState{status: "running", owner: "w2"}
	if got := snapshot(); got != want {
		t.Fatalf("setup state = %+v, want %+v", got, want)
	}

	w1 := WithClaim(ctx, "w1")

	// The pipeline's own writes, fenced through the context.
	if err := s.MarkRunItem(w1, runID, jobs.ItemSummary{
		Key: "module:a", Status: jobs.StatusSucceeded, CostUSD: 0.3}); !errors.Is(err, ErrClaimLost) {
		t.Errorf("MarkRunItem by a non-owner = %v, want ErrClaimLost", err)
	}
	if err := s.SeedRunItems(w1, runID, []diff.Key{"module:b"}, 0.1); err != nil {
		t.Errorf("SeedRunItems is best-effort and must not error: %v", err)
	}
	err = s.RecordRun(w1, jobs.RunSummary{RunID: runID, WorkspaceID: ws, Trigger: "manual",
		Status: jobs.StatusSucceeded, CostUSD: 1.25, Created: 4,
		Items:   []jobs.ItemSummary{{Key: "module:a", Status: jobs.StatusSucceeded, CostUSD: 1.25}},
		Reviews: []jobs.ReviewNote{{Kind: "gap", Title: "a loser's question", Detail: "d"}}})
	if !errors.Is(err, ErrClaimLost) {
		t.Errorf("RecordRun by a non-owner = %v, want ErrClaimLost", err)
	}

	// The worker-facing writes, fenced by argument.
	if err := s.FailRun(ctx, runID, "w1", "boom"); !errors.Is(err, ErrClaimLost) {
		t.Errorf("FailRun by a non-owner = %v, want ErrClaimLost", err)
	}
	if err := s.RequeueRun(ctx, runID, "w1"); !errors.Is(err, ErrClaimLost) {
		t.Errorf("RequeueRun by a non-owner = %v, want ErrClaimLost", err)
	}

	// None of it touched the row, its items, or the review queue.
	if got := snapshot(); got != want {
		t.Errorf("a non-owner changed the run: %+v, want %+v", got, want)
	}
	var items, reviews, ledgerRows int
	var ledgered float64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_items WHERE run_id = $1`, runID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM review_items WHERE workspace_id = $1`, ws).Scan(&reviews); err != nil {
		t.Fatal(err)
	}
	if items != 0 || reviews != 0 {
		t.Errorf("a non-owner wrote progress: run_items=%d review_items=%d", items, reviews)
	}

	// The one thing that must survive: w1 really did spend that money, and the
	// budget window has to see it.
	if err := pool.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(amount_usd), 0) FROM spend_ledger WHERE run_id = $1`,
		runID).Scan(&ledgerRows, &ledgered); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 1 || ledgered != 1.25 {
		t.Errorf("lost run's spend: rows=%d total=%v, want the 1.25 still ledgered", ledgerRows, ledgered)
	}

	// The active-run slot was not freed for a third run to start beside w2's.
	again, created, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil || created || again != runID {
		t.Errorf("enqueue after a lost finish: id=%q created=%v err=%v; the slot must stay w2's", again, created, err)
	}

	// The real owner is unaffected by all of the above and finishes normally.
	w2 := WithClaim(ctx, "w2")
	if err := s.MarkRunItem(w2, runID, jobs.ItemSummary{
		Key: "module:a", Status: jobs.StatusSucceeded, CostUSD: 0.4}); err != nil {
		t.Fatalf("MarkRunItem by the owner: %v", err)
	}
	if err := s.RecordRun(w2, jobs.RunSummary{RunID: runID, WorkspaceID: ws, Trigger: "manual",
		Status: jobs.StatusSucceeded, CostUSD: 0.4}); err != nil {
		t.Fatalf("RecordRun by the owner: %v", err)
	}
	if got := snapshot(); got.status != jobs.StatusSucceeded || got.cost != 0.4 || !got.finished {
		t.Errorf("owner's finish = %+v", got)
	}
}

func TestUnfencedRecordRunStillFinishesAnyRow(t *testing.T) {
	// The CLI and the API tests carry no claim, and must keep writing as they
	// always did: fencing is the worker's opt-in, not a new precondition.
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	runID := claimedRun(t, s, ws, "w1")

	if err := s.RecordRun(ctx, jobs.RunSummary{RunID: runID, WorkspaceID: ws,
		Trigger: "manual", Status: jobs.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != jobs.StatusSucceeded {
		t.Errorf("unfenced RecordRun left status %q", status)
	}
}
