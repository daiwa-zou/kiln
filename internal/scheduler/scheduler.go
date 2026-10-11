// Package scheduler runs kiln's periodic duties: returning stale runs to the
// queue, enqueueing poll-triggered sources, the GC sweep (with the pending blob
// deletes), and the queue-depth gauge.
//
// They used to run inside every worker's claim loop. "kiln scheduler" runs
// them once for the deployment; a worker still runs them inline (Tick) unless
// configured not to, so a new image under an old deployment keeps sweeping, and
// "serve --with-worker" still does everything in one process.
//
// The concurrency contract: every duty must tolerate running concurrently with
// itself in another process. During a rollout, or a misconfiguration that
// leaves workers running duties beside a scheduler, two processes can run the
// same duty at once, and nothing in the database prevents it. Today's duties
// all qualify -- the stale requeue locks with SKIP LOCKED, a poll enqueue
// merges on the active-run index, sweeps are DELETEs, and a blob delete treats
// an absent object as done -- so overlap costs duplicate work, never
// corruption. A duty that cannot be repeated (a digest email) must guard
// itself on its own row before it is added here.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

// Duty names, as they appear in logs and in the duty label of the metrics.
const (
	DutyRequeueStale = "requeue_stale"
	DutyPollSources  = "poll_sources"
	DutySweep        = "sweep"
	DutyQueueDepth   = "queue_depth"
)

// Store is what the duties need from persistence. The worker's own Store
// embeds it, so the worker can hand itself over unchanged.
type Store interface {
	RequeueStaleRuns(ctx context.Context, olderThan time.Duration) ([]store.StaleRun, error)
	PollDueConnectors(ctx context.Context, olderThan time.Duration) ([]store.ConnectorRow, error)
	EnqueueRun(ctx context.Context, workspaceID, trigger, connectorID string) (string, bool, error)
	Sweep(ctx context.Context, softDeleteRetention, runRetention time.Duration) (int64, error)
	// SweepBlobs deletes the blobs queued for deletion and clears their rows.
	SweepBlobs(ctx context.Context, blobs store.BlobDeleter) (done, failed int, err error)
	// PendingBlobDeletes counts the queue, for a process that cannot drain it.
	PendingBlobDeletes(ctx context.Context) (int, error)
	QueueDepth(ctx context.Context) (map[string]int, error)
}

// Config is everything the duties read. The worker fills it from its own
// fields; the scheduler role from the worker.* and storage.* config sections,
// so both run the duties with the same settings.
type Config struct {
	Store   Store
	Log     *slog.Logger
	Metrics *observability.Metrics
	// Blobs deletes the pending blob deletes. Nil leaves the queue for a
	// process that has object storage.
	Blobs store.BlobDeleter

	// StaleAfter is when a claimed run without a heartbeat is requeued; zero
	// disables the requeue. SourcePollInterval is when a poll connector is
	// due; zero disables polling. The retentions drive the sweep; zero
	// disables each part.
	StaleAfter          time.Duration
	SourcePollInterval  time.Duration
	SoftDeleteRetention time.Duration
	RunRetention        time.Duration

	// FirstSweepDelay holds the first GC sweep back, so a crash-looping
	// scheduler does not sweep on every restart. Zero sweeps at once, as a
	// worker always has.
	FirstSweepDelay time.Duration

	// Embedded marks duties running inline in a worker's claim loop. There a
	// failed queue-depth sample stays at debug, as it always has: with the
	// duties in every worker, a warning would repeat once per worker per
	// sample. The scheduler role warns.
	Embedded bool

	// Now is the clock; nil means time.Now. Tests set it.
	Now func() time.Time
}

// readyWait bounds how long Run waits for a first successful duty before it
// goes ready anyway; see Run. A variable so tests can shorten it.
var readyWait = 2 * time.Minute

