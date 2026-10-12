// Package worker claims queued runs and executes them through the same
// pipeline entry point the CLI uses.
//
// The queue is the runs table (FOR UPDATE SKIP LOCKED); there is no external
// broker. One worker process claims one run at a time: builds are LLM-bound
// and take minutes, so per-process concurrency would multiply spend, not
// throughput. Scaling is adding worker processes.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/connector"
	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	webconn "github.com/daiwa-zou/kiln/internal/connector/web"
	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/github"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/scheduler"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// Store is what the worker needs from persistence, declared here so the loop
// is testable against a fake while the SQL stays in internal/store.
type Store interface {
	ClaimNextRun(ctx context.Context, workerID string) (*store.QueuedRun, error)
	// FailRun, RefuseRunOverBudget, RequeueRun and HeartbeatRun are fenced to the
	// worker holding the claim and return store.ErrClaimLost when it no longer
	// does. RecordRun and MarkRunItem are fenced the same way, through the claim
	// process() puts on the pipeline's context.
	FailRun(ctx context.Context, runID, workerID, message string) error
	RefuseRunOverBudget(ctx context.Context, runID, workerID, message string) error
	HeartbeatRun(ctx context.Context, runID, workerID string) error
	// The scheduler duties' half: stale requeue, polling, sweeps, queue depth.
	scheduler.Store
	EnabledConnectors(ctx context.Context, workspaceID string) ([]store.ConnectorRow, error)
	ConnectorByID(ctx context.Context, id string) (*store.ConnectorRow, error)
	MarkConnectorSync(ctx context.Context, id, syncErr string) error
	LoadSealedCredential(ctx context.Context, id string) (*store.SealedCredential, error)
	InstallationServesWorkspace(ctx context.Context, workspaceID string, installationID int64) (bool, error)
	ListFiles(ctx context.Context, workspaceID string) ([]store.FileRow, error)
	// RecordFileSkips persists which uploads a sync could not read (and clears
	// the ones it could), returning the paths whose skip is new.
	RecordFileSkips(ctx context.Context, workspaceID string, skips []store.FileSkip, readPaths []string, at time.Time) ([]string, error)
	LastSuccessfulRef(ctx context.Context, workspaceID string) (string, error)
	WorkspaceBudgetUSD(ctx context.Context, workspaceID string) (*float64, error)
	SpendInWindow(ctx context.Context, workspaceID string, window time.Duration) (float64, error)
	FileReview(ctx context.Context, workspaceID, kind, title, detail string) error
	ResearchQuestionFor(ctx context.Context, runID string) (*store.ResearchQuestion, error)
	RecordResearch(ctx context.Context, reviewID, findings string, resolved bool) error
	RequeueRun(ctx context.Context, runID, workerID string) error

	// Publishing reads the finished wiki back out and records where it went.
	// The pipeline writes the wiki; the worker mirrors it, so these live here
	// rather than on jobs.Store.
	PublishTargetFor(ctx context.Context, workspaceID string) (store.PublishTarget, error)
	MarkPublished(ctx context.Context, workspaceID, commit string, revision int64, publishErr string) error
	WikiRevision(ctx context.Context, workspaceID string) (int64, error)
	LoadPages(ctx context.Context, workspaceID string) ([]wiki.Page, error)
	LoadArtifact(ctx context.Context, workspaceID, kind string) (string, error)
	ListFigures(ctx context.Context, workspaceID string) ([]store.FigureRow, error)
}

// Worker is one claim-and-build loop.
type Worker struct {
	Store    Store
	Pipeline *jobs.Pipeline
	Log      *slog.Logger

	// ID names this worker in runs.claimed_by, for diagnosing which process
	// owns a stuck run.
	ID string

	// Poll is the idle re-check interval; StaleAfter is when a claimed run is
	// presumed orphaned and requeued. Zero values take the config defaults.
	Poll       time.Duration
	StaleAfter time.Duration

	// HeartbeatInterval is how often the claim on a running build is refreshed.
	// Zero derives it from StaleAfter; see heartbeatInterval.
	HeartbeatInterval time.Duration

	// SourcePollInterval is how often trigger_mode='poll' connectors are due
	// for a refresh, for origins with no useful webhook. Zero disables the
	// poll scheduler.
	SourcePollInterval time.Duration

	// BudgetWindow is the rolling window workspace budgets apply to. After
	// each run, spend at or past 80% of budget_usd files a warning review,
	// so the pause the API enforces at 100% never arrives unannounced.
	BudgetWindow time.Duration

	// DrainGrace is how long an in-flight run may continue after shutdown is
	// requested before it is interrupted and requeued. Zero takes a default
	// comfortably above one agent call.
	DrainGrace time.Duration

	// Retention drives the hourly GC sweep; zero values disable each part.
	SoftDeleteRetention time.Duration
	RunRetention        time.Duration

	// SkipSchedulerDuties leaves the stale requeue, polling, sweeps and the
	// queue-depth gauge to a "kiln scheduler" process. Inverted so the zero
	// value is the old behaviour: a Worker built without it keeps its duties.
	SkipSchedulerDuties bool

	// duties runs the scheduler duties inline, built on first use from the
	// fields above; see dutyRunner.
	duties *scheduler.Scheduler

	// PermittedSourceRoots is the local-path allowlist for database-configured
	// connectors. Empty denies every local path: connector configs are
	// API-writable data, and an unchecked path is a local-file-inclusion
	// primitive.
	PermittedSourceRoots []string

	// MasterKey opens sealed credentials, here and nowhere else: the worker
	// at sync time is the single place plaintext secrets are reconstructed.
	MasterKey string

	// GitHub mints installation tokens for clones when a connector names a
	// github installation. Short-lived and repo-scoped, these supersede
	// stored PATs wherever the App is installed.
	GitHub *github.Client

	// AuthDisabled mirrors auth.mode = "none": a loopback-only dev deployment
	// where nobody signs in, so there is no user to link an installation to
	// and the run-time ownership check in cloneRemote cannot apply.
	AuthDisabled bool

	// Blobs reads uploaded workspace files at sync time, for upload
	// connectors in files mode (no path in the config). Nil makes such a
	// connector a clear configuration error rather than a mystery.
	Blobs blob.Store

	// Metrics instruments builds. Nil is fine -- metrics() substitutes a
	// no-op registry -- so tests and embedded uses need not wire one.
	Metrics *observability.Metrics
}

