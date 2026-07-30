package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
)

// loopStore scripts the queue for loop tests: a fixed set of runs to hand
// out, then empty. It records every state change the worker makes.
type loopStore struct {
	mu      sync.Mutex
	queue   []*store.QueuedRun
	claimed []string
	failed  map[string]string
	marked  map[string]string
	requeue int
	sealed  map[string]*store.SealedCredential
	// pinned is what ConnectorByID returns when set, for scripting a run
	// pinned to one connector.
	pinned *store.ConnectorRow
	// pollDue and enqueued script and record the poll scheduler.
	pollDue  []store.ConnectorRow
	enqueued []string
	// budget/spent/reviews script and record the budget warning.
	budget  *float64
	spent   float64
	reviews []string
	// requeued and sweeps record drain and GC activity.
	requeued []string
	sweeps   int

	claimErr   error
	requeueErr error
}

func newLoopStore(runs ...*store.QueuedRun) *loopStore {
	return &loopStore{
		queue:  runs,
		failed: map[string]string{},
		marked: map[string]string{},
		sealed: map[string]*store.SealedCredential{},
	}
}

func (l *loopStore) ClaimNextRun(context.Context, string) (*store.QueuedRun, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claimErr != nil {
		return nil, l.claimErr
	}
	if len(l.queue) == 0 {
		return nil, nil
	}
	run := l.queue[0]
	l.queue = l.queue[1:]
	l.claimed = append(l.claimed, run.ID)
	return run, nil
}

func (l *loopStore) ListFiles(context.Context, string) ([]store.FileRow, error) {
	return nil, nil
}

func (l *loopStore) QueueDepth(context.Context) (map[string]int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]int{"queued": len(l.queue), "running": 0}, nil
}

func (l *loopStore) FailRun(_ context.Context, runID, msg string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed[runID] = msg
	return nil
}

func (l *loopStore) RequeueStaleRuns(context.Context, time.Duration) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.requeueErr != nil {
		return 0, l.requeueErr
	}
	l.requeue++
	return 1, nil
}

func (l *loopStore) EnabledConnectors(context.Context, string) ([]store.ConnectorRow, error) {
	return nil, nil // no connectors: every processed run fails resolution
}

func (l *loopStore) ConnectorByID(_ context.Context, id string) (*store.ConnectorRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pinned != nil && l.pinned.ID == id {
		return l.pinned, nil
	}
	return nil, store.ErrNotFound
}

func (l *loopStore) MarkConnectorSync(_ context.Context, id, syncErr string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.marked[id] = syncErr
	return nil
}

func (l *loopStore) LoadSealedCredential(_ context.Context, id string) (*store.SealedCredential, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.sealed[id]; ok {
		return c, nil
	}
	return nil, store.ErrNotFound
}

func (l *loopStore) PollDueConnectors(context.Context, time.Duration) ([]store.ConnectorRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pollDue, nil
}

func (l *loopStore) EnqueueRun(_ context.Context, workspaceID, trigger, connectorID string) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enqueued = append(l.enqueued, trigger+":"+connectorID)
	return "run-" + connectorID, true, nil
}

func (l *loopStore) LastSuccessfulRef(context.Context, string) (string, error) { return "", nil }

func (l *loopStore) RequeueRun(_ context.Context, runID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requeued = append(l.requeued, runID)
	return nil
}

func (l *loopStore) Sweep(context.Context, time.Duration, time.Duration) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweeps++
	return 1, nil
}

func (l *loopStore) WorkspaceBudgetUSD(context.Context, string) (*float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.budget, nil
}

func (l *loopStore) SpendInWindow(context.Context, string, time.Duration) (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spent, nil
}

func (l *loopStore) FileReview(_ context.Context, _, kind, title, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reviews = append(l.reviews, kind+":"+title)
	return nil
}

