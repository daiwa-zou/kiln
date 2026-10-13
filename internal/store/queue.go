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

// EnqueueOutcome says what became of a build request.
type EnqueueOutcome string

const (
	// EnqueueCreated: the request became a new queued run.
	EnqueueCreated EnqueueOutcome = "created"
	// EnqueueMerged: the request joined a build already waiting in the queue,
	// which will do the work it asked for.
	EnqueueMerged EnqueueOutcome = "merged"
	// EnqueueDeferred: the workspace's active run cannot do the work -- a research
	// run, or a build already underway -- so the request was recorded and becomes
	// a queued build when that run finishes. It is not lost, and not yet queued.
	EnqueueDeferred EnqueueOutcome = "deferred"
)

// EnqueueResult is the full answer to a build request.
type EnqueueResult struct {
	// RunID is the run the caller should watch: the new run, the waiting build
	// that absorbed the request, or the active run the request is waiting behind.
	RunID   string
	Outcome EnqueueOutcome
	// ActiveTrigger is the trigger of the run the request merged into or is
	// waiting behind. Empty when a new run was created.
	ActiveTrigger string
}

// Created reports whether the request became a new run of its own.
func (r EnqueueResult) Created() bool { return r.Outcome == EnqueueCreated }

// EnqueueRun files a build request for a workspace. When a build is already
// waiting, the request merges into it and created is false; when the workspace
// is busy with something that cannot honor it, the request is recorded to run
// afterwards and created is likewise false. See EnqueueBuild.
func (s *WikiStore) EnqueueRun(ctx context.Context, workspaceID, trigger, connectorID string) (runID string, created bool, err error) {
	return s.EnqueueRunOpts(ctx, workspaceID, trigger, connectorID, EnqueueOptions{})
}

// EnqueueRunOpts is EnqueueBuild reduced to the run id and whether a new run was
// created, for callers that do not report what happened to the request.
func (s *WikiStore) EnqueueRunOpts(ctx context.Context, workspaceID, trigger, connectorID string, opts EnqueueOptions) (runID string, created bool, err error) {
	res, err := s.EnqueueBuild(ctx, workspaceID, trigger, connectorID, opts)
	if err != nil {
		return "", false, err
	}
	return res.RunID, res.Created(), nil
}

// EnqueueBuild files a request to build a workspace. The partial unique index
// allows one queued-or-running run per workspace, which is what makes a push
// storm collapse into one build -- but only a queued build can honor a build
// request, so the request goes one of three ways:
//
//   - No active run: a new queued run is created.
//   - A queued build is waiting: the request merges into it. It advances the
//     build's ref_to to the newest head, so a push storm's final state is what
//     gets built, widens a build pinned to one connector to every connector
//     when the request names a different one (or none), so no connector's change
//     is left out of the build that was supposed to include it, and brings the
//     build's start time forward to the request's (a request with none makes
//     it claimable at once).
//   - Anything else (a queued research run, or a build already running): the
//     request is recorded in build_requests and promoted to a queued build when
//     the workspace is next free. Merging into research would drop the request
//     on the floor, because the worker reads sources to answer one question and
//     builds nothing. Merging into a running build would drop it too: that build
//     planned its work from what it read at the start.
//
// Nothing is lost either way, because the range base of an incremental build
// always derives from the last successful build rather than from the request.
func (s *WikiStore) EnqueueBuild(ctx context.Context, workspaceID, trigger, connectorID string, opts EnqueueOptions) (EnqueueResult, error) {
	var notBefore any
	if !opts.NotBefore.IsZero() {
		notBefore = opts.NotBefore
	}
	// Bounded: each pass can lose one insert/finish race (the active run
	// finishes between our conflicting insert and the read-back), and losing
	// it repeatedly means something is wrong enough to surface.
	for attempt := 0; attempt < 3; attempt++ {
		var runID string
		err := s.pool.QueryRow(ctx, `
			INSERT INTO runs (workspace_id, connector_id, trigger, status, ref_to, not_before)
			VALUES ($1, $2, $3, 'queued', $4, $5)
			ON CONFLICT (workspace_id) WHERE status IN ('queued','running') DO NOTHING
			RETURNING id`,
			workspaceID, nullable(connectorID), trigger, nullable(opts.RefTo), notBefore).Scan(&runID)
		if err == nil {
			return EnqueueResult{RunID: runID, Outcome: EnqueueCreated}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return EnqueueResult{}, fmt.Errorf("store: enqueue run: %w", err)
		}

		// Conflict path. First try to merge into a waiting build. The status
		// guard is re-evaluated under the row lock, so a worker that claims the
		// build between the insert above and this update is not merged into.
		// The start time merges as recordBuildRequest's does: the earliest
		// wins, and a request with none clears it, so a person pressing Ingest
		// now is not made to wait out a held continuation's delay.
		// ('research' below is jobs.TriggerResearch, kept as a SQL literal.)
		var mergedTrigger string
		err = s.pool.QueryRow(ctx, `
			UPDATE runs SET
			    ref_to = coalesce($2, ref_to),
			    connector_id = CASE WHEN connector_id IS NOT DISTINCT FROM $3::uuid
			                        THEN connector_id ELSE NULL END,
			    not_before = CASE WHEN not_before IS NULL OR $4::timestamptz IS NULL
			                      THEN NULL
			                      ELSE least(not_before, $4::timestamptz) END
			WHERE workspace_id = $1 AND status = 'queued' AND trigger <> 'research'
			RETURNING id, trigger`,
			workspaceID, nullable(opts.RefTo), nullable(connectorID), notBefore).Scan(&runID, &mergedTrigger)
		if err == nil {
			return EnqueueResult{RunID: runID, Outcome: EnqueueMerged, ActiveTrigger: mergedTrigger}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return EnqueueResult{}, fmt.Errorf("store: merge into waiting build: %w", err)
		}

		// No waiting build. Whatever is active cannot honor the request.
		var activeTrigger string
		err = s.pool.QueryRow(ctx, `
			SELECT id, trigger FROM runs
			WHERE workspace_id = $1 AND status IN ('queued','running')
			ORDER BY created_at DESC LIMIT 1`, workspaceID).Scan(&runID, &activeTrigger)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // the active run finished in between; try the insert again
		}
		if err != nil {
			return EnqueueResult{}, fmt.Errorf("store: find active run: %w", err)
		}

		// Recorded even if the active run finishes before this lands: the
		// promotion at the next claim queues it when no run is active, so there
		// is no window in which the request can be missed.
		if err := s.recordBuildRequest(ctx, workspaceID, trigger, connectorID, opts.RefTo, notBefore); err != nil {
			return EnqueueResult{}, err
		}
		return EnqueueResult{RunID: runID, Outcome: EnqueueDeferred, ActiveTrigger: activeTrigger}, nil
	}
	return EnqueueResult{}, fmt.Errorf("store: enqueue run: lost the insert race repeatedly for workspace %s", workspaceID)
}