// metrics never returns nil, so the recording calls need no guard at each
// site. A discarded registry costs an allocation once, not a branch per run.
func (w *Worker) metrics() *observability.Metrics {
	if w.Metrics == nil {
		w.Metrics = observability.NewMetrics()
	}
	return w.Metrics
}

// New assembles a worker from resolved configuration.
func New(cfg *config.Config, st *store.WikiStore, pipeline *jobs.Pipeline, log *slog.Logger) *Worker {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return &Worker{
		Store:                st,
		Pipeline:             pipeline,
		Log:                  log,
		ID:                   fmt.Sprintf("%s-%d", host, os.Getpid()),
		Poll:                 cfg.Worker.PollInterval,
		StaleAfter:           cfg.Worker.StaleAfter,
		HeartbeatInterval:    cfg.Worker.HeartbeatInterval,
		SourcePollInterval:   cfg.Worker.SourcePollInterval,
		BudgetWindow:         cfg.Agent.BudgetWindow,
		DrainGrace:           cfg.Worker.DrainGrace,
		SoftDeleteRetention:  cfg.Storage.SoftDeleteRetention,
		RunRetention:         cfg.Storage.RunArtifactRetention,
		SkipSchedulerDuties:  !cfg.Worker.SchedulerDuties,
		PermittedSourceRoots: cfg.Worker.PermittedSourceRoots,
		MasterKey:            cfg.Secrets.MasterKey,
		AuthDisabled:         cfg.Auth.Mode == config.AuthNone,
		GitHub: &github.Client{
			AppID:      cfg.GitHub.AppID,
			PrivateKey: []byte(cfg.Secrets.GitHubPrivateKey),
			BaseURL:    cfg.GitHub.BaseURL,
			APIBaseURL: cfg.GitHub.APIBaseURL,
		},
	}
}

// Run claims and executes runs until the context is canceled. The error is
// always ctx.Err(): queue and build failures are recorded on their runs and
// logged, never allowed to kill the loop, because one poisoned run must not
// take the worker down with it.
//
// Shutdown drains: cancellation stops claiming immediately, but the run in
// flight keeps a detached context for DrainGrace so paid-for agent work can
// land. Only past the grace window is the build interrupted — and then it is
// not failed: a rolling deploy must never turn an in-flight build into a
// permanent failure plus wasted spend. A run that had produced nothing yet is
// requeued whole. A build that was part-way keeps the units it finished (they
// are imported and the run is recorded canceled) and a continuation run is
// queued for the rest.
func (w *Worker) Run(ctx context.Context) error {
	log := w.logger().With("worker", w.ID)
	log.Info("worker started", "poll", w.poll().String(), "scheduler_duties", !w.SkipSchedulerDuties)

	for {
		if err := ctx.Err(); err != nil {
			log.Info("worker stopping")
			return err
		}

		if !w.SkipSchedulerDuties {
			w.dutyRunner().Tick(ctx)
		}

		run, err := w.Store.ClaimNextRun(ctx, w.ID)
		if err != nil {
			log.Error("claim failed", "error", err)
			run = nil
		}
		if run == nil {
			select {
			case <-ctx.Done():
			case <-time.After(w.poll()):
			}
			continue
		}

		// The run outlives the shutdown signal by up to DrainGrace.
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		graceDone := make(chan struct{})
		go func() {
			defer close(graceDone)
			select {
			case <-ctx.Done():
				select {
				case <-runCtx.Done(): // process finished first
				case <-time.After(w.drainGrace()):
					log.Warn("drain grace expired; interrupting the in-flight run")
					cancel()
				}
			case <-runCtx.Done():
			}
		}()

		w.process(runCtx, run)
		cancel()
		<-graceDone
	}
}

// RunOnce drains the queue and returns, for tests and one-shot invocations.
func (w *Worker) RunOnce(ctx context.Context) (processed int, err error) {
	for {
		run, err := w.Store.ClaimNextRun(ctx, w.ID)
		if err != nil {
			return processed, err
		}
		if run == nil {
			return processed, nil
		}
		w.process(ctx, run)
		processed++
	}
}

