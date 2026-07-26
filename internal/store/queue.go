package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The queue is the runs table. A run is born 'queued', claimed into 'running'
// by exactly one worker via FOR UPDATE SKIP LOCKED, and finished in place by
// RecordRun. An external queue is deliberately rejected: LLM-bound jobs at
// runs-per-hour throughput gain nothing from one and would lose the
// single-transaction claim+state-change this gives for free.

// QueuedRun is one claimed unit of work, joined with the workspace fields the
// worker needs to build without a second round trip.
type QueuedRun struct {
	ID            string
	WorkspaceID   string
	ConnectorID   string // empty when the run is not pinned to one connector
	Trigger       string
	WorkspaceSlug string
	OrgSlug       string
}

// EnqueueRun files a build request for a workspace. When the workspace already
// has a queued or running run, that run is returned instead and created is
// false: the partial unique index makes double-submission and webhook storms
// collapse into the one run already waiting.
func (s *WikiStore) EnqueueRun(ctx context.Context, workspaceID, trigger, connectorID string) (runID string, created bool, err error) {
	err = s.pool.QueryRow(ctx, `
		INSERT INTO runs (workspace_id, connector_id, trigger, status)
		VALUES ($1, $2, $3, 'queued')
		ON CONFLICT (workspace_id) WHERE status IN ('queued','running') DO NOTHING
		RETURNING id`,
		workspaceID, nullable(connectorID), trigger).Scan(&runID)
	if err == nil {
		return runID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("store: enqueue run: %w", err)
	}

	// Conflict path: surface the active run so the caller can report it.
	err = s.pool.QueryRow(ctx, `
		SELECT id FROM runs
		WHERE workspace_id = $1 AND status IN ('queued','running')
		ORDER BY created_at DESC LIMIT 1`, workspaceID).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The active run finished between the insert and this read; retry once
		// rather than bothering the caller with a race they cannot act on.
		return s.EnqueueRun(ctx, workspaceID, trigger, connectorID)
	}
	if err != nil {
		return "", false, fmt.Errorf("store: find active run: %w", err)
	}
	return runID, false, nil
}

// ClaimNextRun atomically claims the oldest queued run for this worker.
// Returns nil when the queue is empty. Claim and state change are one
// statement, so two workers racing on the same row is impossible: SKIP LOCKED
// makes the loser move on to the next row or come back empty.
func (s *WikiStore) ClaimNextRun(ctx context.Context, workerID string) (*QueuedRun, error) {
	var (
		run       QueuedRun
		connector *string
	)
	err := s.pool.QueryRow(ctx, `
		UPDATE runs r
		SET status = 'running', claimed_by = $1, claimed_at = now(), started_at = now()
		FROM workspaces w JOIN orgs o ON o.id = w.org_id
		WHERE r.id = (
		    SELECT id FROM runs
		    WHERE status = 'queued'
		    ORDER BY created_at
		    FOR UPDATE SKIP LOCKED
		    LIMIT 1)
		  AND w.id = r.workspace_id
		RETURNING r.id, r.workspace_id, r.connector_id, r.trigger, w.slug, o.slug`,
		workerID).Scan(&run.ID, &run.WorkspaceID, &connector, &run.Trigger,
		&run.WorkspaceSlug, &run.OrgSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: claim run: %w", err)
	}
	if connector != nil {
		run.ConnectorID = *connector
	}
	return &run, nil
}

// FailRun finishes a claimed run that never reached the pipeline -- a sync or
// configuration error. Errors inside the pipeline are recorded by RecordRun
// with per-item detail instead.
func (s *WikiStore) FailRun(ctx context.Context, runID, message string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE runs SET status = 'failed', error = $2, finished_at = now()
		WHERE id = $1`, runID, message); err != nil {
		return fmt.Errorf("store: fail run: %w", err)
	}
	return nil
}

// RequeueStaleRuns returns runs claimed longer ago than the deadline to the
// queue. A worker that died mid-build leaves a 'running' row that would
// otherwise block its workspace forever via the active-run index; the pipeline
// is idempotent from the start, so re-running a half-finished build is safe.
func (s *WikiStore) RequeueStaleRuns(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs
		SET status = 'queued', claimed_by = NULL, claimed_at = NULL, started_at = NULL
		WHERE status = 'running' AND claimed_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: requeue stale runs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
