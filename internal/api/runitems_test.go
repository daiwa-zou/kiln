package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

type runItemJSON struct {
	Key        string   `json:"key"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	CostUSD    float64  `json:"costUsd"`
	EstCostUSD *float64 `json:"estCostUsd"`
	Turns      int      `json:"turns"`
	Tokens     int64    `json:"tokens"`
	Error      string   `json:"error"`
}

func TestRunItemsAreOrderedForWhateverTheRunIsDoing(t *testing.T) {
	srv, js, wsID := testServer(t)
	ctx := context.Background()

	runID, _, err := js.EnqueueRun(ctx, wsID, "manual", "")
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	keys := []diff.Key{diff.ModuleKey("ripple"), diff.ModuleKey("beacon"), diff.ModuleKey("sawmill")}
	if err := js.SeedRunItems(ctx, runID, keys, 0.25); err != nil {
		t.Fatalf("SeedRunItems: %v", err)
	}
	// One cheap success, one expensive success, one failure; the third stays
	// pending.
	for _, item := range []jobs.ItemSummary{
		{Key: keys[0], Status: "succeeded", CostUSD: 0.10, Turns: 2, Tokens: 1200},
		{Key: keys[1], Status: "failed", CostUSD: 0.40, Turns: 1, Tokens: 900, Err: "validation violation"},
	} {
		if err := js.MarkRunItem(ctx, runID, item); err != nil {
			t.Fatalf("MarkRunItem %s: %v", item.Key, err)
		}
	}

	var items []runItemJSON
	if code := get(t, srv, "/api/v1/workspaces/demo/runs/"+runID+"/items", &items); code != http.StatusOK {
		t.Fatalf("GET run items = %d", code)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3: %+v", len(items), items)
	}

	// Unsettled work first, then the rest costliest-first: what is still
	// happening while the run is live, what to look at once it is over.
	if items[0].Key != string(keys[2]) || items[0].Status != "pending" {
		t.Errorf("first item = %+v, want the pending unit", items[0])
	}
	if items[1].Key != string(keys[1]) || items[1].CostUSD != 0.40 {
		t.Errorf("second item = %+v, want the costliest settled unit", items[1])
	}
	if items[1].Error != "validation violation" {
		t.Errorf("the failed unit did not carry its reason: %+v", items[1])
	}
	if items[2].Error != "" {
		t.Errorf("a unit that succeeded reported an error: %+v", items[2])
	}
	if items[0].EstCostUSD == nil || *items[0].EstCostUSD != 0.25 {
		t.Errorf("estimate missing from the pending unit: %+v", items[0])
	}
	if items[1].Kind != "module" || items[1].Turns != 1 || items[1].Tokens != 900 {
		t.Errorf("second item lost its accounting: %+v", items[1])
	}
}

func TestRunItemsFromAnotherBenchReadAsEmpty(t *testing.T) {
	// Scoping is in the query, so a run id belonging to someone else is not a
	// 404 that confirms it exists -- it is an empty list.
	srv, js, wsID := testServer(t)
	ctx := context.Background()

	otherWS, err := js.EnsureWorkspace(ctx, "other-org", "other", "Other")
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	runID, _, err := js.EnqueueRun(ctx, otherWS, "manual", "")
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	if err := js.SeedRunItems(ctx, runID, []diff.Key{diff.ModuleKey("secret")}, 0.1); err != nil {
		t.Fatalf("SeedRunItems: %v", err)
	}
	_ = wsID

	var items []runItemJSON
	if code := get(t, srv, "/api/v1/workspaces/demo/runs/"+runID+"/items", &items); code != http.StatusOK {
		t.Fatalf("GET run items = %d, want 200", code)
	}
	if len(items) != 0 {
		t.Errorf("another bench's run items leaked: %+v", items)
	}
}

func TestRunItemsForAnUnknownRunAreEmptyNotNull(t *testing.T) {
	srv, _, _ := testServer(t)

	var items []runItemJSON
	code := get(t, srv, "/api/v1/workspaces/demo/runs/6f2f4a1e-0000-4000-8000-000000000000/items", &items)
	if code != http.StatusOK {
		t.Fatalf("GET run items = %d, want 200", code)
	}
	if items == nil || len(items) != 0 {
		t.Errorf("items = %+v, want an empty array", items)
	}
}
