package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/jobs"
)

func TestEnqueueDebouncesActiveRuns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	first, created, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil || !created {
		t.Fatalf("first enqueue: id=%q created=%v err=%v", first, created, err)
	}

	// A second enqueue while one is queued collapses into the waiting run.
	second, created, err := s.EnqueueRun(ctx, ws, "webhook", "")
	if err != nil {
		t.Fatal(err)
	}
	if created || second != first {
		t.Errorf("second enqueue: id=%q created=%v, want the existing run %q", second, created, first)
	}

	// Claiming moves it to running; enqueue must still debounce.
	claimed, err := s.ClaimNextRun(ctx, "w1")
	if err != nil || claimed == nil || claimed.ID != first {
		t.Fatalf("claim: %+v err=%v", claimed, err)
	}
	third, created, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	if created || third != first {
		t.Errorf("enqueue while running: id=%q created=%v, want debounce onto %q", third, created, first)
	}

	// A different workspace is unaffected by this one's active run.
	ws2, err := s.EnsureWorkspace(ctx, "local", "other", "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err = s.EnqueueRun(ctx, ws2, "manual", ""); err != nil || !created {
		t.Errorf("enqueue on second workspace: created=%v err=%v", created, err)
	}
}

func TestClaimIsExclusive(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	runID, _, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil {
		t.Fatal(err)
	}

	// The M2 verification case: two workers race for one queued run; exactly
	// one claim succeeds.
	const workers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
		errs []error
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			run, err := s.ClaimNextRun(ctx, "worker")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if run != nil {
				wins++
				if run.ID != runID {
					t.Errorf("claimed unexpected run %s", run.ID)
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("claim error: %v", err)
	}
	if wins != 1 {
		t.Errorf("claims = %d, want exactly 1", wins)
	}
}

func TestClaimReturnsWorkspaceContext(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "acme", "docs-bench", "Docs")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueRun(ctx, ws, "manual", ""); err != nil {
		t.Fatal(err)
	}

	run, err := s.ClaimNextRun(ctx, "w1")
	if err != nil || run == nil {
		t.Fatalf("claim: %+v err=%v", run, err)
	}
	if run.WorkspaceSlug != "docs-bench" || run.OrgSlug != "acme" {
		t.Errorf("workspace context: %+v", run)
	}
	if run.Trigger != "manual" {
		t.Errorf("trigger = %q", run.Trigger)
	}

	// Empty queue returns nil, not an error.
	again, err := s.ClaimNextRun(ctx, "w1")
	if err != nil || again != nil {
		t.Errorf("claim on empty queue: %+v err=%v", again, err)
	}
}

func TestRecordRunFinishesClaimedRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	runID, _, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx, "w1"); err != nil {
		t.Fatal(err)
	}

	// The pipeline reports with the database id as RunID: the queued row must
	// be finished in place, not duplicated.
	err = s.RecordRun(ctx, jobs.RunSummary{
		RunID: runID, WorkspaceID: ws, Trigger: "manual",
		Status: jobs.StatusSucceeded, CostUSD: 1.25, Created: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	var (
		count  int
		status string
		cost   float64
	)
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE workspace_id = $1`, ws).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("runs rows = %d, want 1 (finished in place)", count)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status, cost_usd FROM runs WHERE id = $1`, runID).Scan(&status, &cost); err != nil {
		t.Fatal(err)
	}
	if status != jobs.StatusSucceeded || cost != 1.25 {
		t.Errorf("finished run: status=%q cost=%v", status, cost)
	}

	// With the run finished, the workspace is free to queue again.
	next, created, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil || !created || next == runID {
		t.Errorf("enqueue after finish: id=%q created=%v err=%v", next, created, err)
	}
}