// process executes one claimed run. Every failure path finishes the run row:
// a claimed run that stays 'running' would block its workspace until the
// stale-requeue deadline.
func (w *Worker) process(ctx context.Context, run *store.QueuedRun) {
	log := w.logger().With("run", run.ID, "workspace", run.WorkspaceSlug)
	log.Info("claimed run", "trigger", run.Trigger)

	// Everything below runs under this worker's claim. Marking the context
	// fences each write the pipeline makes for the run (its record, its item
	// progress) to this worker, so a claim that lapsed cannot overwrite the
	// worker that took the run over. The heartbeat keeps the claim from lapsing
	// in the first place, and cancels the run if it nonetheless has: a build
	// nobody owns any more is spend nobody can use.
	ctx = store.WithClaim(ctx, w.ID)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopHeartbeat := w.heartbeat(ctx, cancel, run.ID, log)
	defer stopHeartbeat()

	// Counted at claim rather than at execution: a run that fails to resolve
	// its sources never reaches the pipeline, and those are precisely the
	// failures worth alerting on (a bad connector, unreachable storage).
	// Instrumenting later would make them invisible.
	started := time.Now()
	w.metrics().RunStarted(run.Trigger)

	// The rolling budget is judged here, at claim, as well as when a person asks
	// for a build. Webhook, poll and continuation runs are enqueued without any
	// budget check, and a run can wait in the queue while the window fills; this
	// is the one point every run passes through before it can spend.
	if w.refuseOverBudget(ctx, run, log) {
		w.metrics().RunFinished(jobs.StatusOverBudget, time.Since(started), 0, 0, 0, 0)
		return
	}

	spec, connectors, staged, cleanup, err := w.sourceSpec(ctx, run)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		if lostClaim(ctx) {
			w.abandon(log, started)
			return
		}
		if ctx.Err() != nil {
			// Interrupted while resolving, not misconfigured: requeue.
			w.requeueInterrupted(ctx, run.ID, log, started, "resolution interrupted by shutdown; requeuing")
			return
		}
		log.Error("run not executable", "error", err)
		w.failRun(ctx, run.ID, err, log)
		w.metrics().RunFinished("unresolvable", time.Since(started), 0, 0, 0, 0)
		// A run pinned to one connector can attribute the failure to it;
		// workspace-wide resolution failures have no single owner.
		if run.ConnectorID != "" {
			if merr := w.Store.MarkConnectorSync(ctx, run.ConnectorID, err.Error()); merr != nil {
				log.Error("mark connector sync failed", "error", merr)
			}
		}
		return
	}

	// A research run exists to answer one review item, not to build pages. It
	// takes the same claim, the same source resolution, and the same drain
	// handling as a build -- everything above this line -- and diverges in what
	// it does with the material.
	//
	// It deliberately does not touch the connector sync bookkeeping the build
	// path finishes with. Research reads the sources but is not a sync of
	// record: stamping last_sync_at here would push every poll-triggered
	// connector's next refresh out by a full interval, so answering a question
	// about the bench would quietly delay rebuilding it.
	if run.Trigger == jobs.TriggerResearch {
		w.research(ctx, run, spec, started, stopHeartbeat)
		return
	}

	// A run that arrived through a push carries the head it should build
	// toward, which makes it incremental-eligible: the base is always the
	// workspace's own last successful build, never the webhook's claim, so
	// debounced pushes cannot leave a hole in the range. Manual and poll
	// runs stay full (hash-gated) rebuilds.
	baseRef := ""
	if run.RefTo != "" {
		if ref, err := w.Store.LastSuccessfulRef(ctx, run.WorkspaceID); err != nil {
			log.Error("last-ref lookup failed; building full", "error", err)
		} else {
			baseRef = ref
		}
	}

	res, err := w.Pipeline.Execute(ctx, jobs.ExecuteRequest{
		RunID:       run.ID,
		WorkspaceID: run.WorkspaceID,
		Trigger:     run.Trigger,
		Source:      spec,
		Connectors:  connectors,
		BaseRef:     baseRef,
		OnDocSkips:  w.docSkipSink(run, connectors, spec.DocsDir, staged, log),
	})
	// The pipeline has recorded the run, so its row is no longer 'running' and
	// a further beat would be refused as a lost claim -- canceling the publish
	// and bookkeeping below over a claim that was in fact fulfilled. Heartbeat
	// covers the build, which is the part that can outlast stale_after; what
	// follows is short and happens on an already-settled row.
	stopHeartbeat()

	syncErr := ""
	switch {
	// Judged by the outcome, not the context alone: a build that returned
	// cleanly recorded its run under our claim, whatever a late beat said.
	case errors.Is(err, store.ErrClaimLost) || (err != nil && lostClaim(ctx)):
		// Not this worker's run any more: there is nothing to requeue, fail, or
		// record, and the connector bookkeeping below belongs to whoever holds
		// the claim now.
		w.abandon(log, started)
		return
	case err != nil && ctx.Err() != nil:
		// Interrupted by the drain deadline, not broken: back to the queue,
		// where this or another worker picks it up cleanly.
		syncErr = "interrupted by shutdown; requeued"
		w.requeueInterrupted(ctx, run.ID, log, started, "build interrupted by shutdown; requeuing")
	case err == nil && res.Summary.Status == jobs.StatusCanceled:
		// The drain deadline stopped the build part-way. The pipeline imported the
		// units that finished and recorded the run canceled, so the row is
		// terminal -- there is nothing left to requeue, and treating this as the
		// success path below would publish a partial wiki and enqueue the
		// continuation, both on a context that is already dead. The work the
		// build did not reach is what the continuation is for.
		syncErr = "interrupted by shutdown; continuation queued"
		log.Warn("build interrupted by shutdown; continuing in a new run",
			"unreached", res.Deferred, "cost_usd", res.Summary.CostUSD)
		w.metrics().RunFinished(string(res.Summary.Status), time.Since(started),
			res.Summary.CostUSD, res.Summary.Created, res.Summary.Updated, res.Summary.Deleted)
		w.continueInterrupted(ctx, run, log)
	case err != nil:
		syncErr = err.Error()
		log.Error("build failed", "error", err)
		w.failRun(ctx, run.ID, err, log)
		w.metrics().RunFinished("failed", time.Since(started), 0, 0, 0, 0)
	default:
		log.Info("build finished", "status", res.Summary.Status,
			"cost_usd", res.Summary.CostUSD,
			"created", res.Summary.Created, "updated", res.Summary.Updated)
		w.metrics().RunFinished(string(res.Summary.Status), time.Since(started),
			res.Summary.CostUSD, res.Summary.Created, res.Summary.Updated, res.Summary.Deleted)
		w.warnNearBudget(ctx, run.WorkspaceID, log)
		// Mirror the finished wiki to its repository, if this bench publishes.
		// After the import and outside the run's success: the wiki is already
		// committed and correct by now, so a repository that cannot be written
		// is a degraded copy of something that exists, not a broken build.
		w.publishAfterBuild(ctx, run.WorkspaceID, run.WorkspaceSlug, run.Trigger, log)
		w.enqueueContinuation(ctx, run, res, log)
	}
	// Terminal bookkeeping, so it outlives cancellation like the requeue and
	// the run record do. On the interrupted path ctx is already canceled by
	// definition -- writing through it meant the one message that branch
	// composes ("interrupted by shutdown; requeued") could never reach the
	// connector, and every drain logged a failure for it instead.
	syncCtx := context.WithoutCancel(ctx)
	for _, id := range []string{connectors.Git, connectors.Upload, connectors.Web} {
		if id == "" {
			continue
		}
		if merr := w.Store.MarkConnectorSync(syncCtx, id, syncErr); merr != nil {
			log.Error("mark connector sync failed", "error", merr)
		}
	}
}

