package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/daiwa-zou/kiln/internal/store"
)

// RunStore is the run-queue surface: listing for the dashboard, enqueueing
// for the rebuild button and (in M3) webhooks, and the budget-window inputs
// that gate enqueueing. Separate from WriteStore so read-only deployments and
// tests can leave it nil, which unmounts the routes.
type RunStore interface {
	ListRuns(ctx context.Context, workspaceID string, limit, offset int) ([]store.RunRow, error)
	EnqueueRun(ctx context.Context, workspaceID, trigger, connectorID string) (runID string, created bool, err error)
	SpendInWindow(ctx context.Context, workspaceID string, window time.Duration) (float64, error)
	WorkspaceBudgetUSD(ctx context.Context, workspaceID string) (*float64, error)
	FileReview(ctx context.Context, workspaceID, kind, title, detail string) error
}

const defaultRunLimit = 50

// RunSummaryJSON is one run as the dashboard consumes it.
type RunSummaryJSON struct {
	ID           string  `json:"id"`
	Trigger      string  `json:"trigger"`
	Ref          string  `json:"ref,omitempty"`
	Status       string  `json:"status"`
	CostUSD      float64 `json:"costUsd"`
	PagesCreated int     `json:"pagesCreated"`
	PagesUpdated int     `json:"pagesUpdated"`
	PagesDeleted int     `json:"pagesDeleted"`
	Error        string  `json:"error,omitempty"`
	ClaimedBy    string  `json:"claimedBy,omitempty"`
	Created      string  `json:"created"`
	Started      string  `json:"started,omitempty"`
	Finished     string  `json:"finished,omitempty"`
}

// handleRunsList returns a workspace's runs, newest first. Deliberately not
// cached on the wiki revision: a run changes status without bumping any
// revision, and a stale dashboard defeats its purpose.
func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	limit, offset := pagination(r, defaultRunLimit, maxPageLimit)

	rows, err := s.Runs.ListRuns(r.Context(), ws.ID, limit, offset)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]RunSummaryJSON, 0, len(rows))
	for _, run := range rows {
		out = append(out, RunSummaryJSON{
			ID: run.ID, Trigger: run.Trigger, Ref: run.Ref, Status: run.Status,
			CostUSD:      run.CostUSD,
			PagesCreated: run.PagesCreated, PagesUpdated: run.PagesUpdated,
			PagesDeleted: run.PagesDeleted,
			Error:        run.Error, ClaimedBy: run.ClaimedBy,
			Created:  run.CreatedAt.UTC().Format(time.RFC3339),
			Started:  timeOrEmpty(run.StartedAt),
			Finished: timeOrEmpty(run.FinishedAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRunCreate enqueues a build. 202 whether the run was created or
// debounced onto an already-active one -- either way, a build is coming --
// with "created" distinguishing the two for the UI.
//
// Refusal is reserved for the budget window: when the workspace has spent its
// budget_usd within the rolling window, the request is rejected and a review
// item is filed, so the condition surfaces in the queue humans already watch
// rather than in a status code nobody polls.
func (s *Server) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.guardWrite(w, r, maxResolveBytes)
	if !ok {
		return
	}

	if refused, err := s.refuseOverBudget(r.Context(), ws); err != nil {
		s.fail(w, err)
		return
	} else if refused != "" {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": refused})
		return
	}

	runID, created, err := s.Runs.EnqueueRun(r.Context(), ws.ID, "manual", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": runID, "created": created})
}

// refuseOverBudget checks the rolling budget window. Returns a non-empty
// refusal message when the workspace is over budget; enforcement is at
// enqueue on purpose, because it is the last moment a refusal costs nothing.
func (s *Server) refuseOverBudget(ctx context.Context, ws store.WorkspaceRow) (string, error) {
	if s.BudgetWindow <= 0 {
		return "", nil
	}
	budget, err := s.Runs.WorkspaceBudgetUSD(ctx, ws.ID)
	if err != nil {
		return "", err
	}
	if budget == nil || *budget <= 0 {
		return "", nil
	}
	spent, err := s.Runs.SpendInWindow(ctx, ws.ID, s.BudgetWindow)
	if err != nil {
		return "", err
	}
	if spent < *budget {
		return "", nil
	}

	detail := fmt.Sprintf(
		"Builds are paused: $%.2f of the $%.2f budget was spent in the last %s. "+
			"Raise the bench budget or wait for the window to roll on; runs resume as soon as spend is back under the cap.",
		spent, *budget, s.BudgetWindow)
	// Filing can fail independently of the refusal; the refusal stands either
	// way, so the error is only logged.
	if err := s.Runs.FileReview(ctx, ws.ID, "budget", "budget window exceeded", detail); err != nil && s.Log != nil {
		s.Log.Error("filing budget review failed", "err", err)
	}
	return fmt.Sprintf("budget window exceeded: $%.2f of $%.2f spent in the last %s",
		spent, *budget, s.BudgetWindow), nil
}

func timeOrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