func TestRecordRunStillInsertsForCLIRunIDs(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	// CLI ids are "run-<hex>", not UUIDs: the insert path must be untouched.
	if err := s.RecordRun(ctx, jobs.RunSummary{
		RunID: jobs.NewRunID(), WorkspaceID: ws, Trigger: "manual",
		Status: jobs.StatusNoChanges,
	}); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE workspace_id = $1`, ws).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("runs rows = %d, want 1", count)
	}
}

func TestEnqueueRunOptsSchedulingAndRanges(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	// A run scheduled for the future is invisible to the claim query.
	runID, created, err := s.EnqueueRunOpts(ctx, ws, "webhook", "", EnqueueOptions{
		RefTo: "aaa111", NotBefore: time.Now().Add(time.Hour),
	})
	if err != nil || !created {
		t.Fatalf("enqueue: created=%v err=%v", created, err)
	}
	if run, err := s.ClaimNextRun(ctx, "w1"); err != nil || run != nil {
		t.Fatalf("claimed a not-yet-due run: %+v err=%v", run, err)
	}

	// A debounced push advances the waiting run's target head.
	same, created, err := s.EnqueueRunOpts(ctx, ws, "webhook", "", EnqueueOptions{RefTo: "bbb222"})
	if err != nil || created || same != runID {
		t.Fatalf("debounce: id=%q created=%v err=%v", same, created, err)
	}

	// Once due, the claim carries the newest head.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET not_before = now() - interval '1 second' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	run, err := s.ClaimNextRun(ctx, "w1")
	if err != nil || run == nil {
		t.Fatalf("claim: %+v err=%v", run, err)
	}
	if run.RefTo != "bbb222" {
		t.Errorf("refTo = %q, want the debounced push's head", run.RefTo)
	}
}

func TestLastSuccessfulRef(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	// No history yet: empty, not an error — the caller full-rebuilds.
	if ref, err := s.LastSuccessfulRef(ctx, ws); err != nil || ref != "" {
		t.Fatalf("empty history: %q %v", ref, err)
	}

	record := func(status, ref string) {
		t.Helper()
		if err := s.RecordRun(ctx, jobs.RunSummary{
			RunID: jobs.NewRunID(), WorkspaceID: ws, Trigger: "manual",
			Status: status, Ref: ref,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(jobs.StatusSucceeded, "abc1234")
	record(jobs.StatusFailed, "eee9999")  // failures contribute no baseline
	record(jobs.StatusPartial, "def5678") // partials do: their ref was built
	record(jobs.StatusNoChanges, "")      // no ref recorded

	ref, err := s.LastSuccessfulRef(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "def5678" {
		t.Errorf("last successful ref = %q, want the partial's def5678", ref)
	}
}

func TestFailRunAndRequeueStale(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	runID, _, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx, "w1"); err != nil {
		t.Fatal(err)
	}

	// FailRun releases the workspace.
	if err := s.FailRun(ctx, runID, "connector misconfigured"); err != nil {
		t.Fatal(err)
	}
	var status, msg string
	if err := pool.QueryRow(ctx,
		`SELECT status, error FROM runs WHERE id = $1`, runID).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != jobs.StatusFailed || msg != "connector misconfigured" {
		t.Errorf("failed run: status=%q error=%q", status, msg)
	}

	// A dead worker's claim is returned to the queue once stale.
	runID2, _, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx, "w-dead"); err != nil {
		t.Fatal(err)
	}
	// Fresh claims are not stale.
	if n, err := s.RequeueStaleRuns(ctx, time.Hour); err != nil || n != 0 {
		t.Errorf("requeue fresh: n=%d err=%v", n, err)
	}
	// Backdate the claim, then requeue.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET claimed_at = now() - interval '2 hours' WHERE id = $1`, runID2); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RequeueStaleRuns(ctx, time.Hour); err != nil || n != 1 {
		t.Errorf("requeue stale: n=%d err=%v", n, err)
	}
	reclaimed, err := s.ClaimNextRun(ctx, "w2")
	if err != nil || reclaimed == nil || reclaimed.ID != runID2 {
		t.Errorf("reclaim after requeue: %+v err=%v", reclaimed, err)
	}
}

func TestQueueDepthCountsUnfinishedRuns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	// An empty queue must report zeros rather than an empty map: a gauge that
	// stops being published reads as "no data", which an autoscaler and an
	// alert both treat differently from "nothing waiting".
	depth, err := s.QueueDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth["queued"] != 0 || depth["running"] != 0 {
		t.Fatalf("empty queue depth = %+v, want zeros", depth)
	}

	if _, _, err := s.EnqueueRun(ctx, ws, "manual", ""); err != nil {
		t.Fatal(err)
	}
	depth, _ = s.QueueDepth(ctx)
	if depth["queued"] != 1 {
		t.Errorf("queued = %d, want 1", depth["queued"])
	}

	// Claiming moves it from queued to running, which is what tells an
	// operator whether the backlog is stuck or merely deep.
	if _, err := s.ClaimNextRun(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}
	depth, _ = s.QueueDepth(ctx)
	if depth["queued"] != 0 || depth["running"] != 1 {
		t.Errorf("after claim = %+v, want running 1", depth)
	}
}