// refuseOverBudget finishes a just-claimed run as over_budget, without calling
// the pipeline, when its workspace has already spent its budget within the
// rolling window. It reports whether it did.
//
// The review is the same item the API files for a refused request, so the queue
// holds one "budget window exceeded" entry however the pause was reached. No
// continuation follows: nothing was built, so nothing is deferred, and a
// refused run that enqueued a successor would only be refused again.
//
// A run pinned to a connector stamps that connector's sync with the reason. That
// both shows the pause where the connector is managed and, for a poll
// connector, spaces the next attempt a full interval out: left unstamped, the
// connector would stay due and the scheduler would enqueue a run that is
// refused every minute for as long as the window stays full.
//
// A failed lookup lets the run proceed. The API refuses on a lookup error, but a
// worker holding a claim has no caller to report to, and failing queued builds
// over a database hiccup trades a possible overspend for a certain outage.
func (w *Worker) refuseOverBudget(ctx context.Context, run *store.QueuedRun, log *slog.Logger) bool {
	if w.BudgetWindow <= 0 {
		return false
	}
	budget, err := w.Store.WorkspaceBudgetUSD(ctx, run.WorkspaceID)
	if err != nil {
		log.Error("budget lookup failed; running without the window check", "error", err)
		return false
	}
	if budget == nil || *budget <= 0 {
		return false
	}
	spent, err := w.Store.SpendInWindow(ctx, run.WorkspaceID, w.BudgetWindow)
	if err != nil {
		log.Error("spend lookup failed; running without the window check", "error", err)
		return false
	}
	if spent < *budget {
		return false
	}

	reason := fmt.Sprintf("budget window exceeded: $%.2f of $%.2f spent in the last %s",
		spent, *budget, w.BudgetWindow)
	log.Warn("workspace over budget; finishing the claimed run without building",
		"spent_usd", spent, "budget_usd", *budget, "trigger", run.Trigger)

	// Terminal bookkeeping, so it outlives cancellation like the other terminal
	// writes in process.
	done := context.WithoutCancel(ctx)
	switch err := w.Store.RefuseRunOverBudget(done, run.ID, w.ID, reason); {
	case errors.Is(err, store.ErrClaimLost):
		log.Error("over-budget refusal not recorded: the run is no longer claimed by this worker")
	case err != nil:
		log.Error("recording over-budget refusal failed", "error", err)
	}
	title, detail := store.BudgetExceededReview(spent, *budget, w.BudgetWindow)
	if err := w.Store.FileReview(done, run.WorkspaceID, "budget", title, detail); err != nil {
		log.Error("filing budget review failed", "error", err)
	}
	if run.ConnectorID != "" {
		if err := w.Store.MarkConnectorSync(done, run.ConnectorID, reason); err != nil {
			log.Error("mark connector sync failed", "error", err)
		}
	}
	return true
}

// research executes a claimed research run: read the bench's sources with one
// review item's question in hand and write the findings back onto it.
//
// A failure is written onto the item too, not just onto the run. The item was
// already answerable by a human the whole time -- research never took it out
// of the queue -- so what a failure owes the reader is the reason there are no
// findings attached, which is not something they would otherwise find without
// going to the runs list and matching timestamps.
func (w *Worker) research(ctx context.Context, run *store.QueuedRun, spec jobs.SourceSpec, started time.Time, stopHeartbeat func()) {
	log := w.logger().With("run", run.ID, "workspace", run.WorkspaceSlug)

	q, err := w.Store.ResearchQuestionFor(ctx, run.ID)
	if err != nil {
		// The review item is gone -- resolved by a human, or swept with its
		// workspace -- so there is nothing to answer and nothing to release.
		log.Warn("research run has no review item; nothing to answer", "error", err)
		w.failRun(ctx, run.ID, fmt.Errorf("research run has no review item: %w", err), log)
		w.metrics().RunFinished("unresolvable", time.Since(started), 0, 0, 0, 0)
		return
	}

	res, err := w.Pipeline.Research(ctx, jobs.ResearchRequest{
		RunID:       run.ID,
		WorkspaceID: run.WorkspaceID,
		Source:      spec,
		Question: jobs.Question{
			Kind:     q.Kind,
			Title:    q.Title,
			Detail:   q.Detail,
			PageSlug: q.PageSlug,
			FiledBy:  q.FiledBy,
		},
	})
	stopHeartbeat() // see process: the run row is settled, so a beat would only fail

	// Write-back outlives cancellation for the same reason the run record
	// does: the call is already billed, and findings nobody can read are the
	// one outcome worse than no findings at all.
	writeCtx := context.WithoutCancel(ctx)

	if err != nil {
		if lostClaim(ctx) {
			// The item is left untouched for the same reason as below: whoever
			// holds the run now is answering it.
			w.abandon(log, started)
			return
		}
		if ctx.Err() != nil {
			// Interrupted by the drain deadline, not broken: back to the
			// queue, and the item is left untouched because the answer is
			// still coming -- from whichever worker picks this up next.
			w.requeueInterrupted(ctx, run.ID, log, started, "research interrupted by shutdown; requeuing")
			return
		}
		log.Error("research failed", "error", err)
		if rerr := w.Store.RecordResearch(writeCtx, q.ReviewID,
			"Research did not complete: "+err.Error(), false); rerr != nil {
			log.Error("releasing the review item failed", "error", rerr)
		}
		// The pipeline ledgers its own failure on the run row; failRun here
		// would overwrite the detail it recorded with a duplicate.
		w.metrics().RunFinished("failed", time.Since(started), 0, 0, 0, 0)
		return
	}

	if rerr := w.Store.RecordResearch(writeCtx, q.ReviewID, findingsText(res), res.Resolved); rerr != nil {
		log.Error("recording research findings failed", "error", rerr)
	}
	log.Info("research finished", "review", q.ReviewID,
		"resolved", res.Resolved, "cost_usd", res.CostUSD)
	w.metrics().RunFinished(jobs.StatusSucceeded, time.Since(started), res.CostUSD, 0, 0, 0)
	w.warnNearBudget(ctx, run.WorkspaceID, log)
}

// findingsText renders the outcome as the prose that lands on the review card:
// the answer, then what it rests on. Evidence is appended rather than left in a
// structured field because the card is read, not queried, and a citation the
// reader cannot see is a citation that was never made.
func findingsText(res *jobs.ResearchOutcome) string {
	if len(res.Evidence) == 0 {
		return res.Findings
	}
	return res.Findings + "\n\nEvidence:\n  " + strings.Join(res.Evidence, "\n  ")
}