// recordBuildRequest upserts the workspace's one pending follow-up build. A
// second request merges into it exactly as it would into a queued build: the
// newest head wins, a differing connector widens it to all of them, and the
// earliest start time wins so a debounce on one request never delays another.
func (s *WikiStore) recordBuildRequest(ctx context.Context, workspaceID, trigger, connectorID, refTo string, notBefore any) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO build_requests (workspace_id, trigger, connector_id, ref_to, not_before)
		VALUES ($1, $2, $3::uuid, $4, $5::timestamptz)
		ON CONFLICT (workspace_id) DO UPDATE SET
		    connector_id = CASE WHEN build_requests.connector_id IS NOT DISTINCT FROM EXCLUDED.connector_id
		                        THEN build_requests.connector_id ELSE NULL END,
		    ref_to = coalesce(EXCLUDED.ref_to, build_requests.ref_to),
		    not_before = CASE WHEN build_requests.not_before IS NULL OR EXCLUDED.not_before IS NULL
		                      THEN NULL
		                      ELSE least(build_requests.not_before, EXCLUDED.not_before) END,
		    updated_at = now()`,
		workspaceID, trigger, nullable(connectorID), nullable(refTo), notBefore); err != nil {
		return fmt.Errorf("store: record build request: %w", err)
	}
	return nil
}

// promoteBuildRequests turns every pending follow-up whose workspace has no
// active run into a queued build, and drops the request. One statement, so a
// request is either still pending or already a run, never both and never
// neither. ON CONFLICT covers a run enqueued between the check and the insert:
// the request then stays pending and is promoted after that run finishes.
func (s *WikiStore) promoteBuildRequests(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `
		WITH due AS (
		    SELECT b.workspace_id, b.trigger, b.connector_id, b.ref_to, b.not_before
		    FROM build_requests b
		    WHERE NOT EXISTS (
		        SELECT 1 FROM runs r
		        WHERE r.workspace_id = b.workspace_id AND r.status IN ('queued','running'))
		    FOR UPDATE OF b SKIP LOCKED),
		promoted AS (
		    INSERT INTO runs (workspace_id, connector_id, trigger, status, ref_to, not_before)
		    SELECT workspace_id, connector_id, trigger, 'queued', ref_to, not_before FROM due
		    ON CONFLICT (workspace_id) WHERE status IN ('queued','running') DO NOTHING
		    RETURNING workspace_id)
		DELETE FROM build_requests
		WHERE workspace_id IN (SELECT workspace_id FROM promoted)`); err != nil {
		return fmt.Errorf("store: promote build requests: %w", err)
	}
	return nil
}

// ClaimNextRun atomically claims the oldest queued run for this worker.
// Returns nil when the queue is empty. Claim and state change are one
// statement, so two workers racing on the same row is impossible: SKIP LOCKED
// makes the loser move on to the next row or come back empty.
//
// Pending follow-up builds are promoted to queued runs first, so a request that
// arrived while the workspace was busy is claimable the moment it is free, by
// whichever worker looks next -- including after the worker that would have
// promoted it died.
func (s *WikiStore) ClaimNextRun(ctx context.Context, workerID string) (*QueuedRun, error) {
	if err := s.promoteBuildRequests(ctx); err != nil {
		return nil, err
	}
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
//
// Fenced to the claiming worker: without that, a worker whose claim went stale
// would fail a run that another worker is, at that moment, building. The fence
// is claimed_by alone, not status, because the pipeline may already have
// finished the row as failed with item detail, and restating the cause on top
// of it is not a lost claim. ErrClaimLost means no row matched.
func (s *WikiStore) FailRun(ctx context.Context, runID, workerID, message string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET status = 'failed', error = $3, finished_at = now()
		WHERE id = $1 AND claimed_by = $2`, runID, workerID, message)
	if err != nil {
		return fmt.Errorf("store: fail run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

// RefuseRunOverBudget finishes a claimed run as over_budget without it having
// built anything: the workspace's rolling budget was already spent when the
// worker picked the run up.
//
// Fenced like FailRun, and additionally to status 'running': this is a decision
// taken before the pipeline starts, so a row that is already finished is not
// this worker's to restate. ErrClaimLost means no row matched.
func (s *WikiStore) RefuseRunOverBudget(ctx context.Context, runID, workerID, message string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET status = 'over_budget', error = $3, finished_at = now()
		WHERE id = $1 AND claimed_by = $2 AND status = 'running'`, runID, workerID, message)
	if err != nil {
		return fmt.Errorf("store: refuse run over budget: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

// RequeueRun returns one claimed run to the queue, for a worker draining at
// shutdown: interrupted is not failed, and the pipeline is idempotent from
// the start.
//
// Fenced to the claiming worker and to status 'running'. A draining worker
// whose run was meanwhile requeued and claimed elsewhere must not drag that
// other worker's live claim back to the queue, and a run that already finished
// stays finished. ErrClaimLost means no row matched.
func (s *WikiStore) RequeueRun(ctx context.Context, runID, workerID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs
		SET status = 'queued', claimed_by = NULL, claimed_at = NULL, started_at = NULL, error = NULL
		WHERE id = $1 AND claimed_by = $2 AND status = 'running'`, runID, workerID)
	if err != nil {
		return fmt.Errorf("store: requeue run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

// HeartbeatRun refreshes the liveness of a run this worker has claimed.
//
// claimed_at doubles as the heartbeat rather than a new column. The stale sweep
// already reads it, so every worker in a mixed-version rolling deploy agrees on
// what "alive" means; a separate heartbeat_at would leave an old worker's sweep
// requeueing a new worker's healthy run for the length of the rollout. The
// claim's start is still recoverable from started_at.
//
// ErrClaimLost means the run is no longer this worker's, which is the signal to
// stop spending on it.
func (s *WikiStore) HeartbeatRun(ctx context.Context, runID, workerID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET claimed_at = now()
		WHERE id = $1 AND claimed_by = $2 AND status = 'running'`, runID, workerID)
	if err != nil {
		return fmt.Errorf("store: heartbeat run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
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

// StaleRun is a run the stale sweep returned to the queue.
type StaleRun struct {
	ID          string
	WorkspaceID string
	// PreviousClaimedBy is the worker whose claim lapsed, so an operator can
	// tell a dead pod from a live one that stopped heartbeating.
	PreviousClaimedBy string
}

// RequeueStaleRuns returns runs whose claim has not been refreshed within the
// deadline to the queue, and says which. A worker that died mid-build leaves a
// 'running' row that would otherwise block its workspace forever via the
// active-run index; the pipeline is idempotent from the start, so re-running a
// half-finished build is safe.
//
// A live worker keeps claimed_at fresh with HeartbeatRun, so only a run whose
// worker has stopped is swept. Rows are locked with SKIP LOCKED: one being
// heartbeat at this instant is alive by definition and is left alone rather
// than waited on.
func (s *WikiStore) RequeueStaleRuns(ctx context.Context, olderThan time.Duration) ([]StaleRun, error) {
	rows, err := s.pool.Query(ctx, `
		WITH stale AS (
		    SELECT id, claimed_by FROM runs
		    WHERE status = 'running' AND claimed_at < now() - make_interval(secs => $1)
		    FOR UPDATE SKIP LOCKED)
		UPDATE runs r
		SET status = 'queued', claimed_by = NULL, claimed_at = NULL, started_at = NULL
		FROM stale
		WHERE r.id = stale.id
		RETURNING r.id, r.workspace_id, coalesce(stale.claimed_by, '')`,
		olderThan.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: requeue stale runs: %w", err)
	}
	defer rows.Close()

	var out []StaleRun
	for rows.Next() {
		var r StaleRun
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.PreviousClaimedBy); err != nil {
			return nil, fmt.Errorf("store: scan stale run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
