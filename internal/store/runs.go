package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// RunRow is one run as the dashboard consumes it.
type RunRow struct {
	ID           string
	Trigger      string
	Ref          string
	Status       string
	CostUSD      float64
	PagesCreated int
	PagesUpdated int
	PagesDeleted int
	Error        string
	ClaimedBy    string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	// NotBefore is the debounce deadline a queued run waits behind (webhook
	// cooldowns, upload batching); nil when the run is claimable immediately.
	NotBefore *time.Time
}

// ListRuns returns a workspace's runs, newest first.
func (s *WikiStore) ListRuns(ctx context.Context, workspaceID string, limit, offset int) ([]RunRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, trigger, coalesce(ref, ''), status, cost_usd,
		       pages_created, pages_updated, pages_deleted,
		       coalesce(error, ''), coalesce(claimed_by, ''),
		       created_at, started_at, finished_at, not_before
		FROM runs
		WHERE workspace_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	out := []RunRow{}
	for rows.Next() {
		var r RunRow
		if err := rows.Scan(&r.ID, &r.Trigger, &r.Ref, &r.Status, &r.CostUSD,
			&r.PagesCreated, &r.PagesUpdated, &r.PagesDeleted,
			&r.Error, &r.ClaimedBy, &r.CreatedAt, &r.StartedAt, &r.FinishedAt,
			&r.NotBefore); err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SpendInWindow sums a workspace's ledgered spend over the trailing window.
// The ledger exists precisely so this is one indexed aggregate rather than a
// scan of run history.
func (s *WikiStore) SpendInWindow(ctx context.Context, workspaceID string, window time.Duration) (float64, error) {
	var spent float64
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(sum(amount_usd), 0)
		FROM spend_ledger
		WHERE workspace_id = $1 AND occurred_at > now() - make_interval(secs => $2)`,
		workspaceID, window.Seconds()).Scan(&spent)
	if err != nil {
		return 0, fmt.Errorf("store: spend in window: %w", err)
	}
	return spent, nil
}

// WorkspaceBudgetUSD returns the workspace's rolling-window budget cap, or nil
// when none is set (unlimited).
func (s *WikiStore) WorkspaceBudgetUSD(ctx context.Context, workspaceID string) (*float64, error) {
	var budget *float64
	err := s.pool.QueryRow(ctx,
		`SELECT budget_usd FROM workspaces WHERE id = $1`, workspaceID).Scan(&budget)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: workspace budget: %w", err)
	}
	return budget, nil
}

// TrailingUnitCost returns the average actual cost of this workspace's
// recently succeeded units, or 0 when there is no history yet. This is what
// replaces the hardcoded per-unit estimate: a workspace whose pages cost
// $0.40 to write should see previews that say so, not a global constant.
func (s *WikiStore) TrailingUnitCost(ctx context.Context, workspaceID string) (float64, error) {
	var avg float64
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(avg(cost_usd), 0) FROM (
		    SELECT ri.cost_usd
		    FROM run_items ri
		    JOIN runs r ON r.id = ri.run_id
		    WHERE r.workspace_id = $1
		      AND ri.status = 'succeeded'
		      AND ri.cost_usd > 0
		    ORDER BY ri.created_at DESC
		    LIMIT 50
		) recent`, workspaceID).Scan(&avg)
	if err != nil {
		return 0, fmt.Errorf("store: trailing unit cost: %w", err)
	}
	return avg, nil
}

// RunItemRow is one unit's outcome within a run, for cost attribution.
type RunItemRow struct {
	Key        string
	Kind       string
	Status     string
	CostUSD    float64
	EstCostUSD *float64
	Turns      int
	Error      string
}

// ListRunItems returns a run's per-unit state, scoped to the workspace so a
// run id from another tenant reads as absent.
//
// Ordered to answer whichever question the run's state makes relevant: while it
// is live, what is happening now and what is still queued; once it is over,
// what it cost. Unfinished units sort first, and within the finished ones the
// costliest leads -- which is exactly the old ordering on a completed run, so
// the cost-attribution view is unchanged.
func (s *WikiStore) ListRunItems(ctx context.Context, workspaceID, runID string) ([]RunItemRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ri.cache_key, ri.kind, ri.status, ri.cost_usd, ri.est_cost_usd,
		       ri.turns, coalesce(ri.error, '')
		FROM run_items ri
		JOIN runs r ON r.id = ri.run_id
		WHERE r.id = $1 AND r.workspace_id = $2
		ORDER BY CASE ri.status
		             WHEN 'running' THEN 0
		             WHEN 'pending' THEN 1
		             ELSE 2
		         END,
		         ri.cost_usd DESC, ri.cache_key`, runID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: list run items: %w", err)
	}
	defer rows.Close()

	out := []RunItemRow{}
	for rows.Next() {
		var it RunItemRow
		if err := rows.Scan(&it.Key, &it.Kind, &it.Status, &it.CostUSD,
			&it.EstCostUSD, &it.Turns, &it.Error); err != nil {
			return nil, fmt.Errorf("store: scan run item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SeedRunItems records a run's plan as pending items before any unit runs.
//
// This is what makes a build watchable rather than merely reported on: the
// items table answers "what is still coming" from the moment the plan exists,
// instead of materializing all at once when the run is already over.
//
// A no-op for CLI builds. Their run row is not created until the run finishes
// -- there is no id to attach to yet, and nothing polling for it either.
func (s *WikiStore) SeedRunItems(ctx context.Context, runID string, keys []diff.Key, estCostUSD float64) error {
	if !isUUID(runID) || len(keys) == 0 {
		return nil
	}

	kinds := make([]string, len(keys))
	cacheKeys := make([]string, len(keys))
	for i, k := range keys {
		kinds[i] = k.Prefix()
		cacheKeys[i] = string(k)
	}

	// One statement rather than a loop: the plan can be dozens of units, and
	// this runs before the first model call, where latency is pure overhead.
	//
	// DO NOTHING rather than an update: a retried seed must not reset an item
	// that has already started, which is what a resumed or debounced run does.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO run_items (run_id, kind, cache_key, status, est_cost_usd)
		SELECT $1, k.kind, k.cache_key, 'pending', $4
		FROM unnest($2::text[], $3::text[]) AS k(kind, cache_key)
		ON CONFLICT (run_id, cache_key) DO NOTHING`,
		runID, kinds, cacheKeys, estCostUSD); err != nil {
		return fmt.Errorf("store: seed run items: %w", err)
	}
	return nil
}