// sourceSpec resolves a run's connectors into build material. A run pinned to
// a connector uses that one; otherwise every enabled connector on the
// workspace feeds the build, merged into one map like the CLI's --docs flag.
// The returned SourceConnectors carries the id of every connector that fed
// the build, for per-source attribution and sync bookkeeping; staged is the
// sha256 of each uploaded file that was materialized from the blob store,
// keyed by stored path, for the skip sink (nil when no upload connector is in
// files mode); the cleanup (possibly nil) removes any staging a remote clone
// materialized.
func (w *Worker) sourceSpec(ctx context.Context, run *store.QueuedRun) (jobs.SourceSpec, jobs.SourceConnectors, map[string]string, func(), error) {
	fail := func(err error) (jobs.SourceSpec, jobs.SourceConnectors, map[string]string, func(), error) {
		return jobs.SourceSpec{}, jobs.SourceConnectors{}, nil, nil, err
	}

	var connectors []store.ConnectorRow
	if run.ConnectorID != "" {
		c, err := w.Store.ConnectorByID(ctx, run.ConnectorID)
		if err != nil {
			return fail(err)
		}
		connectors = []store.ConnectorRow{*c}
	} else {
		var err error
		connectors, err = w.Store.EnabledConnectors(ctx, run.WorkspaceID)
		if err != nil {
			return fail(err)
		}
	}
	if len(connectors) == 0 {
		return fail(fmt.Errorf(
			"workspace %s has no enabled connector; configure one before queueing runs", run.WorkspaceSlug))
	}

	spec := jobs.SourceSpec{Slug: run.WorkspaceSlug}
	var ids jobs.SourceConnectors
	var staged map[string]string
	// More than one connector can stage material (a remote clone and a
	// blob-store materialization in the same run), so cleanups accumulate
	// rather than occupy a single slot that a second stage would overwrite.
	var cleanups []func()
	cleanup := func() {
		for _, fn := range cleanups {
			fn()
		}
	}
	fail2 := func(err error) (jobs.SourceSpec, jobs.SourceConnectors, map[string]string, func(), error) {
		return jobs.SourceSpec{}, jobs.SourceConnectors{}, nil, cleanup, err
	}
	for _, c := range connectors {
		// Web connectors carry URLs, not paths: policy is enforced by the
		// connector's pinned dialer at fetch time (and at config write time),
		// so nothing needs resolving here.
		if c.Kind == connector.KindWeb {
			if len(spec.WebURLs) > 0 {
				return fail2(fmt.Errorf("workspace %s has multiple web connectors; only one is supported", run.WorkspaceSlug))
			}
			spec.WebURLs = webconn.URLsFrom(c.Config)
			ids.Web = c.ID
			if len(spec.WebURLs) == 0 {
				return fail2(fmt.Errorf("connector %s (web) has no urls configured", c.Name))
			}
			continue
		}

		// An upload connector without a path is in files mode: its material
		// is the workspace's uploaded files, staged from the blob store.
		if path, _ := c.Config["path"].(string); c.Kind == connector.KindUpload && path == "" {
			if spec.DocsDir != "" {
				return fail2(fmt.Errorf("workspace %s has multiple upload connectors; only one is supported", run.WorkspaceSlug))
			}
			dir, blobKeys, skipped, unreadable, shas, clean, err := w.materializeFiles(ctx, run.WorkspaceID)
			if clean != nil {
				cleanups = append(cleanups, clean)
			}
			if err != nil {
				return fail2(fmt.Errorf("connector %s (upload): %w", c.Name, err))
			}
			staged = shas
			// Documents whose bytes are gone are an operational incident, not
			// a decision anyone made. The build continues without them; the
			// review queue is where the wiki asks its humans about exactly
			// this kind of thing, and their pages stay untouched until one
			// answers.
			if len(unreadable) > 0 {
				w.reportUnreadableDocuments(ctx, run, c, unreadable)
			}
			spec.DocsDir = dir
			spec.BlobKeys = blobKeys
			spec.SkippedKeys = skipped
			ids.Upload = c.ID
			continue
		}

		var (
			resolved string
			gitToken string
			err      error
		)
		if remoteURL, _ := c.Config["url"].(string); remoteURL != "" && c.Kind == connector.KindGit {
			var clean func()
			resolved, gitToken, clean, err = w.cloneRemote(ctx, c, remoteURL)
			if clean != nil {
				cleanups = append(cleanups, clean)
			}
			if err != nil {
				return fail2(fmt.Errorf("connector %s (%s): %w", c.Name, c.Kind, err))
			}
		} else {
			path, _ := c.Config["path"].(string)
			if path == "" {
				return fail2(fmt.Errorf("connector %s (%s) has no path configured", c.Name, c.Kind))
			}
			// SECURITY: this config arrived through the API or the database,
			// not the operator's command line. The allowlist is what keeps it
			// from being a read of arbitrary directories the process can see.
			resolved, err = AllowedPath(path, w.PermittedSourceRoots)
			if err != nil {
				return fail2(fmt.Errorf("connector %s (%s): %w", c.Name, c.Kind, err))
			}
		}

		switch c.Kind {
		case connector.KindGit:
			if spec.Path != "" {
				return fail2(fmt.Errorf("workspace %s has multiple git connectors; only one is supported", run.WorkspaceSlug))
			}
			spec.Path = resolved
			spec.GitToken = gitToken
			ids.Git = c.ID
		case connector.KindUpload:
			if spec.DocsDir != "" {
				return fail2(fmt.Errorf("workspace %s has multiple upload connectors; only one is supported", run.WorkspaceSlug))
			}
			spec.DocsDir = resolved
			ids.Upload = c.ID
		default:
			return fail2(fmt.Errorf("connector %s has unsupported kind %q", c.Name, c.Kind))
		}
	}
	return spec, ids, staged, cleanup, nil
}

