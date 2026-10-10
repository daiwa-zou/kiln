package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
)

// ledgerStore is the pipeline's store with the one behavior these tests turn on:
// recording a run finishes its row. Without it the double accepts a terminal
// write and a requeue for the same run, which the database does not -- RequeueRun
// is fenced to status 'running' -- and a test written against it would pass for
// a worker that strands every interrupted run.
type ledgerStore struct {
	*pipelineStore
	rows *loopStore
}

func (s *ledgerStore) RecordRun(ctx context.Context, run jobs.RunSummary) error {
	if err := s.pipelineStore.RecordRun(ctx, run); err != nil {
		return err
	}
	s.rows.settle(run.RunID, run.Status)
	return nil
}

// drainAt returns a context and a runner hook that cancels it as the nth agent
// call begins, which is the drain-grace deadline arriving mid-run. The fake
// runner's latency makes the call that follows observe the cancellation.
func drainAt(n int64) (context.Context, func(int64)) {
	ctx, cancel := context.WithCancel(context.Background())
	return ctx, func(call int64) {
		if call == n {
			cancel()
		}
	}
}

func TestResearchInterruptedByDrainIsRequeuedNotFailed(t *testing.T) {
	st := newLoopStore(&store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "research", ConnectorID: "c1",
	})
	st.question = &store.ResearchQuestion{ReviewID: "rv-1", Kind: "gap", Title: "Why is it slow?"}
	ps := &pipelineStore{}
	w, runner := loopWorker(t, st, &pipelineStore{}, agent.FakeOptions{CostPerCall: 0.02, Latency: 50 * time.Millisecond})
	// The pipeline records through a store that models the row, so a terminal
	// status written before the requeue is visible to it.
	w.Pipeline.Store = &ledgerStore{pipelineStore: ps, rows: st}
	w.Log, _ = bufferLogger()

	ctx, hook := drainAt(1)
	runner.onCall = hook

	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	// Back in the queue, to be researched by whichever worker is next -- not
	// finished as failed with the question never read.
	if got := st.rows["r1"].status; got != "queued" {
		t.Errorf("run status = %q, want queued; the interrupted research was stranded", got)
	}
	if len(st.requeued) != 1 || st.requeued[0] != "r1" {
		t.Errorf("requeued = %v, want [r1]", st.requeued)
	}
	if len(ps.runs) != 0 {
		t.Errorf("the pipeline recorded a terminal status for an interrupted pass: %+v", ps.runs)
	}
	if len(st.failed) != 0 {
		t.Errorf("interrupted research was failed: %v", st.failed)
	}
	// The item is left exactly as it was: the answer is still coming.
	if len(st.researched) != 0 {
		t.Errorf("the review item was annotated by an interrupted pass: %v", st.researched)
	}
	if !strings.Contains(scrapeMetrics(t, w), `kiln_runs_finished_total{status="requeued"} 1`) {
		t.Error("the interrupted run was not counted as requeued")
	}
}

func TestResearchFailureWithoutDrainStillRecordsFailedAndAnnotatesTheItem(t *testing.T) {
	// The drain exemption must not swallow a real failure: with no cancellation
	// the pipeline still ledgers 'failed' and the worker still tells the item.
	st := newLoopStore(&store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "research", ConnectorID: "c1",
	})
	st.question = &store.ResearchQuestion{ReviewID: "rv-1", Kind: "gap", Title: "Why is it slow?"}
	ps := &pipelineStore{pageErr: context.DeadlineExceeded}
	w, _ := loopWorker(t, st, &pipelineStore{}, agent.FakeOptions{CostPerCall: 0.02})
	w.Pipeline.Store = &ledgerStore{pipelineStore: ps, rows: st}
	w.Log, _ = bufferLogger()

	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(ps.runs) != 1 || ps.runs[0].Status != jobs.StatusFailed {
		t.Errorf("runs recorded = %+v, want one failed", ps.runs)
	}
	if st.rows["r1"].status != "failed" || len(st.requeued) != 0 {
		t.Errorf("row = %+v requeued=%v, want failed and not requeued", st.rows["r1"], st.requeued)
	}
	if len(st.researched) != 1 {
		t.Errorf("the failure was not written onto the item: %v", st.researched)
	}
}

