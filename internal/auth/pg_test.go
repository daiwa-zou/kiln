package auth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

// memberRole reads the role Mint left for login in org, or "" when there is
// no membership.
func memberRole(t *testing.T, pool *pgxpool.Pool, login, org string) string {
	t.Helper()
	var role string
	err := pool.QueryRow(context.Background(), `
		SELECT m.role FROM org_members m
		JOIN users u ON u.id = m.user_id
		JOIN orgs o ON o.id = m.org_id
		WHERE u.login = $1 AND o.slug = $2`, login, org).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return role
}

// auth.default_role reaches the membership row: a viewer minted with --org
// is a viewer, not the column default.
func TestMintHonoursTheRequestedRole(t *testing.T) {
	pool := pgPool(t)

	_, m, err := MintWithMembership(context.Background(), pool, MintRequest{Login: "ada", Org: "acme", Role: "viewer"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := memberRole(t, pool, "ada", "acme"); got != "viewer" {
		t.Errorf("role = %q, want viewer", got)
	}
	// What the CLI prints must be what was written.
	if m != (Membership{Org: "acme", Role: "viewer", Created: true}) {
		t.Errorf("reported membership = %+v, want a new acme viewer", m)
	}
}

func TestMintDefaultsToMember(t *testing.T) {
	pool := pgPool(t)

	if _, err := Mint(context.Background(), pool, MintRequest{Login: "ada", Org: "acme"}); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := memberRole(t, pool, "ada", "acme"); got != "member" {
		t.Errorf("role = %q with no Role set, want member", got)
	}
}

// The one path that creates memberships without an owner granting them must
// never create an owner, and refusing must leave nothing half-written.
func TestMintRejectsOwner(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	if _, err := Mint(ctx, pool, MintRequest{Login: "ada", Org: "acme", Role: "owner"}); err == nil {
		t.Fatal("Mint with role owner succeeded")
	}
	for _, table := range []string{"users", "orgs", "org_members", "tokens"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows after a refused mint, want 0", table, n)
		}
	}
}

// Re-minting under default_role=viewer must not demote someone an owner
// already made a member: the setting is for new memberships only.
func TestMintDoesNotDemoteAnExistingMember(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	if _, err := Mint(ctx, pool, MintRequest{Login: "ada", Org: "acme", Role: "member"}); err != nil {
		t.Fatalf("first Mint: %v", err)
	}
	_, m, err := MintWithMembership(ctx, pool, MintRequest{Login: "ada", Org: "acme", Name: "second", Role: "viewer"})
	if err != nil {
		t.Fatalf("second Mint: %v", err)
	}
	if got := memberRole(t, pool, "ada", "acme"); got != "member" {
		t.Errorf("role = %q after a viewer re-mint, want member (unchanged)", got)
	}
	// The report names the role kept, not the one asked for, so the operator
	// is not told a viewer exists when it does not.
	if m != (Membership{Org: "acme", Role: "member", Created: false}) {
		t.Errorf("reported membership = %+v, want the existing acme member", m)
	}

	// No org, no membership to report.
	if _, m, err := MintWithMembership(ctx, pool, MintRequest{Login: "ada", Name: "third"}); err != nil || m != (Membership{}) {
		t.Errorf("mint without an org reported %+v (err %v), want none", m, err)
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
