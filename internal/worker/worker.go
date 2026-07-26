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

	// PermittedSourceRoots is the local-path allowlist for database-configured
	// connectors. Empty denies every local path: connector configs are
	// API-writable data, and an unchecked path is a local-file-inclusion
	// primitive.
	PermittedSourceRoots []string
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
		PermittedSourceRoots: cfg.Worker.PermittedSourceRoots,
	}
}

// Run claims and executes runs until the context is canceled. The error is
// always ctx.Err(): queue and build failures are recorded on their runs and
// logged, never allowed to kill the loop, because one poisoned run must not
// take the worker down with it.
func (w *Worker) Run(ctx context.Context) error {
	log := w.logger().With("worker", w.ID)
	log.Info("worker started", "poll", w.poll().String())

	for {
		if err := ctx.Err(); err != nil {
			log.Info("worker stopping")
			return err
		}

		w.requeueStale(ctx, log)

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

		w.process(ctx, run)
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

	spec, connectorID, err := w.sourceSpec(ctx, run)
	if err != nil {
		log.Error("run not executable", "error", err)
		w.failRun(ctx, run.ID, err, log)
		return
	}

	res, err := w.Pipeline.Execute(ctx, jobs.ExecuteRequest{
		RunID:       run.ID,
		WorkspaceID: run.WorkspaceID,
		Trigger:     run.Trigger,
		Source:      spec,
	})

	syncErr := ""
	if err != nil {
		syncErr = err.Error()
		log.Error("build failed", "error", err)
		w.failRun(ctx, run.ID, err, log)
	} else {
		log.Info("build finished", "status", res.Summary.Status,
			"cost_usd", res.Summary.CostUSD,
			"created", res.Summary.Created, "updated", res.Summary.Updated)
	}
	if connectorID != "" {
		if merr := w.Store.MarkConnectorSync(ctx, connectorID, syncErr); merr != nil {
			log.Error("mark connector sync failed", "error", merr)
		}
	}
}

// sourceSpec resolves a run's connectors into build material. A run pinned to
// a connector uses that one; otherwise every enabled connector on the
// workspace feeds the build, merged into one map like the CLI's --docs flag.
// The returned connector id is the git connector's, for sync bookkeeping.
func (w *Worker) sourceSpec(ctx context.Context, run *store.QueuedRun) (jobs.SourceSpec, string, error) {
	var connectors []store.ConnectorRow
	if run.ConnectorID != "" {
		c, err := w.Store.ConnectorByID(ctx, run.ConnectorID)
		if err != nil {
			return jobs.SourceSpec{}, "", err
		}
		connectors = []store.ConnectorRow{*c}
	} else {
		var err error
		connectors, err = w.Store.EnabledConnectors(ctx, run.WorkspaceID)
		if err != nil {
			return jobs.SourceSpec{}, "", err
		}
	}
	if len(connectors) == 0 {
		return jobs.SourceSpec{}, "", fmt.Errorf(
			"workspace %s has no enabled connector; configure one before queueing runs", run.WorkspaceSlug)
	}

	spec := jobs.SourceSpec{Slug: run.WorkspaceSlug}
	gitConnectorID := ""
	for _, c := range connectors {
		path, _ := c.Config["path"].(string)
		if path == "" {
			return jobs.SourceSpec{}, "", fmt.Errorf("connector %s (%s) has no path configured", c.Name, c.Kind)
		}
		// SECURITY: this config arrived through the API or the database, not
		// the operator's command line. The allowlist is what keeps it from
		// being a read of arbitrary directories the process can see.
		resolved, err := AllowedPath(path, w.PermittedSourceRoots)
		if err != nil {
			return jobs.SourceSpec{}, "", fmt.Errorf("connector %s (%s): %w", c.Name, c.Kind, err)
		}

		switch c.Kind {
		case "git":
			if spec.Path != "" {
				return jobs.SourceSpec{}, "", fmt.Errorf("workspace %s has multiple git connectors; only one is supported", run.WorkspaceSlug)
			}
			spec.Path = resolved
			gitConnectorID = c.ID
		case "upload":
			if spec.DocsDir != "" {
				return jobs.SourceSpec{}, "", fmt.Errorf("workspace %s has multiple upload connectors; only one is supported", run.WorkspaceSlug)
			}
			spec.DocsDir = resolved
		default:
			return jobs.SourceSpec{}, "", fmt.Errorf("connector %s has unsupported kind %q", c.Name, c.Kind)
		}
	}
	if spec.Path == "" {
		return jobs.SourceSpec{}, "", fmt.Errorf(
			"workspace %s has no git connector; a build needs a repository to scan", run.WorkspaceSlug)
	}
	return spec, gitConnectorID, nil
}

func (w *Worker) failRun(ctx context.Context, runID string, cause error, log *slog.Logger) {
	// The run row must not stay 'running' even when the caller's context is
	// gone, or the workspace stays blocked until the stale deadline.
	if err := w.Store.FailRun(context.WithoutCancel(ctx), runID, cause.Error()); err != nil {
		log.Error("recording run failure failed", "error", err)
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

func (w *Worker) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