func TestNewWiresConfigAndIdentity(t *testing.T) {
	cfg := &config.Config{}
	cfg.Worker.PollInterval = 7 * time.Second
	cfg.Worker.StaleAfter = time.Hour
	cfg.Worker.PermittedSourceRoots = []string{"/srv/repos"}
	cfg.Secrets.MasterKey = "k"

	w := New(cfg, nil, nil, nil)
	if w.Poll != 7*time.Second || w.StaleAfter != time.Hour {
		t.Errorf("intervals: %+v", w)
	}
	if len(w.PermittedSourceRoots) != 1 || w.MasterKey != "k" {
		t.Errorf("allowlist/master key not wired: %+v", w)
	}
	if w.ID == "" || !strings.Contains(w.ID, "-") {
		t.Errorf("worker id %q should be host-pid shaped", w.ID)
	}
	// Zero poll falls back to a sane default rather than a busy loop.
	if (&Worker{}).poll() <= 0 {
		t.Error("default poll interval must be positive")
	}
}

func TestRunOnceDrainsQueueAndFailsUnresolvableRuns(t *testing.T) {
	st := newLoopStore(
		&store.QueuedRun{ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "manual"},
		&store.QueuedRun{ID: "r2", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "webhook"},
	)
	w := &Worker{Store: st}

	n, err := w.RunOnce(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("RunOnce = %d, %v; want 2 processed", n, err)
	}
	// Both runs failed resolution (no connectors) and were finished in place,
	// releasing their workspace, rather than left claimed.
	for _, id := range []string{"r1", "r2"} {
		if msg := st.failed[id]; !strings.Contains(msg, "no enabled connector") {
			t.Errorf("run %s failure = %q, want the no-connector message", id, msg)
		}
	}
}

func TestRunOnceSurfacesClaimErrors(t *testing.T) {
	st := newLoopStore()
	st.claimErr = errors.New("db down")
	w := &Worker{Store: st}
	if _, err := w.RunOnce(context.Background()); err == nil {
		t.Error("claim error swallowed")
	}
}

func TestProcessAttributesFailureToPinnedConnector(t *testing.T) {
	st := newLoopStore()
	w := &Worker{Store: st}

	run := &store.QueuedRun{
		ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench",
		Trigger: "manual", ConnectorID: "pin-missing",
	}
	w.process(context.Background(), run)

	if _, ok := st.failed["r1"]; !ok {
		t.Fatal("run not failed")
	}
	if _, ok := st.marked["pin-missing"]; !ok {
		t.Error("pinned connector did not receive the sync error")
	}
}

func TestRunLoopStopsOnContextAndSurvivesStoreErrors(t *testing.T) {
	st := newLoopStore(
		&store.QueuedRun{ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "manual"},
	)
	st.requeueErr = errors.New("requeue broken") // must be logged, not fatal
	w := &Worker{Store: st, Poll: time.Millisecond, StaleAfter: time.Hour}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := w.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v, want the context error", err)
	}
	// The loop claimed and processed the one run before going idle.
	if len(st.claimed) != 1 || st.failed["r1"] == "" {
		t.Errorf("loop did not process the queued run: claimed=%v", st.claimed)
	}
}

func TestRequeueStaleRunsThroughTheLoop(t *testing.T) {
	st := newLoopStore()
	w := &Worker{Store: st, Poll: time.Millisecond, StaleAfter: time.Minute}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.requeue == 0 {
		t.Error("stale requeue never ran")
	}
}

func TestPollSchedulerEnqueuesDueConnectors(t *testing.T) {
	st := newLoopStore()
	st.pollDue = []store.ConnectorRow{
		{ID: "c1", WorkspaceID: "ws1", Name: "repo-a", TriggerMode: "poll"},
		{ID: "c2", WorkspaceID: "ws2", Name: "repo-b", TriggerMode: "poll"},
	}
	w := &Worker{Store: st, Poll: time.Millisecond, SourcePollInterval: 10 * time.Minute}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.enqueued) != 2 {
		t.Fatalf("enqueued = %v, want both due connectors", st.enqueued)
	}
	for _, e := range st.enqueued {
		if !strings.HasPrefix(e, "poll:") {
			t.Errorf("enqueue %q not tagged with the poll trigger", e)
		}
	}
}

