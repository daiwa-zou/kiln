package store

import (
	"context"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// The progress surface exists so a build can be watched while it runs. Before
// it, run items were written in the transaction that finished the run, so the
// only observable states were "running" and "over" -- a bench ingesting a large
// repository looked identical five seconds and five minutes in.
func TestRunItemsReportProgressWhileTheRunIsLive(t *testing.T) {
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

	plan := []diff.Key{"module:alpha", "module:beta", "doc:README.md"}
	if err := s.SeedRunItems(ctx, runID, plan, 0.12); err != nil {
		t.Fatalf("SeedRunItems: %v", err)
	}

	// The whole plan is visible before any of it has run: that is what makes
	// "what is still coming" answerable rather than a shrug.
	progress, err := s.RunProgressFor(ctx, ws, []string{runID})
	if err != nil {
		t.Fatal(err)
	}
	if got := progress[runID]; got.Total != 3 || got.Pending != 3 || got.Done != 0 {
		t.Errorf("after seeding: %+v, want 3 total all pending", got)
	}

	// One unit starts, and is distinguishable from the ones still queued.
	if err := s.MarkRunItem(ctx, runID, jobs.ItemSummary{
		Key: "module:alpha", Status: jobs.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if got := progress2(t, s, ws, runID); got.Running != 1 || got.Pending != 2 || got.Done != 0 {
		t.Errorf("mid-run: %+v, want 1 running, 2 pending", got)
	}

	// And then finishes, carrying its cost.
	if err := s.MarkRunItem(ctx, runID, jobs.ItemSummary{
		Key: "module:alpha", Status: jobs.StatusSucceeded, CostUSD: 0.25, Turns: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if got := progress2(t, s, ws, runID); got.Done != 1 || got.Running != 0 || got.Pending != 2 {
		t.Errorf("after one unit: %+v, want 1 done, 2 pending", got)
	}

	// Seeding is idempotent, which matters because a run can be re-planned:
	// re-seeding must not reset a unit that has already finished.
	if err := s.SeedRunItems(ctx, runID, plan, 0.12); err != nil {
		t.Fatal(err)
	}
	if got := progress2(t, s, ws, runID); got.Done != 1 || got.Total != 3 {
		t.Errorf("after re-seeding: %+v, want the finished unit left alone", got)
	}

	items, err := s.ListRunItems(ctx, ws, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("ListRunItems returned %d items, want 3", len(items))
	}
	// Unfinished units lead: while a run is live the reader's question is what
	// is happening and what is left, not what it cost.
	if items[0].Status == jobs.StatusSucceeded {
		t.Errorf("finished unit sorted first while units are still pending: %+v", items)
	}
	// The estimate the plan carried survives onto the item, so estimates stay
	// auditable against actuals.
	for _, it := range items {
		if it.EstCostUSD == nil || *it.EstCostUSD == 0 {
			t.Errorf("item %s lost its estimate", it.Key)
		}
	}
}

// A finished run must have no items still claiming to be in progress: a
// progress bar that never completes is worse than none. Units the run never
// reached are recorded as deferred and picked up by the next run.
func TestRecordRunSettlesUnreachedItems(t *testing.T) {
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

	plan := []diff.Key{"module:alpha", "module:beta", "module:gamma"}
	if err := s.SeedRunItems(ctx, runID, plan, 0.1); err != nil {
		t.Fatal(err)
	}
	// One finished, one was mid-flight when the run stopped, one never started.
	if err := s.MarkRunItem(ctx, runID, jobs.ItemSummary{
		Key: "module:alpha", Status: jobs.StatusSucceeded, CostUSD: 0.2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRunItem(ctx, runID, jobs.ItemSummary{
		Key: "module:beta", Status: jobs.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordRun(ctx, jobs.RunSummary{
		RunID: runID, WorkspaceID: ws, Trigger: "manual", Status: jobs.StatusOverBudget,
		CostUSD: 0.2,
		Items: []jobs.ItemSummary{
			{Key: "module:alpha", Status: jobs.StatusSucceeded, CostUSD: 0.2, EstCostUSD: 0.1},
		},
	}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	items, err := s.ListRunItems(ctx, ws, runID)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, it := range items {
		byKey[it.Key] = it.Status
	}
	if len(items) != 3 {
		t.Errorf("got %d items, want the whole plan recorded: %+v", len(items), byKey)
	}
	if byKey["module:alpha"] != jobs.StatusSucceeded {
		t.Errorf("module:alpha = %q, want %q", byKey["module:alpha"], jobs.StatusSucceeded)
	}
	for _, key := range []string{"module:beta", "module:gamma"} {
		if byKey[key] != jobs.StatusDeferred {
			t.Errorf("%s = %q on a finished run, want %q", key, byKey[key], jobs.StatusDeferred)
		}
	}

	progress, err := s.RunProgressFor(ctx, ws, []string{runID})
	if err != nil {
		t.Fatal(err)
	}
	if got := progress[runID]; got.Pending != 0 || got.Running != 0 || got.Done != 3 {
		t.Errorf("finished run still reports work in progress: %+v", got)
	}
}

// A CLI build has no run row until it ends, so there is no id to attach items
// to -- and nothing polling for them either. Seeding must no-op rather than
// error, or `kiln build` would fail on a run it could not report.
func TestSeedRunItemsIgnoresRunsWithoutARow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	if err := s.SeedRunItems(ctx, "run-bff86c822909b88f", []diff.Key{"module:alpha"}, 0.1); err != nil {
		t.Errorf("SeedRunItems on a CLI run id: %v", err)
	}
	if err := s.MarkRunItem(ctx, "run-bff86c822909b88f", jobs.ItemSummary{
		Key: "module:alpha", Status: jobs.StatusRunning,
	}); err != nil {
		t.Errorf("MarkRunItem on a CLI run id: %v", err)
	}
}

func progress2(t *testing.T, s *WikiStore, ws, runID string) RunProgress {
	t.Helper()
	p, err := s.RunProgressFor(context.Background(), ws, []string{runID})
	if err != nil {
		t.Fatal(err)
	}
	return p[runID]
}
