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
	ID          string
	WorkspaceID string
	ConnectorID string // empty when the run is not pinned to one connector
	Trigger     string
	// RefTo is the pushed head this run should build toward, when the
	// trigger knew one. The base of the range is not stored anywhere: the
	// worker derives it from the last successful build.
	RefTo         string
	WorkspaceSlug string
	OrgSlug       string
}

// EnqueueOptions carries the optional incremental-build fields.
type EnqueueOptions struct {
	// RefTo is the head the triggering event pushed, empty when unknown.
	RefTo string
	// NotBefore delays claiming, which is how the webhook completion
	// cooldown is expressed without dropping events.
	NotBefore time.Time
}

// QueueDepth counts runs that have not reached a terminal state, grouped by
// status. It is the number workers should be scaled on: CPU is a poor proxy
// for a queue of LLM-bound builds, because a worker waiting on a model call is
// idle by every resource measure and busy by the only one that matters.
//
// Deliberately instance-wide rather than per workspace: this feeds a
// deployment-level gauge, and a label per tenant would grow without bound.
func (s *WikiStore) QueueDepth(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT status, count(*)
		FROM runs
		WHERE status IN ('queued', 'running')
		GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("store: queue depth: %w", err)
	}
	defer rows.Close()

	// Both statuses are always reported, so a drained queue publishes zeros
	// rather than leaving the last non-zero sample looking current forever.
	out := map[string]int{"queued": 0, "running": 0}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: scan queue depth: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}

// EnqueueRun files a build request for a workspace. When the workspace already
// has a queued or running run, that run is returned instead and created is
// false: the partial unique index makes double-submission and webhook storms
// collapse into the one run already waiting.
func (s *WikiStore) EnqueueRun(ctx context.Context, workspaceID, trigger, connectorID string) (runID string, created bool, err error) {
	return s.EnqueueRunOpts(ctx, workspaceID, trigger, connectorID, EnqueueOptions{})
}

// EnqueueRunOpts is EnqueueRun with the incremental fields. A debounced
// enqueue advances the waiting run's ref_to to the newest head, so a push
// storm's final state is what gets built; a run already claimed cannot be
// updated, and loses nothing — the range base always derives from the last
// successful build, so the next enqueue covers the gap.
func (s *WikiStore) EnqueueRunOpts(ctx context.Context, workspaceID, trigger, connectorID string, opts EnqueueOptions) (runID string, created bool, err error) {
	var notBefore any
	if !opts.NotBefore.IsZero() {
		notBefore = opts.NotBefore
	}
	// Bounded: each pass can lose one insert/finish race (the active run
	// finishes between our conflicting insert and the read-back), and losing
	// it repeatedly means something is wrong enough to surface.
	for attempt := 0; attempt < 3; attempt++ {
		err = s.pool.QueryRow(ctx, `
			INSERT INTO runs (workspace_id, connector_id, trigger, status, ref_to, not_before)
			VALUES ($1, $2, $3, 'queued', $4, $5)
			ON CONFLICT (workspace_id) WHERE status IN ('queued','running') DO NOTHING
			RETURNING id`,
			workspaceID, nullable(connectorID), trigger, nullable(opts.RefTo), notBefore).Scan(&runID)
		if err == nil {
			return runID, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", false, fmt.Errorf("store: enqueue run: %w", err)
		}

		// Conflict path: advance the waiting run's target head, then surface
		// the active run so the caller can report it. A running (claimed)
		// run is left alone.
		if opts.RefTo != "" {
			if _, err := s.pool.Exec(ctx, `
				UPDATE runs SET ref_to = $2
				WHERE workspace_id = $1 AND status = 'queued'`,
				workspaceID, opts.RefTo); err != nil {
				return "", false, fmt.Errorf("store: advance ref_to: %w", err)
			}
		}
		err = s.pool.QueryRow(ctx, `
			SELECT id FROM runs
			WHERE workspace_id = $1 AND status IN ('queued','running')
			ORDER BY created_at DESC LIMIT 1`, workspaceID).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // the active run finished in between; try the insert again
		}
		if err != nil {
			return "", false, fmt.Errorf("store: find active run: %w", err)
		}
		return runID, false, nil
	}
	return "", false, fmt.Errorf("store: enqueue run: lost the insert race repeatedly for workspace %s", workspaceID)
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
	var refTo *string
	err := s.pool.QueryRow(ctx, `
		UPDATE runs r
		SET status = 'running', claimed_by = $1, claimed_at = now(), started_at = now()
		FROM workspaces w JOIN orgs o ON o.id = w.org_id
		WHERE r.id = (
		    SELECT id FROM runs
		    WHERE status = 'queued'
		      AND (not_before IS NULL OR not_before <= now())
		    ORDER BY created_at
		    FOR UPDATE SKIP LOCKED
		    LIMIT 1)
		  AND w.id = r.workspace_id
		RETURNING r.id, r.workspace_id, r.connector_id, r.trigger, r.ref_to, w.slug, o.slug`,
		workerID).Scan(&run.ID, &run.WorkspaceID, &connector, &run.Trigger,
		&refTo, &run.WorkspaceSlug, &run.OrgSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: claim run: %w", err)
	}
	if connector != nil {
		run.ConnectorID = *connector
	}
	if refTo != nil {
		run.RefTo = *refTo
	}
	return &run, nil
}

