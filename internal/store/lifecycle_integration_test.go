package store

import (
	"context"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

func TestRequeueRunAfterInterruption(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	runID, _, err := s.EnqueueRun(ctx, ws, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx, "w1"); err != nil {
		t.Fatal(err)
	}

	if err := s.RequeueRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	// The same run is claimable again, cleanly.
	again, err := s.ClaimNextRun(ctx, "w2")
	if err != nil || again == nil || again.ID != runID {
		t.Fatalf("reclaim after requeue: %+v err=%v", again, err)
	}
	// Requeue only touches running rows: a finished run stays finished.
	if err := s.RecordRun(ctx, jobs.RunSummary{RunID: runID, WorkspaceID: ws,
		Trigger: "manual", Status: jobs.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueRun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != jobs.StatusSucceeded {
		t.Errorf("finished run requeued: %q", status)
	}
}

func TestSweepEnforcesRetention(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	// A page soft-deleted long ago, one recently; sessions and tokens on
	// both sides of their lines; an ancient finished run with spend.
	page := func(slug string) wiki.Page {
		p := wiki.Page{Path: "entities/" + slug + ".md", Slug: slug, Body: "# " + slug}
		p.Meta.Type, p.Meta.Title = "entity", slug
		p.Meta.Created, p.Meta.Updated = "2026-07-27", "2026-07-27"
		return p
	}
	if err := s.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, RunID: "run-x",
		UpsertPages: []wiki.Page{page("old"), page("fresh")}}); err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`UPDATE pages SET deleted_at = now() - interval '60 days' WHERE slug = 'old'`)
	mustExec(`UPDATE pages SET deleted_at = now() - interval '1 day' WHERE slug = 'fresh'`)

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (login) VALUES ('u') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	mustExec(`INSERT INTO sessions (id, user_id, expires_at) VALUES ('dead', $1, now() - interval '1 hour')`, userID)
	mustExec(`INSERT INTO sessions (id, user_id, expires_at) VALUES ('live', $1, now() + interval '1 hour')`, userID)
	mustExec(`INSERT INTO tokens (user_id, name, token_hash, scopes, revoked_at)
	          VALUES ($1, 'gone', 'h1', '{read}', now() - interval '30 days')`, userID)
	mustExec(`INSERT INTO tokens (user_id, name, token_hash, scopes) VALUES ($1, 'live', 'h2', '{read}')`, userID)
	mustExec(`INSERT INTO runs (workspace_id, trigger, status, finished_at, created_at)
	          VALUES ($1, 'manual', 'succeeded', now() - interval '90 days', now() - interval '90 days')`, ws)
	var oldRun string
	if err := pool.QueryRow(ctx, `SELECT id FROM runs WHERE workspace_id = $1 LIMIT 1`, ws).Scan(&oldRun); err != nil {
		t.Fatal(err)
	}
	mustExec(`INSERT INTO spend_ledger (workspace_id, run_id, amount_usd) VALUES ($1, $2, 1.00)`, ws, oldRun)

	swept, err := s.Sweep(ctx, 30*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if swept < 4 {
		t.Errorf("swept = %d, want at least the page, session, token, and run", swept)
	}

	count := func(q string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM pages WHERE slug = 'old'`); n != 0 {
		t.Error("old soft-deleted page survived")
	}
	if n := count(`SELECT count(*) FROM pages WHERE slug = 'fresh'`); n != 1 {
		t.Error("fresh soft-deleted page purged inside its recovery window")
	}
	if n := count(`SELECT count(*) FROM sessions WHERE id = 'dead'`); n != 0 {
		t.Error("expired session survived")
	}
	if n := count(`SELECT count(*) FROM sessions WHERE id = 'live'`); n != 1 {
		t.Error("live session purged")
	}
	if n := count(`SELECT count(*) FROM tokens`); n != 1 {
		t.Errorf("tokens = %d, want only the live one", n)
	}
	// The ledger outlives its run; budget windows depend on it.
	if n := count(`SELECT count(*) FROM spend_ledger`); n != 1 {
		t.Error("spend ledger row lost with its run")
	}
	if n := count(`SELECT count(*) FROM spend_ledger WHERE run_id IS NULL`); n != 1 {
		t.Error("ledger run_id not nulled by run deletion")
	}
}

func TestResealCredentialsRotatesAtomically(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	orgID, err := s.OrgOfWorkspace(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}

	oldKey, _ := crypto.NewKeyring("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	newKey, _ := crypto.NewKeyring("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	for _, secret := range []string{"ghp_one", "ghp_two"} {
		ct, nonce, err := oldKey.Seal([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateCredential(ctx, orgID, "git_pat", ct, nonce); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.ResealCredentials(ctx, oldKey.Open, newKey.Seal)
	if err != nil || n != 2 {
		t.Fatalf("reseal: n=%d err=%v", n, err)
	}
	// Everything opens under the new key now.
	metas, err := s.ListCredentialMeta(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		sealed, err := s.LoadSealedCredential(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newKey.Open(sealed.Ciphertext, sealed.Nonce); err != nil {
			t.Errorf("credential %s does not open under the new key: %v", m.ID, err)
		}
		if _, err := oldKey.Open(sealed.Ciphertext, sealed.Nonce); err == nil {
			t.Errorf("credential %s still opens under the old key", m.ID)
		}
	}

	// A wrong old key aborts the whole transaction: nothing half-rotates.
	wrong, _ := crypto.NewKeyring("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	if _, err := s.ResealCredentials(ctx, wrong.Open, oldKey.Seal); err == nil {
		t.Fatal("reseal under a wrong key succeeded")
	}
	sealedAfter, err := s.LoadSealedCredential(ctx, metas[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newKey.Open(sealedAfter.Ciphertext, sealedAfter.Nonce); err != nil {
		t.Error("failed reseal mutated credentials")
	}
}