// duty is one periodic job. next is owned by whichever single goroutine runs
// the duty (Tick's caller, or the duty's own goroutine in Run); alive is read
// by liveness probes from other goroutines.
type duty struct {
	name     string
	interval time.Duration
	// timeout caps one attempt, so a hung query or object-store call costs
	// one missed period rather than the duty forever.
	timeout time.Duration
	// failLevel is how loudly a failed attempt is logged.
	failLevel slog.Level
	run       func(ctx context.Context) error

	next  time.Time
	alive atomic.Int64 // unix nanos: last attempt finished, or the loop started
}

// Scheduler runs the duties, either inline (Tick) or as one long-lived loop
// per duty (Run). One Scheduler must not run Tick and Run at once.
type Scheduler struct {
	cfg     Config
	log     *slog.Logger
	metrics *observability.Metrics
	now     func() time.Time
	duties  []*duty
	ready   atomic.Bool

	// succeeded is closed by the first duty attempt that succeeds.
	succeeded   chan struct{}
	succeedOnce sync.Once
}

// New builds a scheduler. Disabled duties (zero StaleAfter or
// SourcePollInterval) are left out entirely, so they neither run nor count
// toward readiness and liveness.
func New(c Config) *Scheduler {
	s := &Scheduler{cfg: c, log: c.Log, metrics: c.Metrics, now: c.Now, succeeded: make(chan struct{})}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.metrics == nil {
		s.metrics = observability.NewMetrics()
	}
	if s.now == nil {
		s.now = time.Now
	}

	// Table order is Tick's order, which is the order the worker loop has
	// always run them in, ahead of each claim.
	if c.StaleAfter > 0 {
		// The deadline is at least minutes (validate keeps it above the agent
		// timeout), so checking twice a minute adds nothing to recovery time
		// and spares the database a query per worker per poll.
		s.add(&duty{name: DutyRequeueStale, interval: 30 * time.Second, timeout: time.Minute,
			failLevel: slog.LevelError, run: s.requeueStale})
	}
	if c.SourcePollInterval > 0 {
		// Due-ness is measured against each connector's last sync, so
		// sweeping faster than once a minute buys nothing.
		s.add(&duty{name: DutyPollSources, interval: time.Minute, timeout: time.Minute,
			failLevel: slog.LevelError, run: s.pollSources})
	}
	// Roughly hourly; the knobs and the purge index have existed since M0.
	// Ten minutes covers a sweep that drains a backlog of blob deletes.
	sweep := &duty{name: DutySweep, interval: time.Hour, timeout: 10 * time.Minute,
		failLevel: slog.LevelError, run: s.sweep}
	if c.FirstSweepDelay > 0 {
		sweep.next = s.now().Add(c.FirstSweepDelay)
	}
	s.add(sweep)
	// The loop can spin every second, and a count(*) per process per second
	// is load nobody asked for; ten seconds is well inside a scrape interval.
	// A failure is a warning, not an error: monitoring must never be the
	// thing that stops builds.
	gaugeLevel := slog.LevelWarn
	if c.Embedded {
		gaugeLevel = slog.LevelDebug
	}
	s.add(&duty{name: DutyQueueDepth, interval: 10 * time.Second, timeout: time.Minute,
		failLevel: gaugeLevel, run: s.queueDepth})
	return s
}

func (s *Scheduler) add(d *duty) {
	// Seeded here as well as in Run, so a liveness probe that arrives before
	// Run starts does not read a loop that has never moved as stalled.
	d.alive.Store(later(s.now(), d.next).UnixNano())
	s.duties = append(s.duties, d)
	s.metrics.DutyRegistered(d.name)
}

// Tick runs every duty that is due, one after another, and returns. The worker
// calls it ahead of each claim and "kiln scheduler --once" calls it once; each
// duty keeps its own throttle, so calling Tick in a tight loop runs the sweep
// hourly, not on every call.
func (s *Scheduler) Tick(ctx context.Context) {
	for _, d := range s.duties {
		if ctx.Err() != nil {
			return
		}
		now := s.now()
		if now.Before(d.next) {
			continue
		}
		// Stamped before the attempt, so a failing duty waits its interval
		// rather than retrying on every call.
		d.next = now.Add(d.interval)
		s.attempt(ctx, d)
	}
}