// materializeFiles stages every enabled workspace file from the blob store
// into a temp directory under its stored relative path, so the upload
// connector extracts it exactly as it would a local folder. Zero files is a
// valid, empty sync. The returned map carries each staged document's unit key
// to its blob key, for source-record attribution and the deletion cascade;
// skipped lists the unit keys of files left out of this sync -- paused ones,
// whose absence is deliberate, and unreadable ones, whose absence is an
// incident. Both must be reported as skipped rather than missing, or the
// deletion cascade would offer to delete their pages. staged is the sha256
// of each file that was staged, by stored path: a skip the extractor reports
// is recorded against the bytes it read, not against whatever the row holds
// by the time the sink runs.
//
// A blob that cannot be read does not fail the run. One unreadable document
// is data loss for that document; failing the whole build would also stop the
// repository, the web pages, and every other document from ingesting, turning
// a storage incident into a total outage. The failure is returned so the
// caller can surface it where humans look.
func (w *Worker) materializeFiles(ctx context.Context, workspaceID string) (dir string, blobKeys map[string][]string, skipped []diff.Key, unreadable []string, staged map[string]string, cleanup func(), err error) {
	if w.Blobs == nil {
		return "", nil, nil, nil, nil, nil, fmt.Errorf(
			"object storage is not configured on this worker; set storage.* (or give the connector a path under permitted_source_roots)")
	}
	files, err := w.Store.ListFiles(ctx, workspaceID)
	if err != nil {
		return "", nil, nil, nil, nil, nil, err
	}

	dir, err = os.MkdirTemp("", "kiln-files-")
	if err != nil {
		return "", nil, nil, nil, nil, nil, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	// Even though stored paths were sanitized at upload time, staging goes
	// through an os.Root jail so a corrupted row cannot write outside the
	// staging directory.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return dir, nil, nil, nil, nil, cleanup, err
	}
	defer root.Close()

	blobKeys = make(map[string][]string, len(files))
	staged = make(map[string]string, len(files))
	for _, f := range files {
		key := diff.DocKey(diff.UploadOrigin(f.Path))
		if !f.Enabled {
			skipped = append(skipped, key)
			continue
		}
		if err := stageBlob(ctx, w.Blobs, root, f); err != nil {
			// Cancellation is not a storage problem: it means the worker is
			// draining, and every remaining file would "fail" the same way.
			// Report it as the interruption it is.
			if ctx.Err() != nil {
				return dir, nil, nil, nil, nil, cleanup, ctx.Err()
			}
			unreadable = append(unreadable, f.Path)
			skipped = append(skipped, key)
			continue
		}
		blobKeys[string(key)] = append(blobKeys[string(key)], f.BlobKey)
		staged[f.Path] = f.SHA256
	}
	return dir, blobKeys, skipped, unreadable, staged, cleanup, nil
}

// reportUnreadableDocuments records a storage incident where an operator will
// see it: the connector's last_error (visible on the Ingestion page) and the
// review queue. Deliberately not a run failure -- the rest of the bench built
// fine, and marking the run failed would hide that.
func (w *Worker) reportUnreadableDocuments(ctx context.Context, run *store.QueuedRun, c store.ConnectorRow, paths []string) {
	log := w.logger().With("run", run.ID, "workspace", run.WorkspaceSlug)
	log.Error("documents could not be read from object storage; skipped for this build",
		"connector", c.Name, "count", len(paths), "paths", paths)

	detail := fmt.Sprintf(
		"%d uploaded document(s) are recorded on this bench but their content could not be read from object storage:\n\n  %s\n\n"+
			"They were skipped for this build, so their pages are untouched and nothing was deleted. "+
			"This usually means the storage bucket lost objects, or storage.* now points somewhere else. "+
			"Re-upload the documents, or remove them from the Ingestion page if they are no longer wanted.",
		len(paths), strings.Join(paths, "\n  "))

	if err := w.Store.FileReview(ctx, run.WorkspaceID, "storage",
		fmt.Sprintf("%d document(s) unreadable from storage", len(paths)), detail); err != nil {
		log.Error("filing storage review failed", "error", err)
	}
}

// stageBlob copies one stored file into the staging jail.
//
// A copy that fails partway takes its half-written file with it. The caller
// treats a staging failure as "this document could not be read" and carries
// on building without it -- but the extractor reads the staging directory,
// not the file list, so a truncated file left behind would be ingested as
// though it were the whole document. The bench would then hold a page
// written from half a document while the review queue reported that same
// document as skipped.
func stageBlob(ctx context.Context, blobs blob.Store, root *os.Root, f store.FileRow) error {
	rc, err := blobs.Get(ctx, f.BlobKey)
	if err != nil {
		return err
	}
	defer rc.Close()

	name := filepath.FromSlash(f.Path)
	if parent := filepath.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	dst, err := root.Create(name)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, rc); err != nil {
		_ = dst.Close()
		_ = root.Remove(name)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = root.Remove(name)
		return err
	}
	return nil
}

// cloneRemote materializes a shallow clone of a connector's https remote.
// Authentication prefers an App installation token (short-lived, repo-scoped,
// nothing durable to leak) and falls back to a sealed credential decrypted
// just-in-time. Either way the plaintext lives only in this frame and the
// clone subprocess's environment. The token is also returned,
// because an incremental build must fetch with it again to deepen the clone;
// the caller keeps it in memory only, for the length of the run.
func (w *Worker) cloneRemote(ctx context.Context, c store.ConnectorRow, remoteURL string) (dir, token string, cleanup func(), err error) {
	switch {
	case installationID(c.Config) != 0:
		if !w.GitHub.AppConfigured() {
			return "", "", nil, fmt.Errorf(
				"connector names github installation %d, but no GitHub App is configured (set github.app_id and KILN_GITHUB_PRIVATE_KEY)",
				installationID(c.Config))
		}
		if err := w.checkInstallation(ctx, c); err != nil {
			return "", "", nil, err
		}
		token, _, err = w.GitHub.InstallationToken(ctx, installationID(c.Config))
		if err != nil {
			return "", "", nil, err
		}
	case c.CredentialID != "":
		token, err = w.openCredential(ctx, c.CredentialID)
		if err != nil {
			return "", "", nil, err
		}
	}

	staging, err := os.MkdirTemp("", "kiln-clone-")
	if err != nil {
		return "", "", nil, fmt.Errorf("clone staging: %w", err)
	}
	cleanup = func() { os.RemoveAll(staging) }

	if err := gitconn.CloneShallow(ctx, gitconn.CloneOptions{
		URL: remoteURL, Token: token, Dir: staging,
		Branch: configuredBranch(c.Config),
	}); err != nil {
		return "", "", cleanup, err
	}
	return staging, token, cleanup, nil
}

// configuredBranch reads a git connector's optional branch. Empty means the
// remote's default branch, which is also what its push webhook is matched
// against (see the api package's connectorBranch).
func configuredBranch(cfg map[string]any) string {
	b, _ := cfg["branch"].(string)
	return strings.TrimSpace(b)
}