func TestPollSchedulerDisabledByZeroInterval(t *testing.T) {
	st := newLoopStore()
	st.pollDue = []store.ConnectorRow{{ID: "c1", WorkspaceID: "ws1", TriggerMode: "poll"}}
	w := &Worker{Store: st, Poll: time.Millisecond} // SourcePollInterval zero

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.enqueued) != 0 {
		t.Errorf("scheduler ran despite being disabled: %v", st.enqueued)
	}
}

func TestContinuationRuns(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		deferred int
		trigger  string
		want     int
	}{
		{"deferred always continues", jobs.StatusSucceeded, 3, "continuation", 1},
		{"partial retries once", jobs.StatusPartial, 0, "webhook", 1},
		{"partial continuation stops", jobs.StatusPartial, 0, "continuation", 0},
		{"clean run stops", jobs.StatusSucceeded, 0, "webhook", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newLoopStore()
			w := &Worker{Store: st}
			run := &store.QueuedRun{ID: "r1", WorkspaceID: "ws", WorkspaceSlug: "b",
				Trigger: tc.trigger, ConnectorID: "c1"}
			res := &jobs.BuildResult{Deferred: tc.deferred}
			res.Summary.Status = tc.status

			w.enqueueContinuation(context.Background(), run, res, w.logger())

			st.mu.Lock()
			defer st.mu.Unlock()
			if len(st.enqueued) != tc.want {
				t.Errorf("enqueued = %v, want %d", st.enqueued, tc.want)
			}
			if tc.want == 1 && st.enqueued[0] != "continuation:c1" {
				t.Errorf("continuation = %q", st.enqueued[0])
			}
		})
	}
}

func TestWarnNearBudget(t *testing.T) {
	budget := 10.0
	cases := []struct {
		name    string
		budget  *float64
		spent   float64
		window  time.Duration
		reviews int
	}{
		{"under threshold", &budget, 7.9, time.Hour, 0},
		{"at threshold", &budget, 8.0, time.Hour, 1},
		{"over budget", &budget, 12.0, time.Hour, 1},
		{"no budget set", nil, 100, time.Hour, 0},
		{"window disabled", &budget, 12, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newLoopStore()
			st.budget, st.spent = tc.budget, tc.spent
			w := &Worker{Store: st, BudgetWindow: tc.window}
			w.warnNearBudget(context.Background(), "ws1", w.logger())
			if len(st.reviews) != tc.reviews {
				t.Errorf("reviews = %v, want %d", st.reviews, tc.reviews)
			}
			if tc.reviews == 1 && st.reviews[0] != "budget:budget window nearly exhausted" {
				t.Errorf("review = %q", st.reviews[0])
			}
		})
	}
}