// LastSuccessfulRef returns the ref the workspace's newest completed build
// recorded, or "" when none has. This is the base of every incremental
// range: deriving it here rather than trusting the webhook's "before" means
// debounced or dropped pushes can never leave a hole in the diff.
func (s *WikiStore) LastSuccessfulRef(ctx context.Context, workspaceID string) (string, error) {
	var ref *string
	err := s.pool.QueryRow(ctx, `
		SELECT ref FROM runs
		WHERE workspace_id = $1
		  AND status IN ('succeeded', 'partial')
		  AND ref IS NOT NULL
		ORDER BY finished_at DESC NULLS LAST
		LIMIT 1`, workspaceID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ref == nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: last successful ref: %w", err)
	}
	return *ref, nil
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

// RequeueRun returns one claimed run to the queue, for a worker draining at
// shutdown: interrupted is not failed, and the pipeline is idempotent from
// the start.
func (s *WikiStore) RequeueRun(ctx context.Context, runID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE runs
		SET status = 'queued', claimed_by = NULL, claimed_at = NULL, started_at = NULL, error = NULL
		WHERE id = $1 AND status = 'running'`, runID); err != nil {
		return fmt.Errorf("store: requeue run: %w", err)
	}
	return nil
}

// Sweep enforces retention: soft-deleted pages past their window, expired
// sessions, dead tokens, and finished runs past theirs. The spend ledger is
// deliberately untouched — budget windows read it — and run deletion nulls
// its run_id references rather than cascading into it. Zero durations
// disable the corresponding part.
func (s *WikiStore) Sweep(ctx context.Context, softDeleteRetention, runRetention time.Duration) (int64, error) {
	var total int64

	if softDeleteRetention > 0 {
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM pages
			WHERE deleted_at IS NOT NULL
			  AND deleted_at < now() - make_interval(secs => $1)`,
			softDeleteRetention.Seconds())
		if err != nil {
			return total, fmt.Errorf("store: sweep pages: %w", err)
		}
		total += tag.RowsAffected()
	}

	if runRetention > 0 {
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM runs
			WHERE finished_at IS NOT NULL
			  AND finished_at < now() - make_interval(secs => $1)
			  AND status NOT IN ('queued', 'running')`,
			runRetention.Seconds())
		if err != nil {
			return total, fmt.Errorf("store: sweep runs: %w", err)
		}
		total += tag.RowsAffected()
	}

	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	if err != nil {
		return total, fmt.Errorf("store: sweep sessions: %w", err)
	}
	total += tag.RowsAffected()

	// Tokens linger a week past death so an operator can still see what a
	// failing client was presenting.
	tag, err = s.pool.Exec(ctx, `
		DELETE FROM tokens
		WHERE (revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days')
		   OR (expires_at IS NOT NULL AND expires_at < now() - interval '7 days')`)
	if err != nil {
		return total, fmt.Errorf("store: sweep tokens: %w", err)
	}
	total += tag.RowsAffected()

	return total, nil
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