// checkInstallation refuses to mint a token for an installation nobody behind
// the workspace can use. The API already requires the person writing the
// connector to hold the installation, but a connector row outlives that
// moment (the member may leave, lose access on GitHub, or the row may predate
// the check), and an installation id is a bearer reference to someone's
// private repositories. So at clone time at least one current member of the
// workspace's org, or an instance admin, must have it linked.
//
// With auth disabled there are no users and nothing to link, so the check is
// skipped; that mode is loopback-only and treats every caller as an admin.
func (w *Worker) checkInstallation(ctx context.Context, c store.ConnectorRow) error {
	if w.AuthDisabled {
		return nil
	}
	id := installationID(c.Config)
	ok, err := w.Store.InstallationServesWorkspace(ctx, c.WorkspaceID, id)
	if err != nil {
		return fmt.Errorf("checking github installation %d: %w", id, err)
	}
	if !ok {
		return fmt.Errorf(
			"connector %q names github installation %d, which no current member of this workspace's org has access to; "+
				"have an owner or instance admin who can reach it sign in with GitHub, or point the connector at an installation you control",
			c.Name, id)
	}
	return nil
}

// installationID reads a connector's github installation reference. JSON
// numbers decode as float64; ids fit comfortably below the 2^53 boundary.
func installationID(cfg map[string]any) int64 {
	switch v := cfg["installation_id"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// openCredential loads and opens a sealed credential. This is the only call
// path in kiln that turns ciphertext back into a secret.
func (w *Worker) openCredential(ctx context.Context, id string) (string, error) {
	sealed, err := w.Store.LoadSealedCredential(ctx, id)
	if err != nil {
		return "", err
	}
	if sealed.Kind != "git_pat" {
		return "", fmt.Errorf("credential %s has kind %q; git clones need git_pat", id, sealed.Kind)
	}
	keyring, err := crypto.NewKeyring(w.MasterKey)
	if err != nil {
		return "", fmt.Errorf("credential %s cannot be opened: %w", id, err)
	}
	secret, err := keyring.Open(sealed.Ciphertext, sealed.Nonce)
	if err != nil {
		return "", fmt.Errorf("credential %s: %w (was the master key rotated?)", id, err)
	}
	return string(secret), nil
}

func (w *Worker) failRun(ctx context.Context, runID string, cause error, log *slog.Logger) {
	// The run row must not stay 'running' even when the caller's context is
	// gone, or the workspace stays blocked until the stale deadline.
	err := w.Store.FailRun(context.WithoutCancel(ctx), runID, w.ID, cause.Error())
	switch {
	case errors.Is(err, store.ErrClaimLost):
		log.Error("run failure not recorded: the run is no longer claimed by this worker",
			"cause", cause.Error())
	case err != nil:
		log.Error("recording run failure failed", "error", err)
	}
}

// requeueInterrupted is the end of a run the drain deadline stopped: it logs
// msg, hands the run back to the queue and records the outcome as "requeued".
// Callers have already ruled out a lost claim (lostClaim), which is answered
// with abandon instead.
//
// The queue write is fenced: if the run is no longer ours -- it went stale and
// was claimed again while we were draining -- requeueing would pull the new
// owner's live build out from under it, so the store refuses, and the refusal
// is logged as what it is rather than as a database failure.
func (w *Worker) requeueInterrupted(ctx context.Context, runID string, log *slog.Logger, started time.Time, msg string) {
	log.Warn(msg)
	err := w.Store.RequeueRun(context.WithoutCancel(ctx), runID, w.ID)
	switch {
	case errors.Is(err, store.ErrClaimLost):
		log.Error("run not requeued: it is no longer claimed by this worker")
	case err != nil:
		log.Error("requeue after interruption failed", "error", err)
	}
	w.metrics().RunFinished("requeued", time.Since(started), 0, 0, 0, 0)
}

// lostClaim reports whether the run's context was canceled because the
// heartbeat found the run no longer belongs to this worker. It is read from the
// cancellation cause, not inferred from ctx.Err(), so a lost claim cannot be
// mistaken for a drain interruption and answered with a requeue.
func lostClaim(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), store.ErrClaimLost)
}

// abandon is the end of a run this worker no longer owns. It writes nothing:
// the run's row, items and connector bookkeeping are the current owner's, and a
// loser touching them is the corruption fencing exists to prevent.
func (w *Worker) abandon(log *slog.Logger, started time.Time) {
	log.Error("run claim lost; abandoning without recording a result -- another worker may have taken this run, and this build's spend is not part of it",
		"worker", w.ID)
	w.metrics().RunFinished("claim_lost", time.Since(started), 0, 0, 0, 0)
}

// heartbeat keeps this worker's claim on a run alive while it builds, and
// returns the function that stops it.
//
// Without it a claim is only as fresh as the moment it was made, and a run is
// many sequential paid agent calls: a healthy long build would cross
// stale_after, be requeued, and be built a second time by another worker while
// the first is still spending.
//
// If a beat finds the run is no longer ours, the run's context is canceled with
// store.ErrClaimLost as its cause. Transient database errors only log: one
// missed beat is what stale_after's headroom is for, and abandoning a healthy
// build over a hiccup would waste exactly the money this protects.
func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, runID string, log *slog.Logger) (stop func()) {
	interval := w.heartbeatInterval()
	beatCtx, stopBeats := context.WithCancel(ctx)
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case <-tick.C:
			}
			// Bounded by the interval so a hung query cannot hold up the next
			// beat, nor the stop.
			callCtx, done := context.WithTimeout(beatCtx, interval)
			err := w.Store.HeartbeatRun(callCtx, runID, w.ID)
			done()
			switch {
			case err == nil:
			case errors.Is(err, store.ErrClaimLost):
				log.Error("heartbeat found the run no longer claimed by this worker; canceling it",
					"worker", w.ID)
				cancel(store.ErrClaimLost)
				return
			case beatCtx.Err() != nil:
				return
			default:
				log.Warn("heartbeat failed; will retry", "error", err)
			}
		}
	}()

	return func() {
		stopBeats()
		<-finished
	}
}

