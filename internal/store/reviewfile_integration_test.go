package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// agentFixture is a bench with one live page and n API keys, returning the
// workspace id and the keys' ids (origin_token is a plain id, kept after the key is gone).
func agentFixture(t *testing.T, pool *pgxpool.Pool, keys int) (string, []string) {
	t.Helper()
	ctx := context.Background()
	freshSchema(t, pool)
	wikiID := seedWorkspace(t, pool)
	if err := insertPage(t, pool, wikiID, "entities/ripple.md", "entity", "ripple", "Ripple", "body"); err != nil {
		t.Fatal(err)
	}
	var wsID, userID string
	if err := pool.QueryRow(ctx, `SELECT workspace_id FROM wikis WHERE id = $1`, wikiID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (login) VALUES ('agent-owner') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, keys)
	for i := range ids {
		if err := pool.QueryRow(ctx, `
			INSERT INTO tokens (user_id, name, token_hash, scopes)
			VALUES ($1, $2, $3, ARRAY['read']) RETURNING id`,
			userID, fmt.Sprintf("key-%d", i), fmt.Sprintf("hash-%d", i)).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	return wsID, ids
}

// The cap is read and enforced under one per-bench lock. Without it, filings
// racing past the count would each see room and all land: the cap would be a
// suggestion exactly when an agent loops hardest.
func TestFileAgentReviewCapIsExactUnderConcurrency(t *testing.T) {
	pool := testPool(t)
	ws, keys := agentFixture(t, pool, 1)
	s := NewWikiStore(pool)
	ctx := context.Background()

	const callers = AgentOpenCapPerKey + 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		filed   int
		capped  int
		failure error
	)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, outcome, err := s.FileAgentReview(ctx, FileReviewParams{
				WorkspaceID: ws, Kind: "uncertain", PageRef: "ripple",
				Title: fmt.Sprintf("question %d", i), Origin: "key-0", OriginKind: OriginKey, OriginToken: keys[0],
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrAgentCap):
				capped++
			case err != nil:
				failure = err
			case outcome == FiledNew:
				filed++
			}
		}()
	}
	wg.Wait()
	if failure != nil {
		t.Fatal(failure)
	}
	if filed != AgentOpenCapPerKey || capped != callers-AgentOpenCapPerKey {
		t.Errorf("filed %d and capped %d of %d, want exactly %d filed", filed, capped, callers, AgentOpenCapPerKey)
	}
	var open int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM review_items WHERE status = 'open'`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != AgentOpenCapPerKey {
		t.Errorf("%d open items, want %d", open, AgentOpenCapPerKey)
	}
}

// The housekeeping sweep deletes a token a week after it is revoked. Its open
// items must still count against both caps and still read as key-filed:
// otherwise revoking an agent's key would free it from its cap, and its items
// would lose the badge that says a program filed them.
func TestAgentItemsOutliveTheSweptKey(t *testing.T) {
	pool := testPool(t)
	ws, keys := agentFixture(t, pool, 1)
	s := NewWikiStore(pool)
	ctx := context.Background()

	for i := range AgentOpenCapPerKey {
		if _, _, err := s.FileAgentReview(ctx, FileReviewParams{
			WorkspaceID: ws, Kind: "uncertain", PageRef: "ripple", Title: fmt.Sprintf("q%d", i),
			Origin: "session", OriginKind: OriginKey, OriginToken: keys[0],
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE tokens SET revoked_at = now() - interval '8 days' WHERE id = $1`, keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sweep(ctx, 365*24*time.Hour, 365*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var tokens int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tokens`).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("the sweep left %d tokens (%v); the test needs the key gone", tokens, err)
	}

	rows, err := s.ListReviews(ctx, ws, "open", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		// Named "session" on purpose: the name is free text, the kind decides.
		if !r.AgentFiled || r.OriginKind != OriginKey {
			t.Fatalf("after the sweep a key-filed item reads %+v", r)
		}
	}
	if _, _, err := s.FileAgentReview(ctx, FileReviewParams{
		WorkspaceID: ws, Kind: "uncertain", PageRef: "ripple", Title: "one more",
		Origin: "key-0", OriginKind: OriginKey, OriginToken: keys[0],
	}); !errors.Is(err, ErrAgentCap) {
		t.Errorf("the swept key's next filing = %v, want ErrAgentCap", err)
	}
}

func TestFileAgentReviewDedupesAndSkipsTheCapForPeople(t *testing.T) {
	pool := testPool(t)
	ws, keys := agentFixture(t, pool, 1)
	s := NewWikiStore(pool)
	ctx := context.Background()

	q := FileReviewParams{WorkspaceID: ws, Kind: "contradiction", PageRef: "entities/ripple.md",
		Title: "Which is it?", Origin: "key-0", OriginKind: OriginKey, OriginToken: keys[0]}
	id1, out1, err := s.FileAgentReview(ctx, q)
	if err != nil || out1 != FiledNew {
		t.Fatalf("first filing = %q, %v", out1, err)
	}
	id2, out2, err := s.FileAgentReview(ctx, q)
	if err != nil || out2 != FiledAlreadyOpen || id2 != id1 {
		t.Fatalf("second filing = %q %q, %v; want already_open with the same id", id2, out2, err)
	}

	// Two different corrections to one page are two proposals; the title is
	// derived from the page, not taken from the caller.
	for _, text := range []string{"It is 5.", "It is 7."} {
		if _, out, err := s.FileAgentReview(ctx, FileReviewParams{
			WorkspaceID: ws, Kind: "correction", PageRef: "ripple", Title: "ignored",
			Proposed: text, Origin: "key-0", OriginKind: OriginKey, OriginToken: keys[0],
		}); err != nil || out != FiledNew {
			t.Fatalf("correction %q = %q, %v", text, out, err)
		}
	}
	rows, err := s.ListReviews(ctx, ws, "open", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	corrections := 0
	for _, r := range rows {
		if r.Kind == "correction" {
			corrections++
			if r.Title != "Proposed correction: ripple" || r.Proposed == "" || !r.AgentFiled || r.Origin != "key-0" {
				t.Errorf("correction row = %+v", r)
			}
		}
	}
	if corrections != 2 {
		t.Errorf("%d correction items, want 2", corrections)
	}

	// A person filing from a session is not a loop; past the key cap, their
	// filing still lands.
	if _, err := pool.Exec(ctx, `
		INSERT INTO review_items (workspace_id, kind, title, origin, origin_kind, origin_token)
		SELECT $1, 'uncertain', 'filler ' || g, 'key-0', 'key', $2 FROM generate_series(1, $3) g`,
		ws, keys[0], AgentOpenCapPerKey); err != nil {
		t.Fatal(err)
	}
	if _, out, err := s.FileAgentReview(ctx, FileReviewParams{
		WorkspaceID: ws, Kind: "uncertain", PageRef: "ripple", Title: "from a person", Origin: "session", OriginKind: OriginSession,
	}); err != nil || out != FiledNew {
		t.Errorf("a session filing past the key cap = %q, %v", out, err)
	}

	// A page outside the bench, or deleted, is not a page here.
	if _, _, err := s.FileAgentReview(ctx, FileReviewParams{
		WorkspaceID: ws, Kind: "uncertain", PageRef: "nowhere", Title: "x", Origin: "session", OriginKind: OriginSession,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing page = %v, want ErrNotFound", err)
	}
}
