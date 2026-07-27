package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
}

// ListRuns returns a workspace's runs, newest first.
func (s *WikiStore) ListRuns(ctx context.Context, workspaceID string, limit, offset int) ([]RunRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, trigger, coalesce(ref, ''), status, cost_usd,
		       pages_created, pages_updated, pages_deleted,
		       coalesce(error, ''), coalesce(claimed_by, ''),
		       created_at, started_at, finished_at
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
			&r.Error, &r.ClaimedBy, &r.CreatedAt, &r.StartedAt, &r.FinishedAt); err != nil {
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

// ListRunItems returns a run's per-unit outcomes, costliest first, scoped to
// the workspace so a run id from another tenant reads as absent.
func (s *WikiStore) ListRunItems(ctx context.Context, workspaceID, runID string) ([]RunItemRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ri.cache_key, ri.kind, ri.status, ri.cost_usd, ri.est_cost_usd,
		       ri.turns, coalesce(ri.error, '')
		FROM run_items ri
		JOIN runs r ON r.id = ri.run_id
		WHERE r.id = $1 AND r.workspace_id = $2
		ORDER BY ri.cost_usd DESC, ri.cache_key`, runID, workspaceID)
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