// heartbeatInterval is how often the claim is refreshed. Left unset it is a
// third of StaleAfter, capped at a minute: frequent enough that several beats
// fit inside the deadline, and rare enough that the update is invisible.
func (w *Worker) heartbeatInterval() time.Duration {
	const ceiling = time.Minute
	if w.HeartbeatInterval > 0 {
		return w.HeartbeatInterval
	}
	if w.StaleAfter > 0 {
		return min(max(w.StaleAfter/3, time.Millisecond), ceiling)
	}
	return ceiling
}

// enqueueContinuation keeps a bench converging without operator attention.
// Two cases: units deferred by the per-run page cap get a follow-up, and a
// partial run gets exactly one retry — a continuation that goes partial
// again stops, because retrying a persistent failure in a loop is a bill,
// not a fix. Continuations carry no ref: the full hash-gated route is what
// rescues units an incremental range would no longer visit.
//
// A deferral only earns its follow-up when the run landed something. The
// chain terminates because each round strictly shrinks the stale set, but
// only success shrinks it: a failed unit keeps its old hash and is planned
// again next time. A run where nothing succeeded would therefore defer the
// same units, enqueue the same continuation, and fail the same way -- an
// unbounded chain of paid runs converging on nothing. Zero progress is
// exactly the case where the next round is provably identical to this one,
// so it is where the chain has to stop and ask for a human.
func (w *Worker) enqueueContinuation(ctx context.Context, run *store.QueuedRun, res *jobs.BuildResult, log *slog.Logger) {
	retryPartial := res.Summary.Status == jobs.StatusPartial && run.Trigger != jobs.TriggerContinuation
	continueDeferred := res.Deferred > 0 && progressed(res)
	if res.Deferred > 0 && !continueDeferred {
		log.Warn("run deferred work but completed no unit; not continuing",
			"deferred", res.Deferred, "status", res.Summary.Status)
	}
	if !continueDeferred && !retryPartial {
		return
	}
	if _, created, err := w.Store.EnqueueRun(ctx, run.WorkspaceID, jobs.TriggerContinuation, run.ConnectorID); err != nil {
		log.Error("continuation enqueue failed", "error", err)
	} else if created {
		log.Info("continuation enqueued", "deferred", res.Deferred, "retry_partial", retryPartial)
	}
}

// continueInterrupted queues the follow-up for a build the drain deadline cut
// short. It is not enqueueContinuation, and does not share its gates: those
// exist to stop a chain of runs that fail the same way, and an interruption is
// not a failure of the work. Even a build that finished nothing is continued,
// because the cause was the deploy, not the units. The chain is bounded by the
// deploys that interrupt it.
//
// The enqueue is detached from ctx for the same reason the requeue is: ctx is
// canceled by definition on this path, and a write through it would fail and
// leave the unreached units waiting for some later trigger.
func (w *Worker) continueInterrupted(ctx context.Context, run *store.QueuedRun, log *slog.Logger) {
	id, created, err := w.Store.EnqueueRun(context.WithoutCancel(ctx), run.WorkspaceID, jobs.TriggerContinuation, run.ConnectorID)
	switch {
	case err != nil:
		log.Error("continuation after interruption not enqueued; the unreached units wait for the next trigger",
			"error", err)
	case created:
		log.Info("continuation enqueued after interruption", "continuation", id)
	}
}

// progressed reports whether any unit succeeded. That is what advances a
// source record's input hash, and so the only thing that makes the next
// run's plan smaller than this one's.
func progressed(res *jobs.BuildResult) bool {
	for _, it := range res.Summary.Items {
		if it.Status == jobs.StatusSucceeded {
			return true
		}
	}
	return false
}

// warnNearBudget files a review when a workspace has spent 80% or more of
// its budget within the rolling window. The queue humans already watch is
// where the warning lands, deduplicated so a workspace hovering near the
// line asks once, and the hard stop at 100% (enforced at enqueue by the API)
// never arrives unannounced.
func (w *Worker) warnNearBudget(ctx context.Context, workspaceID string, log *slog.Logger) {
	if w.BudgetWindow <= 0 {
		return
	}
	budget, err := w.Store.WorkspaceBudgetUSD(ctx, workspaceID)
	if err != nil || budget == nil || *budget <= 0 {
		if err != nil {
			log.Error("budget lookup failed", "error", err)
		}
		return
	}
	spent, err := w.Store.SpendInWindow(ctx, workspaceID, w.BudgetWindow)
	if err != nil {
		log.Error("spend lookup failed", "error", err)
		return
	}
	if spent < *budget*0.8 {
		return
	}
	detail := fmt.Sprintf(
		"$%.2f of the $%.2f budget is spent in the current %s window. "+
			"At 100%% new runs are refused until the window rolls on; raise the bench budget if this pace is intended.",
		spent, *budget, w.BudgetWindow)
	if err := w.Store.FileReview(ctx, workspaceID, "budget", "budget window nearly exhausted", detail); err != nil {
		log.Error("filing budget warning failed", "error", err)
	}
}

// dutyRunner returns the scheduler that runs the periodic duties inline,
// building it from the worker's own fields on first use. Lazily, rather than
// in New, so a Worker assembled as a struct literal runs the same duties as
// one from New. FirstSweepDelay stays zero: a worker has always swept at
// start, and a scheduler deployment turns the worker's duties off anyway.
func (w *Worker) dutyRunner() *scheduler.Scheduler {
	if w.duties == nil {
		c := scheduler.Config{
			Store:               w.Store,
			Log:                 w.logger().With("worker", w.ID),
			Metrics:             w.metrics(),
			StaleAfter:          w.StaleAfter,
			SourcePollInterval:  w.SourcePollInterval,
			SoftDeleteRetention: w.SoftDeleteRetention,
			RunRetention:        w.RunRetention,
			Embedded:            true,
		}
		// Assigned only when set: a nil blob.Store inside a non-nil
		// interface would read as "has object storage".
		if w.Blobs != nil {
			c.Blobs = w.Blobs
		}
		w.duties = scheduler.New(c)
	}
	return w.duties
}

func (w *Worker) poll() time.Duration {
	if w.Poll > 0 {
		return w.Poll
	}
	return 5 * time.Second
}

func (w *Worker) drainGrace() time.Duration {
	if w.DrainGrace > 0 {
		return w.DrainGrace
	}
	return 15 * time.Minute // above one agent call's timeout
}

func (w *Worker) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