// MarkRunItem settles one planned item as the run reaches it.
//
// Upserts rather than updates so it is correct even when the seed did not run
// or the plan grew after it: the item is the record of the unit either way.
// finished_at stays null while the unit is still in flight, which is what lets
// a reader tell "started three minutes ago" from "took three minutes".
func (s *WikiStore) MarkRunItem(ctx context.Context, runID string, item jobs.ItemSummary) error {
	if !isUUID(runID) {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO run_items (run_id, kind, cache_key, status, cost_usd, turns, error, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7,
		        CASE WHEN $4 IN ('pending', 'running') THEN NULL ELSE now() END)
		ON CONFLICT (run_id, cache_key) DO UPDATE SET
		    status      = EXCLUDED.status,
		    cost_usd    = EXCLUDED.cost_usd,
		    turns       = EXCLUDED.turns,
		    error       = EXCLUDED.error,
		    finished_at = EXCLUDED.finished_at`,
		runID, item.Key.Prefix(), string(item.Key), item.Status,
		item.CostUSD, item.Turns, nullable(item.Err)); err != nil {
		return fmt.Errorf("store: mark run item %s: %w", item.Key, err)
	}
	return nil
}

// RunProgress counts a run's items by disposition, for the dashboard's
// progress line. Aggregated in the database rather than by fetching every item
// per run: the runs list renders ten of them at once.
type RunProgress struct {
	Total   int
	Done    int
	Running int
	Pending int
}

// RunProgressFor returns per-run item counts for the given runs, keyed by run
// id. Runs with no items are absent rather than zero, so a caller can tell
// "nothing planned yet" from "a plan of zero units".
func (s *WikiStore) RunProgressFor(ctx context.Context, workspaceID string, runIDs []string) (map[string]RunProgress, error) {
	out := map[string]RunProgress{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ri.run_id,
		       count(*),
		       count(*) FILTER (WHERE ri.status NOT IN ('pending', 'running')),
		       count(*) FILTER (WHERE ri.status = 'running'),
		       count(*) FILTER (WHERE ri.status = 'pending')
		FROM run_items ri
		JOIN runs r ON r.id = ri.run_id
		WHERE r.workspace_id = $1 AND ri.run_id = ANY($2::uuid[])
		GROUP BY ri.run_id`, workspaceID, runIDs)
	if err != nil {
		return nil, fmt.Errorf("store: run progress: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var p RunProgress
		if err := rows.Scan(&id, &p.Total, &p.Done, &p.Running, &p.Pending); err != nil {
			return nil, fmt.Errorf("store: scan run progress: %w", err)
		}
		out[id] = p
	}
	return out, rows.Err()
}

// FileReview inserts an open review item, deduplicated against open items with
// the same kind and title, mirroring how agent-raised flags are recorded: a
// condition that persists across attempts asks its question once.
func (s *WikiStore) FileReview(ctx context.Context, workspaceID, kind, title, detail string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO review_items (workspace_id, kind, title, detail)
		SELECT $1, $2, $3, $4
		WHERE NOT EXISTS (
		    SELECT 1 FROM review_items
		    WHERE workspace_id = $1 AND kind = $2 AND title = $3 AND status = 'open')`,
		workspaceID, kind, title, detail); err != nil {
		return fmt.Errorf("store: file review: %w", err)
	}
	return nil
}