func TestBuildInterruptedByDrainIsContinuedAndNotReportedSucceeded(t *testing.T) {
	st := newLoopStore(&store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "webhook", ConnectorID: "c1",
	})
	ps := &pipelineStore{}
	w, runner := loopWorker(t, st, &pipelineStore{}, agent.FakeOptions{CostPerCall: 0.02, Latency: 50 * time.Millisecond})
	w.Pipeline.Store = &ledgerStore{pipelineStore: ps, rows: st}
	w.Log, _ = bufferLogger()

	// The bench plans two units (the module and the architecture overview); the
	// deadline lands as the second agent call begins, partway through the first.
	ctx, hook := drainAt(2)
	runner.onCall = hook

	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(ps.runs) != 1 || ps.runs[0].Status != jobs.StatusCanceled {
		t.Fatalf("runs recorded = %+v, want one canceled (the setup must interrupt the build)", ps.runs)
	}
	// The run is terminal -- the pipeline imported what finished and recorded it
	// -- so requeueing it is not on the table; the rest of the work continues in
	// a run of its own, enqueued through a context that is no longer alive.
	if len(st.enqueued) != 1 || st.enqueued[0] != "continuation:c1" {
		t.Errorf("enqueued = %v, want one continuation for the unreached units", st.enqueued)
	}
	if len(st.failed) != 0 {
		t.Errorf("interrupted build was failed: %v", st.failed)
	}
	// No publish of a partial wiki on a dead context; the continuation's own
	// finish publishes the whole.
	if st.publishChecks != 0 {
		t.Errorf("a publish was attempted for an interrupted build (%d)", st.publishChecks)
	}
	out := scrapeMetrics(t, w)
	if strings.Contains(out, `kiln_runs_finished_total{status="succeeded"}`) {
		t.Errorf("an interrupted build was counted as succeeded:\n%s", out)
	}
	if !strings.Contains(out, `kiln_runs_finished_total{status="canceled"} 1`) {
		t.Errorf("the interrupted build was not counted as canceled:\n%s", out)
	}
	// The connector is stamped with what happened, through a live context.
	if got := st.marked["c1"]; !strings.Contains(got, "interrupted") {
		t.Errorf("connector sync note = %q, want the interruption", got)
	}
}

func TestBuildThatNeverStartedWorkIsRequeuedWhole(t *testing.T) {
	// A drain deadline that arrives before the pipeline produced anything is the
	// plain requeue the architecture doc promises: no import to keep, no
	// continuation to enqueue.
	st := newLoopStore(&store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "webhook", ConnectorID: "c1",
	})
	w, _ := loopWorker(t, st, &pipelineStore{}, agent.FakeOptions{CostPerCall: 0.02})
	w.Log, _ = bufferLogger()
	// The context is canceled as soon as the run is claimed, before source
	// resolution can finish. The path is outside the allowlist, so resolution
	// would otherwise fail the run, which is what tells the two outcomes apart.
	ctx, cancel := context.WithCancel(context.Background())
	st.pinned = &store.ConnectorRow{ID: "c1", Kind: "git", Name: "code",
		Config: map[string]any{"path": "/nonexistent-and-forbidden"}}

	run, err := st.ClaimNextRun(ctx, w.ID)
	if err != nil || run == nil {
		t.Fatal(err)
	}
	cancel()
	w.process(ctx, run)

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.rows["r1"].status != "queued" || len(st.requeued) != 1 {
		t.Errorf("row = %+v requeued=%v, want queued once", st.rows["r1"], st.requeued)
	}
	if len(st.enqueued) != 0 || len(st.failed) != 0 {
		t.Errorf("unexpected writes: enqueued=%v failed=%v", st.enqueued, st.failed)
	}
}
