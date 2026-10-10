package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
)

// countingRunner counts agent calls, which is the pipeline's whole footprint:
// a run that never reached the pipeline made none.
type countingRunner struct {
	inner agent.Runner
	calls atomic.Int64
	// onCall, when set, runs as call n begins and before it does any work: how a
	// test places a drain deadline at an exact point in a run.
	onCall func(n int64)
}

func (c *countingRunner) Run(ctx context.Context, req agent.Request) (*agent.Result, error) {
	n := c.calls.Add(1)
	if c.onCall != nil {
		c.onCall(n)
	}
	return c.inner.Run(ctx, req)
}

// loopWorker wires a worker over loopStore -- which models run status and
// ownership -- whose bench is one small repository pinned as connector c1, with
// the free fake agent behind a call counter.
func loopWorker(t *testing.T, st *loopStore, ps *pipelineStore, fake agent.FakeOptions) (*Worker, *countingRunner) {
	t.Helper()

	repo := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod":  "module example.com/demo\n\ngo 1.25\n",
		"main.go": "package main\n\n// Dispatch hands work to workers.\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st.pinned = &store.ConnectorRow{
		ID: "c1", Kind: "git", Name: "code", Config: map[string]any{"path": repo},
	}

	runner := &countingRunner{inner: agent.NewFakeRunner(fake)}
	return &Worker{
		Store: st,
		ID:    "w1",
		Pipeline: &jobs.Pipeline{
			Store:  ps,
			Runner: runner,
			Budget: jobs.Budget{AnalyzeUSD: 0.4, PageUSD: 1, RunUSD: 4},
			Model:  "fake",
		},
		PermittedSourceRoots: []string{repo},
	}, runner
}

func TestClaimedRunInAnOverBudgetWorkspaceIsRefusedWithoutBuilding(t *testing.T) {
	// A webhook, poll or continuation run never passed the API's budget check, so
	// the claim is where an exhausted window has to stop it.
	for _, trigger := range []string{"webhook", "poll", "continuation", "manual"} {
		t.Run(trigger, func(t *testing.T) {
			budget := 10.0
			st := newLoopStore()
			st.budget, st.spent = &budget, 10.0
			ps := &pipelineStore{}
			w, runner := loopWorker(t, st, ps, agent.FakeOptions{CostPerCall: 0.02})
			w.BudgetWindow = time.Hour
			w.Log, _ = bufferLogger()

			w.process(context.Background(), &store.QueuedRun{
				ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench",
				Trigger: trigger, ConnectorID: "c1",
			})

			st.mu.Lock()
			defer st.mu.Unlock()
			if runner.calls.Load() != 0 || len(ps.runs) != 0 {
				t.Errorf("the pipeline ran for an over-budget workspace: calls=%d runs=%+v",
					runner.calls.Load(), ps.runs)
			}
			if st.rows["r1"].status != "over_budget" {
				t.Errorf("run status = %q, want over_budget", st.rows["r1"].status)
			}
			if !strings.Contains(st.refused["r1"], "budget window exceeded") {
				t.Errorf("refusal reason = %q", st.refused["r1"])
			}
			if len(st.failed) != 0 || len(st.requeued) != 0 {
				t.Errorf("refusal was recorded as a failure or requeue: failed=%v requeued=%v",
					st.failed, st.requeued)
			}
			// Nothing was built, so nothing is deferred: no successor run that
			// would be refused in turn.
			if len(st.enqueued) != 0 {
				t.Errorf("a refused run enqueued work: %v", st.enqueued)
			}
			if len(st.reviews) != 1 || st.reviews[0] != "budget:budget window exceeded" {
				t.Errorf("reviews = %v, want the one over-budget item", st.reviews)
			}
			if !strings.Contains(st.marked["c1"], "budget window exceeded") {
				t.Errorf("pinned connector was not stamped with the pause: %q", st.marked["c1"])
			}
		})
	}
}

func TestOverBudgetRefusalAppliesToResearchRuns(t *testing.T) {
	budget := 5.0
	st := newLoopStore()
	st.budget, st.spent = &budget, 6.0
	st.question = &store.ResearchQuestion{ReviewID: "rv-1", Kind: "gap", Title: "Why?"}
	ps := &pipelineStore{}
	w, runner := loopWorker(t, st, ps, agent.FakeOptions{CostPerCall: 0.02})
	w.BudgetWindow = time.Hour
	w.Log, _ = bufferLogger()

	w.process(context.Background(), &store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "research", ConnectorID: "c1",
	})

	st.mu.Lock()
	defer st.mu.Unlock()
	if runner.calls.Load() != 0 {
		t.Error("a research run spent an over-budget workspace's money")
	}
	if st.rows["r1"].status != "over_budget" {
		t.Errorf("run status = %q, want over_budget", st.rows["r1"].status)
	}
	if len(st.researched) != 0 {
		t.Errorf("findings were written for a refused run: %v", st.researched)
	}
}

func TestClaimedRunUnderBudgetOrWithoutALimitStillBuilds(t *testing.T) {
	budget := 10.0
	cases := []struct {
		name   string
		budget *float64
		spent  float64
		window time.Duration
	}{
		{"under the cap", &budget, 9.99, time.Hour},
		{"no budget set", nil, 1000, time.Hour},
		{"window disabled", &budget, 1000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newLoopStore()
			st.budget, st.spent = tc.budget, tc.spent
			ps := &pipelineStore{}
			w, runner := loopWorker(t, st, ps, agent.FakeOptions{CostPerCall: 0.02})
			w.BudgetWindow = tc.window
			w.Log, _ = bufferLogger()

			w.process(context.Background(), &store.QueuedRun{
				ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench",
				Trigger: "webhook", ConnectorID: "c1",
			})

			st.mu.Lock()
			defer st.mu.Unlock()
			if runner.calls.Load() == 0 || len(ps.runs) == 0 {
				t.Errorf("the run did not build: calls=%d runs=%+v", runner.calls.Load(), ps.runs)
			}
			if len(st.refused) != 0 {
				t.Errorf("a run with budget left was refused: %v", st.refused)
			}
		})
	}
}
