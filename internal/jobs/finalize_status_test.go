package jobs

import "testing"

func TestFinalizeStatus(t *testing.T) {
	ok := ItemSummary{Key: "module:a", Status: StatusSucceeded}
	bad := ItemSummary{Key: "module:b", Status: StatusFailed, Err: "validation failed"}
	otherBad := ItemSummary{Key: "doc:c", Status: StatusFailed, Err: "timed out"}
	// A failed item that never recorded why: it must not become an error.
	mute := ItemSummary{Key: "module:d", Status: StatusFailed}

	tests := []struct {
		name       string
		in         RunSummary
		wantStatus string
		wantErr    string
	}{
		{
			name:       "succeeded",
			in:         RunSummary{Status: StatusRunning, Items: []ItemSummary{ok, ok}},
			wantStatus: StatusSucceeded,
		},
		{
			name:       "no items is no_changes",
			in:         RunSummary{Status: StatusRunning},
			wantStatus: StatusNoChanges,
		},
		{
			name:       "no_changes set earlier is kept",
			in:         RunSummary{Status: StatusNoChanges},
			wantStatus: StatusNoChanges,
		},
		{
			name:       "partial carries the first failed item's error",
			in:         RunSummary{Status: StatusRunning, Items: []ItemSummary{ok, bad, otherBad}},
			wantStatus: StatusPartial,
			wantErr:    "module:b: validation failed",
		},
		{
			name:       "failed carries the first failed item's error",
			in:         RunSummary{Status: StatusRunning, Items: []ItemSummary{otherBad, bad}},
			wantStatus: StatusFailed,
			wantErr:    "doc:c: timed out",
		},
		{
			name:       "failed items with no message leave the error empty",
			in:         RunSummary{Status: StatusRunning, Items: []ItemSummary{mute}},
			wantStatus: StatusFailed,
		},
		{
			name:       "the first failed item with a message wins over a mute one",
			in:         RunSummary{Status: StatusRunning, Items: []ItemSummary{mute, bad}},
			wantStatus: StatusFailed,
			wantErr:    "module:b: validation failed",
		},
		{
			// A halt is not re-derived from the items: the units that finished
			// before it would read as success.
			name:       "canceled is kept, not re-derived",
			in:         RunSummary{Status: StatusCanceled, Items: []ItemSummary{ok, ok}},
			wantStatus: StatusCanceled,
		},
		{
			name:       "canceled still reports a failed item",
			in:         RunSummary{Status: StatusCanceled, Items: []ItemSummary{ok, bad}},
			wantStatus: StatusCanceled,
			wantErr:    "module:b: validation failed",
		},
		{
			name:       "over_budget is kept, not re-derived",
			in:         RunSummary{Status: StatusOverBudget, Items: []ItemSummary{ok}},
			wantStatus: StatusOverBudget,
		},
		{
			name:       "over_budget still reports a failed item",
			in:         RunSummary{Status: StatusOverBudget, Items: []ItemSummary{bad}},
			wantStatus: StatusOverBudget,
			wantErr:    "module:b: validation failed",
		},
		{
			name:       "an error already on the summary is not overwritten",
			in:         RunSummary{Status: StatusRunning, Err: "earlier", Items: []ItemSummary{ok, bad}},
			wantStatus: StatusPartial,
			wantErr:    "earlier",
		},
		{
			name:       "a clean run drops no error it was given",
			in:         RunSummary{Status: StatusRunning, Err: "note", Items: []ItemSummary{ok}},
			wantStatus: StatusSucceeded,
			wantErr:    "note",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, errMsg := finalizeStatus(tc.in)
			if status != tc.wantStatus || errMsg != tc.wantErr {
				t.Errorf("finalizeStatus = (%q, %q), want (%q, %q)", status, errMsg, tc.wantStatus, tc.wantErr)
			}
		})
	}
}
