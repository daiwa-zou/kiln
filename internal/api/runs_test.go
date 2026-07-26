package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

func TestRunsCreateListAndDebounce(t *testing.T) {
	srv, _, _ := testServer(t)

	var created struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}
	code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/runs", "",
		map[string]any{}, &created)
	if code != http.StatusAccepted {
		t.Fatalf("POST runs = %d, want 202", code)
	}
	if !created.Created || created.ID == "" {
		t.Errorf("first enqueue: %+v", created)
	}

	// A second submission joins the waiting run rather than duplicating it.
	var again struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}
	code = send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/runs", "",
		map[string]any{}, &again)
	if code != http.StatusAccepted {
		t.Fatalf("second POST runs = %d, want 202", code)
	}
	if again.Created || again.ID != created.ID {
		t.Errorf("debounce: %+v, want the run %s", again, created.ID)
	}

	var runs []RunSummaryJSON
	if code := get(t, srv, "/api/v1/workspaces/demo/runs", &runs); code != http.StatusOK {
		t.Fatalf("GET runs = %d", code)
	}
	if len(runs) != 1 || runs[0].Status != "queued" || runs[0].Trigger != "manual" {
		t.Errorf("runs list: %+v", runs)
	}
}

func TestRunsCreateRefusedOverBudget(t *testing.T) {
	srv, js, wsID := testServer(t)
	ctx := context.Background()

	// A $1 budget with $2 already ledgered inside the window.
	if _, err := js.Pool().Exec(ctx,
		`UPDATE workspaces SET budget_usd = 1.00 WHERE id = $1`, wsID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Pool().Exec(ctx,
		`INSERT INTO spend_ledger (workspace_id, amount_usd) VALUES ($1, 2.00)`, wsID); err != nil {
		t.Fatal(err)
	}

	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/runs", "",
		map[string]any{}, nil); code != http.StatusTooManyRequests {
		t.Fatalf("POST runs over budget = %d, want 429", code)
	}

	// The refusal files its question in the review queue -- once, however many
	// times the button is mashed.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/runs", "",
		map[string]any{}, nil); code != http.StatusTooManyRequests {
		t.Fatalf("repeat POST runs over budget = %d, want 429", code)
	}
	var n int
	if err := js.Pool().QueryRow(ctx,
		`SELECT count(*) FROM review_items WHERE workspace_id = $1 AND kind = 'budget' AND status = 'open'`,
		wsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("budget review items = %d, want exactly 1", n)
	}

	// Nothing was queued.
	var runs []RunSummaryJSON
	get(t, srv, "/api/v1/workspaces/demo/runs", &runs)
	if len(runs) != 0 {
		t.Errorf("runs queued despite refusal: %+v", runs)
	}
}

func TestSpendLedgerAndTrailingCostFeedEstimates(t *testing.T) {
	_, js, wsID := testServer(t)
	ctx := context.Background()

	// Record a run whose items carry actual costs; the trailing average is
	// what future previews are built from.
	if err := js.RecordRun(ctx, runWithItems(wsID, 0.30, 0.10)); err != nil {
		t.Fatal(err)
	}

	avg, err := js.TrailingUnitCost(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	if avg < 0.19 || avg > 0.21 {
		t.Errorf("trailing unit cost = %v, want ~0.20", avg)
	}

	spent, err := js.SpendInWindow(ctx, wsID, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if spent < 0.39 || spent > 0.41 {
		t.Errorf("spend in window = %v, want the run's 0.40", spent)
	}
}

// runWithItems builds a succeeded run summary whose two items cost the given
// amounts, with estimates recorded alongside the actuals.
func runWithItems(wsID string, costs ...float64) jobs.RunSummary {
	run := jobs.RunSummary{
		WorkspaceID: wsID, Trigger: "manual", Status: jobs.StatusSucceeded,
	}
	for i, c := range costs {
		run.CostUSD += c
		run.Items = append(run.Items, jobs.ItemSummary{
			Key:    diff.ModuleKey(string(rune('a' + i))),
			Status: jobs.StatusSucceeded, CostUSD: c, EstCostUSD: 0.12,
		})
	}
	return run
}
