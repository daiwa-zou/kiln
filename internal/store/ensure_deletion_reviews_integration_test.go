package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// TestEnsureDeletionReviewsFilesABatchWithTheSameDedupe covers the shape that
// motivated batching -- a namespace vanishing and raising thousands of
// candidates at once -- while holding to the per-candidate rules: a key with a
// live (open or approved) review is skipped, a key whose earlier review was
// resolved is asked about again, and a key repeated in the input is filed once.
func TestEnsureDeletionReviewsFilesABatchWithTheSameDedupe(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	pool := js.Pool()

	seed := func(title, status string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO review_items (workspace_id, kind, title, detail, status)
			VALUES ($1, 'deletion', $2, 'earlier', $3)`, ws, title, status); err != nil {
			t.Fatal(err)
		}
	}
	seed("doc:live-open", "open")
	seed("doc:live-approved", "approved")
	seed("doc:was-kept", "resolved")

	const n = 2000
	cands := []jobs.DeletionCandidate{
		{Key: diff.Key("doc:live-open"), Detail: "new"},
		{Key: diff.Key("doc:live-approved"), Detail: "new"},
		{Key: diff.Key("doc:was-kept"), Detail: "again"},
		{Key: diff.Key("doc:twice"), Detail: "first"},
		{Key: diff.Key("doc:twice"), Detail: "second"},
	}
	for i := 0; i < n; i++ {
		cands = append(cands, jobs.DeletionCandidate{
			Key:    diff.Key(fmt.Sprintf("doc:gone/%04d.md", i)),
			Detail: fmt.Sprintf("detail %d", i),
		})
	}

	start := time.Now()
	if err := js.EnsureDeletionReviews(ctx, ws, cands); err != nil {
		t.Fatal(err)
	}
	t.Logf("filed %d candidates in %v", len(cands), time.Since(start))
	// Asked again with the same input, nothing new is filed.
	if err := js.EnsureDeletionReviews(ctx, ws, cands); err != nil {
		t.Fatal(err)
	}

	count := func(where string, args ...any) int {
		t.Helper()
		var c int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM review_items WHERE workspace_id = $1 AND kind = 'deletion' AND `+where,
			append([]any{ws}, args...)...).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got, want := count("true"), 3+1+1+n; got != want { // seeded + was-kept refiled + twice + the rest
		t.Errorf("deletion reviews = %d, want %d", got, want)
	}
	if got := count("title = 'doc:live-open'"); got != 1 {
		t.Errorf("live open key has %d reviews, want 1", got)
	}
	if got := count("title = 'doc:live-approved'"); got != 1 {
		t.Errorf("live approved key has %d reviews, want 1", got)
	}
	if got := count("title = 'doc:was-kept'"); got != 2 {
		t.Errorf("resolved key has %d reviews, want 2 (the old one and a new open one)", got)
	}
	if got := count("title = 'doc:was-kept' AND status = 'open' AND detail = 'again'"); got != 1 {
		t.Errorf("resolved key was not refiled as an open review")
	}
	if got := count("title = 'doc:twice' AND status = 'open' AND detail = 'first'"); got != 1 {
		t.Errorf("repeated key was not filed once from its first occurrence")
	}
	if got := count("title = 'doc:twice'"); got != 1 {
		t.Errorf("repeated key has %d reviews, want 1", got)
	}
	if got := count("title = 'doc:gone/0042.md' AND detail = 'detail 42' AND actions = '[\"approve\",\"keep\"]'::jsonb"); got != 1 {
		t.Errorf("a batched candidate lost its detail or actions")
	}

	// No candidates is not an error and files nothing.
	if err := js.EnsureDeletionReviews(ctx, ws, nil); err != nil {
		t.Fatal(err)
	}
}