// Run serves each duty on its own goroutine and period until ctx is canceled,
// then waits for every in-flight attempt to stop before returning ctx's error.
// A slow duty (a blob sweep stuck on object storage) therefore delays only
// itself, and a duty never overlaps itself because its goroutine waits for one
// attempt before scheduling the next.
func (s *Scheduler) Run(ctx context.Context) error {
	start := s.now()
	readyBy := time.NewTimer(readyWait)
	defer readyBy.Stop()
	var wg, first sync.WaitGroup
	for _, d := range s.duties {
		// The liveness clock starts when the loop is first due, so a delayed
		// sweep is not "stalled" while it waits out its delay.
		d.alive.Store(later(start, d.next).UnixNano())
		due := !start.Before(d.next)
		if due {
			first.Add(1)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, d, start, due, first.Done)
		}()
	}

	// Ready once every duty due at start has finished its first attempt and
	// at least one attempt has succeeded: readiness then says the duties are
	// being served, which is what a rollout waiting on this pod needs to
	// know. Past readyWait it goes ready without a success, loudly -- a
	// database outage is the alerts' business, not a reason to fail a
	// rollout.
	readyDone := make(chan struct{})
	go func() {
		defer close(readyDone)
		first.Wait()
		select {
		case <-s.succeeded:
		case <-readyBy.C:
			if ctx.Err() == nil {
				s.log.Warn("scheduler ready without a successful duty; every first attempt failed",
					"waited", readyWait.String())
			}
		case <-ctx.Done():
		}
		if ctx.Err() == nil {
			s.ready.Store(true)
		}
	}()

	wg.Wait()
	<-readyDone
	return ctx.Err()
}

func (s *Scheduler) loop(ctx context.Context, d *duty, start time.Time, due bool, firstDone func()) {
	timer := time.NewTimer(max(d.next.Sub(start), 0))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			if due {
				firstDone()
			}
			return
		case <-timer.C:
		}
		s.attempt(ctx, d)
		if due {
			firstDone()
			due = false
		}
		timer.Reset(d.interval)
	}
}

// attempt runs one duty under its timeout and records the outcome.
func (s *Scheduler) attempt(ctx context.Context, d *duty) {
	dctx, cancel := context.WithTimeout(ctx, d.timeout)
	err := d.run(dctx)
	cancel()
	d.alive.Store(s.now().UnixNano())

	switch {
	case err == nil:
		s.metrics.DutySucceeded(d.name)
		s.succeedOnce.Do(func() { close(s.succeeded) })
	case ctx.Err() != nil:
		// Shutdown, not a failure.
	case errors.Is(err, context.DeadlineExceeded):
		s.metrics.DutyFailed(d.name)
		s.log.Error("scheduler duty timed out", "duty", d.name, "timeout", d.timeout.String(), "error", err)
	default:
		s.metrics.DutyFailed(d.name)
		s.log.Log(context.Background(), d.failLevel, "scheduler duty failed", "duty", d.name, "error", err)
	}
}

// Ready reports whether Run has finished the first pass of every duty that was
// due when it started, with at least one success or readyWait gone by. It
// reads in-process state only, so a probe costs the database nothing.
func (s *Scheduler) Ready() bool { return s.ready.Load() }