func TestOpenCredentialRoundTripAndFailures(t *testing.T) {
	masterKey := hex.EncodeToString([]byte(strings.Repeat("k", 32)))
	keyring, err := crypto.NewKeyring(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := keyring.Seal([]byte("ghp_token"))
	if err != nil {
		t.Fatal(err)
	}

	st := newLoopStore()
	st.sealed["cred"] = &store.SealedCredential{ID: "cred", Kind: "git_pat", Ciphertext: ct, Nonce: nonce}
	st.sealed["wrong-kind"] = &store.SealedCredential{ID: "wrong-kind", Kind: "ssh_key", Ciphertext: ct, Nonce: nonce}

	w := &Worker{Store: st, MasterKey: masterKey}
	got, err := w.openCredential(context.Background(), "cred")
	if err != nil || got != "ghp_token" {
		t.Fatalf("openCredential = %q, %v", got, err)
	}

	if _, err := w.openCredential(context.Background(), "wrong-kind"); err == nil ||
		!strings.Contains(err.Error(), "git_pat") {
		t.Errorf("wrong-kind credential accepted: %v", err)
	}
	if _, err := w.openCredential(context.Background(), "absent"); err == nil {
		t.Error("missing credential opened")
	}

	// A worker without the master key must fail with guidance, not garbage.
	bare := &Worker{Store: st}
	if _, err := bare.openCredential(context.Background(), "cred"); err == nil ||
		!strings.Contains(err.Error(), "KILN_MASTER_KEY") {
		t.Errorf("keyless open error unhelpful: %v", err)
	}

	// A rotated master key fails authentication and says so.
	rotated := &Worker{Store: st, MasterKey: hex.EncodeToString([]byte(strings.Repeat("x", 32)))}
	if _, err := rotated.openCredential(context.Background(), "cred"); err == nil ||
		!strings.Contains(err.Error(), "master key") {
		t.Errorf("rotated-key open error unhelpful: %v", err)
	}
}

func TestSourceSpecRejectsForbiddenRemoteURL(t *testing.T) {
	// A url-configured git connector goes through the clone path, whose
	// policy re-check must reject a loopback remote before any process runs.
	st := newLoopStore()
	st.pinned = &store.ConnectorRow{
		ID: "c1", Kind: "git", Name: "code",
		Config: map[string]any{"url": "https://127.0.0.1/repo.git"},
	}
	w := &Worker{Store: st}

	run := &store.QueuedRun{ID: "r", WorkspaceID: "ws", WorkspaceSlug: "bench", ConnectorID: "c1"}
	spec, _, cleanup, err := w.sourceSpec(context.Background(), run)
	if cleanup != nil {
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "public address") {
		t.Errorf("loopback remote accepted: spec=%+v err=%v", spec, err)
	}
}

func TestInterruptedRunIsRequeuedNotFailed(t *testing.T) {
	st := newLoopStore()
	// A pinned connector that does not exist makes process error out; with
	// the run context already canceled, that reads as a drain interruption
	// and the run must go back to the queue, not into 'failed'.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &Worker{Store: st}
	w.process(ctx, &store.QueuedRun{ID: "r1", WorkspaceID: "ws",
		WorkspaceSlug: "b", Trigger: "webhook", ConnectorID: "missing"})

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.requeued) != 1 || st.requeued[0] != "r1" {
		t.Errorf("requeued = %v, want [r1]", st.requeued)
	}
	if len(st.failed) != 0 {
		t.Errorf("interrupted run marked failed: %v", st.failed)
	}
}

func TestSweepRunsHourlyFromTheLoop(t *testing.T) {
	st := newLoopStore()
	w := &Worker{Store: st, Poll: time.Millisecond,
		SoftDeleteRetention: time.Hour, RunRetention: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.sweeps != 1 {
		t.Errorf("sweeps = %d, want exactly 1 (hourly throttle)", st.sweeps)
	}
}

func TestDrainGraceDefaultsAboveAgentTimeout(t *testing.T) {
	if got := (&Worker{}).drainGrace(); got != 15*time.Minute {
		t.Errorf("default drain grace = %v", got)
	}
	if got := (&Worker{DrainGrace: time.Minute}).drainGrace(); got != time.Minute {
		t.Errorf("configured drain grace = %v", got)
	}
}

func TestRunDrainsInFlightRunOnShutdown(t *testing.T) {
	// One queued run whose processing fails resolution (no connectors), a
	// context canceled almost immediately: the loop must still process the
	// claimed run to completion before returning.
	st := newLoopStore(&store.QueuedRun{ID: "r1", WorkspaceID: "ws",
		WorkspaceSlug: "b", Trigger: "manual"})
	w := &Worker{Store: st, Poll: time.Millisecond, DrainGrace: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_ = w.Run(ctx)

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.claimed) != 1 {
		t.Fatalf("claimed = %v", st.claimed)
	}
	// The run finished (failed on no-connectors) rather than being abandoned
	// mid-flight: drain means completion or requeue, never limbo.
	if len(st.failed) != 1 && len(st.requeued) != 1 {
		t.Errorf("run left in limbo: failed=%v requeued=%v", st.failed, st.requeued)
	}
}
