package auth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/store"
)

// pgPool mirrors the store package's test harness: skip without a database,
// reset the schema so every test starts from empty. Duplicated rather than
// imported because test helpers do not cross package boundaries.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("KILN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KILN_TEST_DATABASE_URL not set; skipping Postgres auth test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	// The auth tables live in migration 001; apply everything the same way
	// production does rather than hand-rolling a parallel schema. store does
	// not import auth, so the dependency is acyclic.
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func TestMintCreatesTheWholeChain(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	plain, err := Mint(ctx, pool, MintRequest{Login: "ada", Org: "acme"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !looksLikeToken(plain) {
		t.Fatalf("minted token %q does not look like a token", plain)
	}

	// The plaintext is returned exactly once; what is stored must be its hash.
	id, err := (&PGSource{Pool: pool}).IdentityForToken(ctx, HashToken(plain))
	if err != nil {
		t.Fatalf("IdentityForToken: %v", err)
	}
	if id.Admin {
		t.Error("plain mint produced an admin")
	}
	if len(id.Scopes) != 1 || id.Scopes[0] != "read" {
		t.Errorf("default scopes = %v, want [read]", id.Scopes)
	}

	// User, org, and membership all exist after one call.
	var members int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM org_members m
		JOIN users u ON u.id = m.user_id
		JOIN orgs o ON o.id = m.org_id
		WHERE u.login = 'ada' AND o.slug = 'acme'`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 1 {
		t.Errorf("memberships = %d, want 1", members)
	}
}

func TestMintReusesTheUserByLogin(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	if _, err := Mint(ctx, pool, MintRequest{Login: "ada", Org: "acme"}); err != nil {
		t.Fatalf("first Mint: %v", err)
	}
	// users.login has no unique constraint (github_user_id is the canonical
	// key); the documented contract is deterministic reuse by login.
	if _, err := Mint(ctx, pool, MintRequest{Login: "ada", Org: "acme", Name: "second"}); err != nil {
		t.Fatalf("second Mint: %v", err)
	}

	var users, tokens int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE login = 'ada'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Errorf("users named ada = %d, want 1 (reused, not duplicated)", users)
	}
	if tokens != 2 {
		t.Errorf("tokens = %d, want 2", tokens)
	}
}

func TestMintPromotesAnExistingUserToAdmin(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	first, err := Mint(ctx, pool, MintRequest{Login: "ada"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := Mint(ctx, pool, MintRequest{Login: "ada", Name: "admin-tok", Admin: true}); err != nil {
		t.Fatalf("admin Mint: %v", err)
	}

	// Promotion applies to the user, so even the first token now carries it.
	id, err := (&PGSource{Pool: pool}).IdentityForToken(ctx, HashToken(first))
	if err != nil {
		t.Fatalf("IdentityForToken: %v", err)
	}
	if !id.Admin {
		t.Error("existing user was not promoted by an admin mint")
	}
}

func TestMintValidation(t *testing.T) {
	pool := pgPool(t)

	if _, err := Mint(context.Background(), pool, MintRequest{}); err == nil {
		t.Error("mint without a login succeeded; a token must belong to someone")
	}
}

func TestExpiredAndRevokedTokensDoNotAuthenticate(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	src := &PGSource{Pool: pool}

	expired, err := Mint(ctx, pool, MintRequest{Login: "ada", Name: "short", TTL: time.Millisecond})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := src.IdentityForToken(ctx, HashToken(expired)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired token = %v, want ErrUnauthenticated", err)
	}

	live, err := Mint(ctx, pool, MintRequest{Login: "ada", Name: "live"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := src.IdentityForToken(ctx, HashToken(live)); err != nil {
		t.Fatalf("live token rejected: %v", err)
	}

	n, err := Revoke(ctx, pool, "ada", "live")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if n != 1 {
		t.Errorf("revoked = %d, want 1", n)
	}
	if _, err := src.IdentityForToken(ctx, HashToken(live)); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked token = %v, want ErrUnauthenticated", err)
	}

	// Idempotence: an already-revoked token is not recounted, and an unknown
	// login is zero rather than an error.
	if n, _ := Revoke(ctx, pool, "ada", "live"); n != 0 {
		t.Errorf("second revoke = %d, want 0", n)
	}
	if n, _ := Revoke(ctx, pool, "nobody", "default"); n != 0 {
		t.Errorf("revoke for unknown login = %d, want 0", n)
	}
}

func TestUnknownTokenIsUnauthenticated(t *testing.T) {
	pool := pgPool(t)

	_, err := (&PGSource{Pool: pool}).IdentityForToken(context.Background(), HashToken("kiln_nope"))
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("unknown token = %v, want ErrUnauthenticated", err)
	}
}

func TestLastUsedIsWrittenOnceThenThrottled(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	src := &PGSource{Pool: pool}

	plain, err := Mint(ctx, pool, MintRequest{Login: "ada"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := src.IdentityForToken(ctx, HashToken(plain)); err != nil {
		t.Fatal(err)
	}
	var first *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_used_at FROM tokens`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("last_used_at not written on first use")
	}

	// Within the 60s window a second use must not rewrite it: telemetry is
	// throttled so a busy token is not a write per request.
	if _, err := src.IdentityForToken(ctx, HashToken(plain)); err != nil {
		t.Fatal(err)
	}
	var second *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_used_at FROM tokens`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if !second.Equal(*first) {
		t.Errorf("last_used_at rewritten within the throttle window: %v -> %v", first, second)
	}
}
