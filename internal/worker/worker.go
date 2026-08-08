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
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/config"
	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	webconn "github.com/daiwa-zou/kiln/internal/connector/web"
	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/github"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

// Store is what the worker needs from persistence, declared here so the loop
// is testable against a fake while the SQL stays in internal/store.
type Store interface {
	ClaimNextRun(ctx context.Context, workerID string) (*store.QueuedRun, error)
	FailRun(ctx context.Context, runID, message string) error
	RequeueStaleRuns(ctx context.Context, olderThan time.Duration) (int, error)
	EnabledConnectors(ctx context.Context, workspaceID string) ([]store.ConnectorRow, error)
	ConnectorByID(ctx context.Context, id string) (*store.ConnectorRow, error)
	MarkConnectorSync(ctx context.Context, id, syncErr string) error
	LoadSealedCredential(ctx context.Context, id string) (*store.SealedCredential, error)
	PollDueConnectors(ctx context.Context, olderThan time.Duration) ([]store.ConnectorRow, error)
	EnqueueRun(ctx context.Context, workspaceID, trigger, connectorID string) (string, bool, error)
	ListFiles(ctx context.Context, workspaceID string) ([]store.FileRow, error)
	LastSuccessfulRef(ctx context.Context, workspaceID string) (string, error)
	WorkspaceBudgetUSD(ctx context.Context, workspaceID string) (*float64, error)
	SpendInWindow(ctx context.Context, workspaceID string, window time.Duration) (float64, error)
	FileReview(ctx context.Context, workspaceID, kind, title, detail string) error
	ResearchQuestionFor(ctx context.Context, runID string) (*store.ResearchQuestion, error)
	RecordResearch(ctx context.Context, reviewID, findings string, resolved bool) error
	RequeueRun(ctx context.Context, runID string) error
	Sweep(ctx context.Context, softDeleteRetention, runRetention time.Duration) (int64, error)
	QueueDepth(ctx context.Context) (map[string]int, error)
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

	// SourcePollInterval is how often trigger_mode='poll' connectors are due
	// for a refresh, for origins with no useful webhook. Zero disables the
	// scheduler.
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
	lastSweep           time.Time

	// lastQueueSample throttles the queue-depth gauge; see sampleQueueDepth.
	lastQueueSample time.Time

	// lastSourcePoll throttles the scheduler to roughly one sweep per
	// interval regardless of how fast the claim loop spins.
	lastSourcePoll time.Time

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
		SourcePollInterval:   cfg.Worker.SourcePollInterval,
		BudgetWindow:         cfg.Agent.BudgetWindow,
		DrainGrace:           cfg.Worker.DrainGrace,
		SoftDeleteRetention:  cfg.Storage.SoftDeleteRetention,
		RunRetention:         cfg.Storage.RunArtifactRetention,
		PermittedSourceRoots: cfg.Worker.PermittedSourceRoots,
		MasterKey:            cfg.Secrets.MasterKey,
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
// requeued, not failed: a rolling deploy must never turn an in-flight build
// into a permanent failure plus wasted spend.
func (w *Worker) Run(ctx context.Context) error {
	log := w.logger().With("worker", w.ID)
	log.Info("worker started", "poll", w.poll().String())

	for {
		if err := ctx.Err(); err != nil {
			log.Info("worker stopping")
			return err
		}

		w.requeueStale(ctx, log)
		w.pollSources(ctx, log)
		w.sweep(ctx, log)
		w.sampleQueueDepth(ctx, log)

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

	// Counted at claim rather than at execution: a run that fails to resolve
	// its sources never reaches the pipeline, and those are precisely the
	// failures worth alerting on (a bad connector, unreachable storage).
	// Instrumenting later would make them invisible.
	started := time.Now()
	w.metrics().RunStarted(run.Trigger)

	spec, connectors, cleanup, err := w.sourceSpec(ctx, run)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		if ctx.Err() != nil {
			// Interrupted while resolving, not misconfigured: requeue.
			log.Warn("resolution interrupted by shutdown; requeuing")
			if rerr := w.Store.RequeueRun(context.WithoutCancel(ctx), run.ID); rerr != nil {
				log.Error("requeue after interruption failed", "error", rerr)
			}
			w.metrics().RunFinished("requeued", time.Since(started), 0, 0, 0, 0)
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
	if run.Trigger == "research" {
		w.research(ctx, run, spec, started)
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
	})

	syncErr := ""
	switch {
	case err != nil && ctx.Err() != nil:
		// Interrupted by the drain deadline, not broken: back to the queue,
		// where this or another worker picks it up cleanly.
		syncErr = "interrupted by shutdown; requeued"
		log.Warn("build interrupted by shutdown; requeuing")
		if rerr := w.Store.RequeueRun(context.WithoutCancel(ctx), run.ID); rerr != nil {
			log.Error("requeue after interruption failed", "error", rerr)
		}
		w.metrics().RunFinished("requeued", time.Since(started), 0, 0, 0, 0)
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

// research executes a claimed research run: read the bench's sources with one
// review item's question in hand and write the findings back onto it.
//
// A failure is written onto the item too, not just onto the run. The item was
// already answerable by a human the whole time -- research never took it out
// of the queue -- so what a failure owes the reader is the reason there are no
// findings attached, which is not something they would otherwise find without
// going to the runs list and matching timestamps.
func (w *Worker) research(ctx context.Context, run *store.QueuedRun, spec jobs.SourceSpec, started time.Time) {
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
		},
	})

	// Write-back outlives cancellation for the same reason the run record
	// does: the call is already billed, and findings nobody can read are the
	// one outcome worse than no findings at all.
	writeCtx := context.WithoutCancel(ctx)

	if err != nil {
		if ctx.Err() != nil {
			// Interrupted by the drain deadline, not broken: back to the
			// queue, and the item is left untouched because the answer is
			// still coming -- from whichever worker picks this up next.
			log.Warn("research interrupted by shutdown; requeuing")
			if rerr := w.Store.RequeueRun(writeCtx, run.ID); rerr != nil {
				log.Error("requeue after interruption failed", "error", rerr)
			}
			w.metrics().RunFinished("requeued", time.Since(started), 0, 0, 0, 0)
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
// the build, for per-source attribution and sync bookkeeping; the cleanup
// (possibly nil) removes any staging a remote clone materialized.
func (w *Worker) sourceSpec(ctx context.Context, run *store.QueuedRun) (jobs.SourceSpec, jobs.SourceConnectors, func(), error) {
	fail := func(err error) (jobs.SourceSpec, jobs.SourceConnectors, func(), error) {
		return jobs.SourceSpec{}, jobs.SourceConnectors{}, nil, err
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
	// More than one connector can stage material (a remote clone and a
	// blob-store materialization in the same run), so cleanups accumulate
	// rather than occupy a single slot that a second stage would overwrite.
	var cleanups []func()
	cleanup := func() {
		for _, fn := range cleanups {
			fn()
		}
	}
	fail2 := func(err error) (jobs.SourceSpec, jobs.SourceConnectors, func(), error) {
		return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, err
	}
	for _, c := range connectors {
		// Web connectors carry URLs, not paths: policy is enforced by the
		// connector's pinned dialer at fetch time (and at config write time),
		// so nothing needs resolving here.
		if c.Kind == "web" {
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
		if path, _ := c.Config["path"].(string); c.Kind == "upload" && path == "" {
			if spec.DocsDir != "" {
				return fail2(fmt.Errorf("workspace %s has multiple upload connectors; only one is supported", run.WorkspaceSlug))
			}
			dir, blobKeys, skipped, unreadable, clean, err := w.materializeFiles(ctx, run.WorkspaceID)
			if clean != nil {
				cleanups = append(cleanups, clean)
			}
			if err != nil {
				return fail2(fmt.Errorf("connector %s (upload): %w", c.Name, err))
			}
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
			err      error
		)
		if remoteURL, _ := c.Config["url"].(string); remoteURL != "" && c.Kind == "git" {
			var clean func()
			resolved, clean, err = w.cloneRemote(ctx, c, remoteURL)
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
		case "git":
			if spec.Path != "" {
				return fail2(fmt.Errorf("workspace %s has multiple git connectors; only one is supported", run.WorkspaceSlug))
			}
			spec.Path = resolved
			ids.Git = c.ID
		case "upload":
			if spec.DocsDir != "" {
				return fail2(fmt.Errorf("workspace %s has multiple upload connectors; only one is supported", run.WorkspaceSlug))
			}
			spec.DocsDir = resolved
			ids.Upload = c.ID
		default:
			return fail2(fmt.Errorf("connector %s has unsupported kind %q", c.Name, c.Kind))
		}
	}
	return spec, ids, cleanup, nil
}

// materializeFiles stages every enabled workspace file from the blob store
// into a temp directory under its stored relative path, so the upload
// connector extracts it exactly as it would a local folder. Zero files is a
// valid, empty sync. The returned map carries each staged document's unit key
// to its blob key, for source-record attribution and the deletion cascade;
// skipped lists the unit keys of files left out of this sync -- paused ones,
// whose absence is deliberate, and unreadable ones, whose absence is an
// incident. Both must be reported as skipped rather than missing, or the
// deletion cascade would offer to delete their pages.
//
// A blob that cannot be read does not fail the run. One unreadable document
// is data loss for that document; failing the whole build would also stop the
// repository, the web pages, and every other document from ingesting, turning
// a storage incident into a total outage. The failure is returned so the
// caller can surface it where humans look.
func (w *Worker) materializeFiles(ctx context.Context, workspaceID string) (dir string, blobKeys map[string][]string, skipped []diff.Key, unreadable []string, cleanup func(), err error) {
	if w.Blobs == nil {
		return "", nil, nil, nil, nil, fmt.Errorf(
			"object storage is not configured on this worker; set storage.* (or give the connector a path under permitted_source_roots)")
	}
	files, err := w.Store.ListFiles(ctx, workspaceID)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}

	dir, err = os.MkdirTemp("", "kiln-files-")
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	// Even though stored paths were sanitized at upload time, staging goes
	// through an os.Root jail so a corrupted row cannot write outside the
	// staging directory.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return dir, nil, nil, nil, cleanup, err
	}
	defer root.Close()

	blobKeys = make(map[string][]string, len(files))
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
				return dir, nil, nil, nil, cleanup, ctx.Err()
			}
			unreadable = append(unreadable, f.Path)
			skipped = append(skipped, key)
			continue
		}
		blobKeys[string(key)] = append(blobKeys[string(key)], f.BlobKey)
	}
	return dir, blobKeys, skipped, unreadable, cleanup, nil
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
// clone subprocess's environment.
func (w *Worker) cloneRemote(ctx context.Context, c store.ConnectorRow, remoteURL string) (dir string, cleanup func(), err error) {
	token := ""
	switch {
	case installationID(c.Config) != 0:
		if !w.GitHub.AppConfigured() {
			return "", nil, fmt.Errorf(
				"connector names github installation %d, but no GitHub App is configured (set github.app_id and KILN_GITHUB_PRIVATE_KEY)",
				installationID(c.Config))
		}
		token, _, err = w.GitHub.InstallationToken(ctx, installationID(c.Config))
		if err != nil {
			return "", nil, err
		}
	case c.CredentialID != "":
		token, err = w.openCredential(ctx, c.CredentialID)
		if err != nil {
			return "", nil, err
		}
	}

	staging, err := os.MkdirTemp("", "kiln-clone-")
	if err != nil {
		return "", nil, fmt.Errorf("clone staging: %w", err)
	}
	cleanup = func() { os.RemoveAll(staging) }

	if err := gitconn.CloneShallow(ctx, gitconn.CloneOptions{
		URL: remoteURL, Token: token, Dir: staging,
	}); err != nil {
		return "", cleanup, err
	}
	return staging, cleanup, nil
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
	if err := w.Store.FailRun(context.WithoutCancel(ctx), runID, cause.Error()); err != nil {
		log.Error("recording run failure failed", "error", err)
	}
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
	retryPartial := res.Summary.Status == jobs.StatusPartial && run.Trigger != "continuation"
	continueDeferred := res.Deferred > 0 && progressed(res)
	if res.Deferred > 0 && !continueDeferred {
		log.Warn("run deferred work but completed no unit; not continuing",
			"deferred", res.Deferred, "status", res.Summary.Status)
	}
	if !continueDeferred && !retryPartial {
		return
	}
	if _, created, err := w.Store.EnqueueRun(ctx, run.WorkspaceID, "continuation", run.ConnectorID); err != nil {
		log.Error("continuation enqueue failed", "error", err)
	} else if created {
		log.Info("continuation enqueued", "deferred", res.Deferred, "retry_partial", retryPartial)
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

// pollSources enqueues runs for poll-triggered connectors that are due. The
// sweep runs at most once per minute: due-ness itself is measured against
// each connector's last sync, so sweeping faster buys nothing.
func (w *Worker) pollSources(ctx context.Context, log *slog.Logger) {
	if w.SourcePollInterval <= 0 || time.Since(w.lastSourcePoll) < time.Minute {
		return
	}
	w.lastSourcePoll = time.Now()

	due, err := w.Store.PollDueConnectors(ctx, w.SourcePollInterval)
	if err != nil {
		log.Error("poll scheduler query failed", "error", err)
		return
	}
	for _, c := range due {
		// The active-run index makes this idempotent: a connector already
		// building debounces onto its waiting run.
		if _, created, err := w.Store.EnqueueRun(ctx, c.WorkspaceID, "poll", c.ID); err != nil {
			log.Error("poll enqueue failed", "connector", c.Name, "error", err)
		} else if created {
			log.Info("poll enqueued run", "connector", c.Name, "workspace", c.WorkspaceID)
		}
	}
}

func (w *Worker) requeueStale(ctx context.Context, log *slog.Logger) {
	stale := w.StaleAfter
	if stale <= 0 {
		return
	}
	n, err := w.Store.RequeueStaleRuns(ctx, stale)
	if err != nil {
		log.Error("requeue stale runs failed", "error", err)
		return
	}
	if n > 0 {
		log.Warn("requeued stale runs from dead workers", "count", n)
	}
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

// sweep enforces retention roughly hourly: soft-deleted pages past their
// window, expired sessions, dead tokens, and finished runs past theirs. The
// knobs and the purge index have existed since M0; this is the purger.
func (w *Worker) sweep(ctx context.Context, log *slog.Logger) {
	if time.Since(w.lastSweep) < time.Hour {
		return
	}
	w.lastSweep = time.Now()
	swept, err := w.Store.Sweep(ctx, w.SoftDeleteRetention, w.RunRetention)
	if err != nil {
		log.Error("gc sweep failed", "error", err)
		return
	}
	if swept > 0 {
		log.Info("gc sweep removed expired rows", "rows", swept)
	}
}

// sampleQueueDepth publishes the queue gauge on a leash rather than on every
// claim-loop iteration. The loop spins as fast as the poll interval (a second
// in some deployments), and a count(*) per worker per second is a database
// load nobody asked for; ten seconds is well inside a Prometheus scrape.
//
// A failure here is logged at debug and otherwise ignored: monitoring must
// never be the thing that stops builds.
func (w *Worker) sampleQueueDepth(ctx context.Context, log *slog.Logger) {
	if time.Since(w.lastQueueSample) < 10*time.Second {
		return
	}
	w.lastQueueSample = time.Now()

	depth, err := w.Store.QueueDepth(ctx)
	if err != nil {
		log.Debug("queue depth sample failed", "error", err)
		return
	}
	w.metrics().SetQueueDepth(depth)
}

func (w *Worker) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
