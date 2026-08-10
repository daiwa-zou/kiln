package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

// keyServer mounts the agent-key routes: they need a pool to hold keys and an
// identity to own them, which is exactly the condition the router checks.
func keyServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, string, *mutableSource) {
	t.Helper()
	base, pool, wsID, src := authedServer(t)
	base.Close() // its router lacks SessionPool, so the key routes never mounted

	js := store.NewWikiStore(pool)
	srv := httptest.NewServer((&Server{
		Store: js, Writes: js, DB: &store.DB{Pool: pool},
		Auth:        &auth.Middleware{Source: src},
		SessionPool: pool,
	}).Router())
	t.Cleanup(srv.Close)
	return srv, pool, wsID, src
}

func keyCall(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kiln_valid")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func keyList(t *testing.T, srv *httptest.Server) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/tokens", nil)
	req.Header.Set("Authorization", "Bearer kiln_valid")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return out
}

func TestAgentKeyMintedOverTheAPIIsReadOnly(t *testing.T) {
	srv, pool, wsID, src := keyServer(t)
	ctx := context.Background()

	// An admin asking for an agent key: the endpoint must be a reduction of
	// authority, never a way to mint more than the session already has.
	owner := addMember(t, pool, wsID, "owner", "owner")
	src.id = auth.Identity{UserID: owner, Admin: true, Scopes: []string{"read", "write", "admin"}}

	code, made := keyCall(t, srv, http.MethodPost, "/tokens", `{"name":"laptop claude"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /tokens = %d, want 201 (%v)", code, made)
	}
	plain, _ := made["token"].(string)
	if plain == "" {
		t.Fatal("no plaintext key in the response")
	}
	scopes, _ := made["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != "read" {
		t.Errorf("scopes = %v, want read only", scopes)
	}

	// The key authenticates as its owner and carries nothing but read.
	id, err := (&auth.PGSource{Pool: pool}).IdentityForToken(ctx, auth.HashToken(plain))
	if err != nil {
		t.Fatalf("the minted key does not authenticate: %v", err)
	}
	if id.UserID != owner {
		t.Errorf("key authenticates as %q, want its owner", id.UserID)
	}
	if id.HasScope("write") || id.HasScope("admin") {
		t.Errorf("agent key carries %v; every MCP tool is a read", id.Scopes)
	}

	// Stored as a hash, and never handed back again.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM tokens WHERE name = $1`, "laptop claude").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == plain {
		t.Fatal("the key was stored in the clear")
	}
	for _, row := range keyList(t, srv) {
		if _, leaked := row["token"]; leaked {
			t.Error("the key list hands back the key itself")
		}
	}
}

func TestAgentKeysAreScopedToTheirOwner(t *testing.T) {
	srv, pool, wsID, src := keyServer(t)

	owner := addMember(t, pool, wsID, "owner", "owner")
	other := addMember(t, pool, wsID, "other", "member")

	// A browser session carries the user's full authority; scopes narrow
	// automation tokens, not people signed in at the keyboard.
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write"}}
	code, made := keyCall(t, srv, http.MethodPost, "/tokens", `{"name":"mine"}`)
	if code != http.StatusCreated {
		t.Fatalf("mint = %d", code)
	}
	id, _ := made["id"].(string)

	// Another user must not see it, and must not be able to revoke it -- and
	// must not be able to tell "not yours" from "does not exist".
	src.id = auth.Identity{UserID: other, Scopes: []string{"read", "write"}}
	if rows := keyList(t, srv); len(rows) != 0 {
		t.Errorf("another user sees %d keys, want none", len(rows))
	}
	if code, _ := keyCall(t, srv, http.MethodDelete, "/tokens/"+id, ""); code != http.StatusNotFound {
		t.Errorf("cross-user revoke = %d, want 404", code)
	}
	code, _ = keyCall(t, srv, http.MethodDelete, "/tokens/00000000-0000-0000-0000-000000000000", "")
	if code != http.StatusNotFound {
		t.Errorf("missing key = %d, want the same 404 as one that is not yours", code)
	}
	// A malformed id is the same answer, not a 500.
	if code, _ := keyCall(t, srv, http.MethodDelete, "/tokens/not-a-uuid", ""); code != http.StatusNotFound {
		t.Errorf("malformed id = %d, want 404", code)
	}

	// The owner can revoke, and the key stops working.
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write"}}
	if code, body := keyCall(t, srv, http.MethodDelete, "/tokens/"+id, ""); code != http.StatusOK {
		t.Fatalf("owner revoke = %d (%v)", code, body)
	}
	if rows := keyList(t, srv); len(rows) != 0 {
		t.Errorf("revoked key still listed: %+v", rows)
	}
	plain, _ := made["token"].(string)
	if _, err := (&auth.PGSource{Pool: pool}).IdentityForToken(context.Background(), auth.HashToken(plain)); err == nil {
		t.Error("a revoked key still authenticates")
	}
}

func TestAgentKeyRoutesAreAbsentWithoutAuth(t *testing.T) {
	// With auth disabled there is no identity to own a key and the routes are
	// not mounted: a key would guard nothing, so offering one would be a lie.
	srv := httptest.NewServer((&Server{}).Router())
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/api/v1/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /tokens with auth disabled = %d, want 404", res.StatusCode)
	}
}

func TestAReadOnlyKeyCannotMintAnotherKey(t *testing.T) {
	srv, pool, wsID, src := keyServer(t)
	owner := addMember(t, pool, wsID, "owner", "owner")

	// The keys this endpoint issues are read-scoped, which means one of them
	// cannot be used to issue another. That matters more than it looks: a key
	// that could mint keys would let a leaked one replace itself indefinitely
	// and outlive the revocation of the original.
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read"}}
	if code, _ := keyCall(t, srv, http.MethodPost, "/tokens", `{"name":"bootstrap"}`); code != http.StatusForbidden {
		t.Errorf("read-only caller minted a key: %d, want 403", code)
	}
	if code, _ := keyCall(t, srv, http.MethodDelete, "/tokens/00000000-0000-0000-0000-000000000000", ""); code != http.StatusForbidden {
		t.Errorf("read-only caller reached revoke: %d, want 403", code)
	}
	// Reading its own list is fine: that is a read.
	if rows := keyList(t, srv); len(rows) != 0 {
		t.Errorf("unexpected keys: %+v", rows)
	}
}
