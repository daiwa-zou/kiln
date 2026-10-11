package worker

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/store"
)

// scrapeMetrics renders the worker's registry as Prometheus would read it.
func scrapeMetrics(t *testing.T, w *Worker) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.metrics().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// bufferLogger returns a logger whose output the test can read back.
func bufferLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func researchRun() *store.QueuedRun {
	return &store.QueuedRun{ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "research"}
}

func researchStore() *fakeStore {
	return &fakeStore{question: &store.ResearchQuestion{
		ReviewID: "rv-1", Kind: "gap", Title: "Why is it slow?"}}
}

func TestHeartbeatRefreshesTheClaimWhileARunExecutesAndStopsAfter(t *testing.T) {
	st := researchStore()
	w := researchWorker(t, st, &pipelineStore{})
	w.ID = "w1"
	w.HeartbeatInterval = 15 * time.Millisecond
	// A pipeline that takes many heartbeat intervals, as a real build does:
	// without the heartbeat the claim is exactly as old at the end as at the
	// start, and the stale sweep would take the run mid-build.
	w.Pipeline.Runner = agent.NewFakeRunner(agent.FakeOptions{
		CostPerCall: 0.02, Latency: 200 * time.Millisecond})

	w.process(context.Background(), researchRun())

	during := st.beatCount()
	if during < 3 {
		t.Fatalf("heartbeats during a ~200ms run at 15ms = %d, want several", during)
	}

	// The goroutine ends with the run: a heartbeat that outlived it would keep a
	// finished (or requeued) row looking claimed, and leak a goroutine per run.
	time.Sleep(100 * time.Millisecond)
	if after := st.beatCount(); after != during {
		t.Errorf("heartbeats kept going after the run ended: %d -> %d", during, after)
	}
}

func TestHeartbeatStopsBeforeThePostBuildWork(t *testing.T) {
	// Once the pipeline has recorded the run its row is no longer 'running', so
	// a beat would be refused as a lost claim and cancel the publish and
	// bookkeeping that follow -- over a claim that was fulfilled, not lost.
	st := researchStore()
	w := researchWorker(t, st, &pipelineStore{})
	w.ID = "w1"
	w.HeartbeatInterval = 5 * time.Millisecond
	w.BudgetWindow = time.Hour
	var atEntry, atExit int
	st.onBudget = func() {
		atEntry = st.beatCount()
		time.Sleep(60 * time.Millisecond) // twelve intervals of post-build work
		atExit = st.beatCount()
	}
	w.Pipeline.Runner = agent.NewFakeRunner(agent.FakeOptions{
		CostPerCall: 0.02, Latency: 30 * time.Millisecond})

	w.process(context.Background(), researchRun())

	if atEntry == 0 {
		t.Fatal("the post-build hook never ran, so this test proves nothing")
	}
	if atExit != atEntry {
		t.Errorf("heartbeats continued past the pipeline: %d -> %d", atEntry, atExit)
	}
}

func TestLostClaimCancelsTheRunAndWritesNothing(t *testing.T) {
	st := researchStore()
	st.beatErr, st.beatErrAt = store.ErrClaimLost, 2
	ps := &pipelineStore{}
	w := researchWorker(t, st, ps)
	w.ID = "w1"
	w.HeartbeatInterval = 10 * time.Millisecond
	w.Log, _ = bufferLogger()
	// Long enough that only cancellation can end it inside the test's patience.
	w.Pipeline.Runner = agent.NewFakeRunner(agent.FakeOptions{
		CostPerCall: 0.02, Latency: 30 * time.Second})

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.process(context.Background(), researchRun())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process kept running after the heartbeat found the claim lost; the pipeline is still spending")
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	// Not a drain, so not a requeue; not a failure of the build, so not a fail.
	// Either would write to a row that now belongs to another worker.
	if len(st.requeued) != 0 || len(st.failed) != 0 {
		t.Errorf("a lost claim wrote to the run: requeued=%v failed=%v", st.requeued, st.failed)
	}
	if len(st.recorded) != 0 {
		t.Errorf("a lost claim wrote findings back: %v", st.recorded)
	}
	if !strings.Contains(scrapeMetrics(t, w), `kiln_runs_finished_total{status="claim_lost"} 1`) {
		t.Error("the abandoned run was not counted as claim_lost")
	}
}

func TestFencedTerminalWritesFromANonOwnerAreRefusedAndLogged(t *testing.T) {
	// Each path a worker can end a run by -- fail it, requeue it -- must leave
	// the row alone when another worker has taken it over, and must say so
	// rather than log a success.
	t.Run("fail", func(t *testing.T) {
		st := newLoopStore()
		log, buf := bufferLogger()
		w := &Worker{Store: st, ID: "w1", Log: log}
		st.takeOver("r1", "w2")

		// No connectors: resolution fails and the worker tries to fail the run.
		w.process(context.Background(), &store.QueuedRun{ID: "r1", WorkspaceID: "ws",
			WorkspaceSlug: "b", Trigger: "manual"})

		if msg, ok := st.failed["r1"]; ok {
			t.Errorf("non-owner failed the run: %q", msg)
		}
		if !strings.Contains(buf.String(), "no longer claimed by this worker") {
			t.Errorf("lost claim was not logged:\n%s", buf.String())
		}
		if st.rows["r1"].owner != "w2" || st.rows["r1"].status != "running" {
			t.Errorf("the new owner's claim was disturbed: %+v", st.rows["r1"])
		}
	})

	t.Run("requeue", func(t *testing.T) {
		st := newLoopStore()
		log, buf := bufferLogger()
		w := &Worker{Store: st, ID: "w1", Log: log}
		st.takeOver("r1", "w2")

		// A canceled context reads as a drain interruption.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w.process(ctx, &store.QueuedRun{ID: "r1", WorkspaceID: "ws",
			WorkspaceSlug: "b", Trigger: "webhook", ConnectorID: "missing"})

		if len(st.requeued) != 0 {
			t.Errorf("a draining non-owner requeued another worker's live claim: %v", st.requeued)
		}
		if !strings.Contains(buf.String(), "no longer claimed by this worker") {
			t.Errorf("refused requeue was not logged:\n%s", buf.String())
		}
		if st.rows["r1"].owner != "w2" || st.rows["r1"].status != "running" {
			t.Errorf("the new owner's claim was disturbed: %+v", st.rows["r1"])
		}
	})
}

func TestHeartbeatIntervalDerivation(t *testing.T) {
	cases := []struct {
		name string
		w    Worker
		want time.Duration
	}{
		{"configured wins", Worker{HeartbeatInterval: 7 * time.Second, StaleAfter: time.Hour}, 7 * time.Second},
		{"a third of a short deadline", Worker{StaleAfter: 90 * time.Second}, 30 * time.Second},
		{"capped for a long deadline", Worker{StaleAfter: 45 * time.Minute}, time.Minute},
		{"default when nothing is set", Worker{}, time.Minute},
		{"never zero", Worker{StaleAfter: time.Nanosecond}, time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.heartbeatInterval(); got != tc.want {
				t.Errorf("heartbeatInterval = %v, want %v", got, tc.want)
			}
		})
	}
}
