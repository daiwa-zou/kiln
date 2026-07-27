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
	"log/slog"
	"os"
	"time"

	"github.com/daiwa-zou/kiln/internal/config"
	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	webconn "github.com/daiwa-zou/kiln/internal/connector/web"
	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/github"
	"github.com/daiwa-zou/kiln/internal/jobs"
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
	LastSuccessfulRef(ctx context.Context, workspaceID string) (string, error)
	WorkspaceBudgetUSD(ctx context.Context, workspaceID string) (*float64, error)
	SpendInWindow(ctx context.Context, workspaceID string, window time.Duration) (float64, error)
	FileReview(ctx context.Context, workspaceID, kind, title, detail string) error
	RequeueRun(ctx context.Context, runID string) error
	Sweep(ctx context.Context, softDeleteRetention, runRetention time.Duration) (int64, error)
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
			return
		}
		log.Error("run not executable", "error", err)
		w.failRun(ctx, run.ID, err, log)
		// A run pinned to one connector can attribute the failure to it;
		// workspace-wide resolution failures have no single owner.
		if run.ConnectorID != "" {
			if merr := w.Store.MarkConnectorSync(ctx, run.ConnectorID, err.Error()); merr != nil {
				log.Error("mark connector sync failed", "error", merr)
			}
		}
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
	case err != nil:
		syncErr = err.Error()
		log.Error("build failed", "error", err)
		w.failRun(ctx, run.ID, err, log)
	default:
		log.Info("build finished", "status", res.Summary.Status,
			"cost_usd", res.Summary.CostUSD,
			"created", res.Summary.Created, "updated", res.Summary.Updated)
		w.warnNearBudget(ctx, run.WorkspaceID, log)
		w.enqueueContinuation(ctx, run, res, log)
	}
	for _, id := range []string{connectors.Git, connectors.Upload, connectors.Web} {
		if id == "" {
			continue
		}
		if merr := w.Store.MarkConnectorSync(ctx, id, syncErr); merr != nil {
			log.Error("mark connector sync failed", "error", merr)
		}
	}
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
	var cleanup func()
	for _, c := range connectors {
		// Web connectors carry URLs, not paths: policy is enforced by the
		// connector's pinned dialer at fetch time (and at config write time),
		// so nothing needs resolving here.
		if c.Kind == "web" {
			if len(spec.WebURLs) > 0 {
				return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf("workspace %s has multiple web connectors; only one is supported", run.WorkspaceSlug)
			}
			spec.WebURLs = webconn.URLsFrom(c.Config)
			ids.Web = c.ID
			if len(spec.WebURLs) == 0 {
				return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf("connector %s (web) has no urls configured", c.Name)
			}
			continue
		}

		var (
			resolved string
			err      error
		)
		if remoteURL, _ := c.Config["url"].(string); remoteURL != "" && c.Kind == "git" {
			resolved, cleanup, err = w.cloneRemote(ctx, c, remoteURL)
			if err != nil {
				return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf("connector %s (%s): %w", c.Name, c.Kind, err)
			}
		} else {
			path, _ := c.Config["path"].(string)
			if path == "" {
				return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf("connector %s (%s) has no path configured", c.Name, c.Kind)
			}
			// SECURITY: this config arrived through the API or the database,
			// not the operator's command line. The allowlist is what keeps it
			// from being a read of arbitrary directories the process can see.
			resolved, err = AllowedPath(path, w.PermittedSourceRoots)
			if err != nil {
				return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf("connector %s (%s): %w", c.Name, c.Kind, err)
			}
		}

		switch c.Kind {
		case "git":
			if spec.Path != "" {
				return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf("workspace %s has multiple git connectors; only one is supported", run.WorkspaceSlug)
			}
			spec.Path = resolved
			ids.Git = c.ID
		case "upload":
			if spec.DocsDir != "" {
				return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf("workspace %s has multiple upload connectors; only one is supported", run.WorkspaceSlug)
			}
			spec.DocsDir = resolved
			ids.Upload = c.ID
		default:
			return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf("connector %s has unsupported kind %q", c.Name, c.Kind)
		}
	}
	if spec.Path == "" {
		return jobs.SourceSpec{}, jobs.SourceConnectors{}, cleanup, fmt.Errorf(
			"workspace %s has no git connector; a build needs a repository to scan", run.WorkspaceSlug)
	}
	return spec, ids, cleanup, nil
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
// Two cases: units deferred by the per-run page cap always get a follow-up
// (the deferral strictly shrinks each round, so the chain terminates), and a
// partial run gets exactly one retry — a continuation that goes partial
// again stops, because retrying a persistent failure in a loop is a bill,
// not a fix. Continuations carry no ref: the full hash-gated route is what
// rescues units an incremental range would no longer visit.
func (w *Worker) enqueueContinuation(ctx context.Context, run *store.QueuedRun, res *jobs.BuildResult, log *slog.Logger) {
	retryPartial := res.Summary.Status == jobs.StatusPartial && run.Trigger != "continuation"
	if res.Deferred == 0 && !retryPartial {
		return
	}
	if _, created, err := w.Store.EnqueueRun(ctx, run.WorkspaceID, "continuation", run.ConnectorID); err != nil {
		log.Error("continuation enqueue failed", "error", err)
	} else if created {
		log.Info("continuation enqueued", "deferred", res.Deferred, "retry_partial", retryPartial)
	}
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

func (w *Worker) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