// Live reports whether every duty loop is making progress: each has finished
// an attempt (or started waiting for its first) within three periods plus its
// timeout. A duty failing fast against a dead database is alive; one stuck in
// an attempt that ignores its timeout is not, and a restart is the remedy.
func (s *Scheduler) Live() error {
	now := s.now()
	var stalled []string
	for _, d := range s.duties {
		limit := 3*d.interval + d.timeout
		if since := now.Sub(time.Unix(0, d.alive.Load())); since > limit {
			stalled = append(stalled, fmt.Sprintf("%s (no progress for %s, limit %s)",
				d.name, since.Round(time.Second), limit))
		}
	}
	if len(stalled) > 0 {
		return fmt.Errorf("scheduler duty loop stalled: %s", strings.Join(stalled, ", "))
	}
	return nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (s *Scheduler) requeueStale(ctx context.Context) error {
	stale := s.cfg.StaleAfter
	requeued, err := s.cfg.Store.RequeueStaleRuns(ctx, stale)
	if err != nil {
		return fmt.Errorf("requeue stale runs: %w", err)
	}
	// Named one by one: a stale requeue means a claim went unrefreshed for
	// stale_after, which is either a dead worker or a live one cut off from the
	// database, and the previous owner is how an operator tells which.
	for _, r := range requeued {
		s.log.Warn("requeued a stale run; its worker stopped heartbeating",
			"run", r.ID, "workspace_id", r.WorkspaceID,
			"previous_worker", r.PreviousClaimedBy, "stale_after", stale.String())
	}
	s.metrics.StaleRequeued(len(requeued))
	return nil
}

// pollSources enqueues runs for poll-triggered connectors that are due.
func (s *Scheduler) pollSources(ctx context.Context) error {
	due, err := s.cfg.Store.PollDueConnectors(ctx, s.cfg.SourcePollInterval)
	if err != nil {
		return fmt.Errorf("poll scheduler query: %w", err)
	}
	failed := 0
	for _, c := range due {
		// The active-run index makes this idempotent: a connector already
		// building debounces onto its waiting run.
		if _, created, err := s.cfg.Store.EnqueueRun(ctx, c.WorkspaceID, jobs.TriggerPoll, c.ID); err != nil {
			failed++
			s.log.Error("poll enqueue failed", "connector", c.Name, "error", err)
		} else if created {
			s.log.Info("poll enqueued run", "connector", c.Name, "workspace", c.WorkspaceID)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d poll enqueues failed", failed, len(due))
	}
	return nil
}

// sweep enforces retention: soft-deleted pages past their window, expired
// sessions, dead tokens, and finished runs past theirs; then the pending blob
// deletes.
func (s *Scheduler) sweep(ctx context.Context) error {
	swept, err := s.cfg.Store.Sweep(ctx, s.cfg.SoftDeleteRetention, s.cfg.RunRetention)
	if err != nil {
		return fmt.Errorf("gc sweep: %w", err)
	}
	if swept > 0 {
		s.log.Info("gc sweep removed expired rows", "rows", swept)
	}
	return s.sweepBlobs(ctx)
}

// sweepBlobs finishes the blob deletes that the request paths queued but did
// not complete -- a refused object-store call, a process that died between the
// commit and the delete. Without a blob store there is nothing to delete with,
// and the queue waits for a process that has one -- said on every sweep while
// it is not empty, because nothing else would notice it growing.
func (s *Scheduler) sweepBlobs(ctx context.Context) error {
	if s.cfg.Blobs == nil {
		n, err := s.cfg.Store.PendingBlobDeletes(ctx)
		if err != nil {
			return fmt.Errorf("count pending blob deletes: %w", err)
		}
		if n > 0 {
			s.log.Warn("blob deletes are queued but this process has no object storage to delete them with",
				"pending", n)
		}
		return nil
	}
	done, failed, err := s.cfg.Store.SweepBlobs(ctx, s.cfg.Blobs)
	if err != nil {
		return fmt.Errorf("blob sweep: %w", err)
	}
	if done > 0 {
		s.log.Info("blob sweep deleted orphaned blobs", "blobs", done)
	}
	if failed > 0 {
		s.log.Warn("blob sweep could not delete some blobs; they will be retried", "blobs", failed)
	}
	return nil
}

// queueDepth publishes the queue gauge.
func (s *Scheduler) queueDepth(ctx context.Context) error {
	depth, err := s.cfg.Store.QueueDepth(ctx)
	if err != nil {
		return fmt.Errorf("queue depth sample: %w", err)
	}
	s.metrics.SetQueueDepth(depth)
	return nil
}
