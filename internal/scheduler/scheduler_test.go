package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

// fakeStore records every duty call. Each knob scripts one way a duty's
// dependency can misbehave.
type fakeStore struct {
	mu         sync.Mutex
	requeues   int
	polls      int
	sweeps     int
	blobSweeps int
	samples    int
	enqueued   []string
	stale      []store.StaleRun
	pollDue    []store.ConnectorRow
	depth      map[string]int

	requeueErr error
	depthErr   error
	// pending is what PendingBlobDeletes reports.
	pending int

	// blockBlobs holds SweepBlobs until its context ends, as an object-store
	// call that never answers would; blobsIn/blobsOut bracket the stall.
	blockBlobs bool
	blobsIn    chan struct{}
	blobsOut   chan struct{}

	// depthGate, when set, holds QueueDepth until it is closed. depthDelay
	// slows every sample. inDepth/maxInDepth catch a duty overlapping itself.
	depthGate  chan struct{}
	depthDelay time.Duration
	inDepth    int
	maxInDepth int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		depth:    map[string]int{"queued": 3, "running": 1},
		blobsIn:  make(chan struct{}),
		blobsOut: make(chan struct{}),
	}
}

func (f *fakeStore) RequeueStaleRuns(context.Context, time.Duration) ([]store.StaleRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requeues++
	if f.requeueErr != nil {
		return nil, f.requeueErr
	}
	return f.stale, nil
}

func (f *fakeStore) PollDueConnectors(context.Context, time.Duration) ([]store.ConnectorRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	return f.pollDue, nil
}

func (f *fakeStore) EnqueueRun(ctx context.Context, _, trigger, connectorID string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, trigger+":"+connectorID)
	return "run-" + connectorID, true, nil
}

func (f *fakeStore) Sweep(context.Context, time.Duration, time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++
	return 1, nil
}

func (f *fakeStore) SweepBlobs(ctx context.Context, _ store.BlobDeleter) (int, int, error) {
	f.mu.Lock()
	f.blobSweeps++
	block := f.blockBlobs
	f.mu.Unlock()
	if block {
		close(f.blobsIn)
		<-ctx.Done()
		close(f.blobsOut)
		return 0, 0, ctx.Err()
	}
	return 0, 0, nil
}

func (f *fakeStore) PendingBlobDeletes(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending, nil
}

func (f *fakeStore) QueueDepth(ctx context.Context) (map[string]int, error) {
	f.mu.Lock()
	f.inDepth++
	f.maxInDepth = max(f.maxInDepth, f.inDepth)
	gate, delay := f.depthGate, f.depthDelay
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inDepth--
		f.samples++
		f.mu.Unlock()
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.depthErr != nil {
		return nil, f.depthErr
	}
	return f.depth, nil
}

// count reads one counter under the lock.
func (f *fakeStore) count(p *int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *p
}

type deleter struct{}

func (deleter) Delete(context.Context, string) error { return nil }

// clock is a settable time source for the throttle and liveness tests.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)} }

func bufferLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func scrape(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// setInterval shortens a duty's period so Run can be observed in milliseconds.
func (s *Scheduler) setInterval(name string, d time.Duration) {
	for _, dt := range s.duties {
		if dt.name == name {
			dt.interval = d
			return
		}
	}
	panic("no duty " + name)
}

// eventually polls cond for up to two seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// C1: the GC sweep runs at most hourly however often Tick is called -- the
// worker's claim loop calls it every poll interval.
func TestSweepHourlyThrottle(t *testing.T) {
	st := newFakeStore()
	clk := newClock()
	s := New(Config{Store: st, Now: clk.Now, SoftDeleteRetention: time.Hour, RunRetention: time.Hour})

	for range 6 {
		s.Tick(context.Background())
		clk.Advance(10 * time.Minute)
	}
	if got := st.count(&st.sweeps); got != 1 {
		t.Fatalf("sweeps = %d across 50 minutes of ticks, want exactly 1 (hourly throttle)", got)
	}
	clk.Advance(10 * time.Minute)
	s.Tick(context.Background())
	if got := st.count(&st.sweeps); got != 2 {
		t.Errorf("sweeps = %d after the hour turned, want 2", got)
	}
}

// The blob queue drains with the hourly sweep, but only a scheduler that can
// reach object storage has anything to delete with.
func TestSweepDrainsPendingBlobDeletesOnlyWithABlobStore(t *testing.T) {
	for _, tt := range []struct {
		name  string
		blobs store.BlobDeleter
		want  int
	}{
		{"with a blob store", deleter{}, 1},
		{"without one", nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			s := New(Config{Store: st, Blobs: tt.blobs})
			s.Tick(context.Background())
			s.Tick(context.Background())
			if got := st.count(&st.blobSweeps); got != tt.want {
				t.Errorf("blob sweeps = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRequeueStaleLogsAndCountsEachRun(t *testing.T) {
	st := newFakeStore()
	st.stale = []store.StaleRun{
		{ID: "run-a", WorkspaceID: "ws1", PreviousClaimedBy: "pod-x-17"},
		{ID: "run-b", WorkspaceID: "ws2", PreviousClaimedBy: "pod-y-3"},
	}
	log, buf := bufferLogger()
	m := observability.NewMetrics()
	s := New(Config{Store: st, Log: log, Metrics: m, StaleAfter: time.Minute})

	s.Tick(context.Background())

	out := buf.String()
	for _, want := range []string{"run-a", "pod-x-17", "run-b", "pod-y-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("stale requeue log is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(scrape(t, m), "kiln_runs_stale_requeued_total 2") {
		t.Error("stale requeues were not counted")
	}
}

func TestRequeueDisabledByZeroStaleAfter(t *testing.T) {
	st := newFakeStore()
	New(Config{Store: st}).Tick(context.Background())
	if got := st.count(&st.requeues); got != 0 {
		t.Errorf("requeue ran %d times with stale_after unset", got)
	}
}

func TestPollSchedulerEnqueuesDueConnectors(t *testing.T) {
	st := newFakeStore()
	st.pollDue = []store.ConnectorRow{
		{ID: "c1", WorkspaceID: "ws1", Name: "repo-a", TriggerMode: "poll"},
		{ID: "c2", WorkspaceID: "ws2", Name: "repo-b", TriggerMode: "poll"},
	}
	clk := newClock()
	s := New(Config{Store: st, Now: clk.Now, SourcePollInterval: 10 * time.Minute})

	s.Tick(context.Background())
	s.Tick(context.Background()) // inside the minute: throttled

	if len(st.enqueued) != 2 {
		t.Fatalf("enqueued = %v, want both due connectors once", st.enqueued)
	}
	for _, e := range st.enqueued {
		if !strings.HasPrefix(e, "poll:") {
			t.Errorf("enqueue %q not tagged with the poll trigger", e)
		}
	}
	clk.Advance(time.Minute)
	s.Tick(context.Background())
	if got := st.count(&st.polls); got != 2 {
		t.Errorf("polls = %d after a minute, want 2", got)
	}
}

func TestPollSchedulerDisabledByZeroInterval(t *testing.T) {
	st := newFakeStore()
	st.pollDue = []store.ConnectorRow{{ID: "c1", WorkspaceID: "ws1", TriggerMode: "poll"}}
	New(Config{Store: st}).Tick(context.Background())
	if len(st.enqueued) != 0 || st.polls != 0 {
		t.Errorf("poll scheduler ran despite being disabled: %v", st.enqueued)
	}
}

// C2: the gauge comes from the scheduler alone; no worker has to exist.
func TestSchedulerPublishesQueueDepthWithZeroWorkers(t *testing.T) {
	st := newFakeStore()
	m := observability.NewMetrics()
	New(Config{Store: st, Metrics: m}).Tick(context.Background())

	body := scrape(t, m)
	for _, want := range []string{
		`kiln_queue_depth{status="queued"} 3`,
		`kiln_queue_depth{status="running"} 1`,
		`kiln_scheduler_last_success_timestamp_seconds{duty="queue_depth"}`,
		`kiln_scheduler_last_success_timestamp_seconds{duty="sweep"}`,
		`kiln_scheduler_errors_total{duty="queue_depth"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}

// C6: a blob delete that never answers holds only the sweep's goroutine; the
// stale requeue keeps its own schedule, and Run waits for the stalled sweep
// to give up before it returns.
func TestBlockedBlobDeleteDoesNotStarveRequeue(t *testing.T) {
	st := newFakeStore()
	st.blockBlobs = true
	s := New(Config{Store: st, Blobs: deleter{}, StaleAfter: time.Minute})
	s.setInterval(DutyRequeueStale, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	<-st.blobsIn
	before := st.count(&st.requeues)
	eventually(t, "requeues during the blob stall", func() bool { return st.count(&st.requeues) >= before+5 })
	select {
	case <-st.blobsOut:
		t.Fatal("the blob sweep returned before shutdown; the stall did not hold")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	select {
	case <-st.blobsOut:
	default:
		t.Error("Run returned before the stalled sweep did: goroutines not joined")
	}
}

// C7: each duty keeps its own period. A slow one never overlaps itself and
// never delays another.
func TestRunServesEachDutyOnItsOwnTicker(t *testing.T) {
	st := newFakeStore()
	st.depthDelay = 30 * time.Millisecond
	s := New(Config{Store: st, StaleAfter: time.Minute})
	s.setInterval(DutyQueueDepth, time.Millisecond)
	s.setInterval(DutyRequeueStale, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = s.Run(ctx)

	if got := st.count(&st.maxInDepth); got != 1 {
		t.Errorf("queue depth ran %d at once, want never overlapping itself", got)
	}
	if samples, requeues := st.count(&st.samples), st.count(&st.requeues); requeues < 3*samples {
		t.Errorf("requeues = %d against %d slow samples: the slow duty held the others back", requeues, samples)
	}
}

// C8: a failing duty is visible -- a warning naming it and a counter -- where
// the queue gauge used to fail at debug and keep a stale value.
func TestFailedDutyWarnsAndCountsErrors(t *testing.T) {
	st := newFakeStore()
	st.depthErr = errors.New("db gone")
	st.requeueErr = errors.New("db gone")
	log, buf := bufferLogger()
	m := observability.NewMetrics()
	s := New(Config{Store: st, Log: log, Metrics: m, StaleAfter: time.Minute})
	registered := sampleLine(scrape(t, m), `kiln_scheduler_last_success_timestamp_seconds{duty="queue_depth"}`)
	time.Sleep(5 * time.Millisecond)
	s.Tick(context.Background())

	out := buf.String()
	for _, want := range []string{
		`level=WARN msg="scheduler duty failed" duty=queue_depth`,
		`level=ERROR msg="scheduler duty failed" duty=requeue_stale`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	body := scrape(t, m)
	for _, want := range []string{
		`kiln_scheduler_errors_total{duty="queue_depth"} 1`,
		`kiln_scheduler_errors_total{duty="requeue_stale"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
	if got := sampleLine(body, `kiln_scheduler_last_success_timestamp_seconds{duty="queue_depth"}`); got != registered {
		t.Errorf("a failed sample moved the last-success stamp: %q, registered %q", got, registered)
	}
}

// sampleLine returns the exposition line for one series, or "".
func sampleLine(body, series string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, series+" ") {
			return line
		}
	}
	return ""
}

// R2: the last-success series exists from registration, so a duty that never
// succeeds reads as stale from process start instead of never appearing.
func TestDutySeriesExistFromRegistration(t *testing.T) {
	m := observability.NewMetrics()
	New(Config{Store: newFakeStore(), Metrics: m, StaleAfter: time.Minute})
	body := scrape(t, m)
	for _, duty := range []string{DutyRequeueStale, DutySweep, DutyQueueDepth} {
		if sampleLine(body, `kiln_scheduler_last_success_timestamp_seconds{duty="`+duty+`"}`) == "" {
			t.Errorf("no last-success series for %s before its first attempt", duty)
		}
	}
}

// R5: inside a worker a failed gauge sample stays at debug, as it always was
// (one warning per worker per sample would be noise); it is still counted.
func TestEmbeddedGaugeFailureStaysAtDebug(t *testing.T) {
	st := newFakeStore()
	st.depthErr = errors.New("db gone")
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := observability.NewMetrics()
	New(Config{Store: st, Log: log, Metrics: m, Embedded: true}).Tick(context.Background())

	if !strings.Contains(buf.String(), `level=DEBUG msg="scheduler duty failed" duty=queue_depth`) ||
		strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("embedded gauge failure not at debug:\n%s", buf.String())
	}
	if !strings.Contains(scrape(t, m), `kiln_scheduler_errors_total{duty="queue_depth"} 1`) {
		t.Error("embedded gauge failure not counted")
	}
}

// R4: a process with no object store says how many blob deletes are waiting,
// every sweep, rather than leaving the queue to grow unseen.
func TestSweepWithoutBlobStoreWarnsAboutPendingDeletes(t *testing.T) {
	for _, tt := range []struct {
		pending int
		warn    bool
	}{{3, true}, {0, false}} {
		st := newFakeStore()
		st.pending = tt.pending
		log, buf := bufferLogger()
		New(Config{Store: st, Log: log}).Tick(context.Background())
		got := strings.Contains(buf.String(), `level=WARN msg="blob deletes are queued but this process has no object storage to delete them with" pending=3`)
		if got != tt.warn {
			t.Errorf("pending=%d: warned=%v, want %v:\n%s", tt.pending, got, tt.warn, buf.String())
		}
	}
}

// R3: readiness wants one success, but only for so long: with every attempt
// failing it stays unready until readyWait, then goes ready and says so, so a
// database outage does not fail a rollout.
func TestReadyWaitsBoundedForASuccess(t *testing.T) {
	saved := readyWait
	readyWait = 150 * time.Millisecond
	t.Cleanup(func() { readyWait = saved })

	st := newFakeStore()
	st.requeueErr = errors.New("db gone")
	st.depthErr = errors.New("db gone")
	log, buf := bufferLogger()
	s := New(Config{Store: st, Log: log, StaleAfter: time.Minute, FirstSweepDelay: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- s.Run(ctx) }()

	eventually(t, "the failing first pass", func() bool {
		return st.count(&st.requeues) >= 1 && st.count(&st.samples) >= 1
	})
	if s.Ready() {
		t.Fatal("ready after a first pass in which every duty failed")
	}
	eventually(t, "readiness at the bound", s.Ready)
	if waited := time.Since(started); waited < readyWait {
		t.Errorf("ready after %s, before the %s bound", waited, readyWait)
	}
	cancel()
	<-done
	if !strings.Contains(buf.String(), "scheduler ready without a successful duty") {
		t.Errorf("going ready without a success was not logged:\n%s", buf.String())
	}
}

// A duty that outlives its timeout is cut off and logged as an error, and the
// next duty still runs.
func TestDutyTimeoutIsAnError(t *testing.T) {
	st := newFakeStore()
	st.depthGate = make(chan struct{}) // never opened
	log, buf := bufferLogger()
	s := New(Config{Store: st, Log: log})
	for _, d := range s.duties {
		d.timeout = 10 * time.Millisecond
	}
	s.Tick(context.Background())
	if !strings.Contains(buf.String(), `level=ERROR msg="scheduler duty timed out" duty=queue_depth`) {
		t.Errorf("timeout not logged as an error:\n%s", buf.String())
	}
}

// C10: the first sweep waits for the startup delay, so a crash-looping
// scheduler does not sweep on every restart; zero sweeps at once.
func TestStartupDelayInjectable(t *testing.T) {
	st := newFakeStore()
	clk := newClock()
	s := New(Config{Store: st, Now: clk.Now, FirstSweepDelay: 5 * time.Minute})

	s.Tick(context.Background())
	clk.Advance(4 * time.Minute)
	s.Tick(context.Background())
	if got := st.count(&st.sweeps); got != 0 {
		t.Fatalf("swept %d times inside the startup delay", got)
	}
	clk.Advance(time.Minute)
	s.Tick(context.Background())
	if got := st.count(&st.sweeps); got != 1 {
		t.Fatalf("sweeps = %d once the delay passed, want 1", got)
	}
	// The delay applies once: the next sweep is an interval later, not a delay.
	clk.Advance(5 * time.Minute)
	s.Tick(context.Background())
	if got := st.count(&st.sweeps); got != 1 {
		t.Errorf("sweeps = %d five minutes after the first, want still 1", got)
	}

	st0 := newFakeStore()
	New(Config{Store: st0}).Tick(context.Background())
	if got := st0.count(&st0.sweeps); got != 1 {
		t.Errorf("zero delay: sweeps = %d, want an immediate sweep", got)
	}
}

// Readiness means the duties are running, not that the process is: it flips
// only once the first pass of every duty due at start has finished.
func TestReadyOnlyAfterFirstPass(t *testing.T) {
	st := newFakeStore()
	st.depthGate = make(chan struct{})
	s := New(Config{Store: st, StaleAfter: time.Minute, FirstSweepDelay: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	eventually(t, "the other duties' first pass", func() bool { return st.count(&st.requeues) >= 1 })
	time.Sleep(20 * time.Millisecond)
	if s.Ready() {
		t.Fatal("ready while the queue-depth duty's first pass is still running")
	}
	close(st.depthGate)
	eventually(t, "readiness", s.Ready)

	cancel()
	<-done
}

// Liveness is about the loops, not the database: a duty that keeps failing
// fast is alive, one stuck past several periods is not.
func TestLiveFailsWhenALoopStalls(t *testing.T) {
	st := newFakeStore()
	st.depthGate = make(chan struct{})
	clk := newClock()
	s := New(Config{Store: st, Now: clk.Now, StaleAfter: time.Minute})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the requeue loop", func() bool { return st.count(&st.requeues) >= 1 })

	if err := s.Live(); err != nil {
		t.Fatalf("live at start: %v", err)
	}
	// queue_depth: 10s period, 1m timeout; three periods plus the timeout is
	// the most a healthy loop can go without finishing an attempt.
	clk.Advance(3*10*time.Second + time.Minute + time.Second)
	err := s.Live()
	if err == nil || !strings.Contains(err.Error(), DutyQueueDepth) {
		t.Fatalf("Live = %v, want the stalled queue_depth loop named", err)
	}

	close(st.depthGate)
	eventually(t, "the stalled loop to recover", func() bool { return s.Live() == nil })
	cancel()
	<-done
}

// Run returns the context's error once every duty goroutine has stopped.
func TestRunReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(Config{Store: newFakeStore()}).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}
