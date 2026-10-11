package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Hex letters on purpose, so the upper-case spelling below differs.
const benchA = "aaaaaaaa-1111-4111-8111-11111111111b"

func TestProposeScopeFormatIsValidated(t *testing.T) {
	cases := []struct {
		scope string
		want  string
		ok    bool
	}{
		{"propose:" + benchA, benchA, true},
		// A bare propose pins to nothing, so it grants nothing.
		{"propose", "", false},
		{"propose:", "", false},
		{"propose:demo", "", false},
		// Only the canonical spelling: a second spelling of the same bench
		// would be a second string to compare, and one to get wrong.
		{"propose:" + strings.ToUpper(benchA), "", false},
		{"propose:{" + benchA + "}", "", false},
		{"propose:" + benchA + " ", "", false},
		{"PROPOSE:" + benchA, "", false},
		{"write", "", false},
	}
	for _, tc := range cases {
		got, ok := ProposeWorkspace(tc.scope)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ProposeWorkspace(%q) = %q, %v; want %q, %v", tc.scope, got, ok, tc.want, tc.ok)
		}
	}
	if ProposeScope(benchA) != "propose:"+benchA {
		t.Errorf("ProposeScope = %q", ProposeScope(benchA))
	}

	id := Identity{Scopes: []string{"read", "propose:" + benchA, "propose:junk"}}
	if !id.CanProposeIn(benchA) {
		t.Error("a read+propose token cannot file into its own bench")
	}
	if id.CanProposeIn("22222222-2222-4222-8222-222222222222") || id.CanProposeIn("") {
		t.Error("a propose token is not pinned to its bench")
	}
	if (Identity{Scopes: []string{"propose:" + benchA}}).CanProposeIn(benchA) {
		t.Error("propose without read is admitted")
	}
}

// The middleware admits a propose-scoped token to a mutation only where the
// router's func says so, and checks the token itself regardless of what that
// func answers: a matcher that said yes to everything would still not let a
// read-only token, or a malformed propose scope, through.
func TestProposeScopePassesOnlyTheOptedInRoute(t *testing.T) {
	tokens := map[string][]string{
		"propose":       {"read", "propose:" + benchA},
		"read":          {"read"},
		"proposeNoRead": {"propose:" + benchA},
		"bare":          {"read", "propose"},
		"malformed":     {"read", "propose:demo"},
		"write":         {"read", "write"},
	}
	src := &fakeSource{byHash: map[string]Identity{}}
	plains := map[string]string{}
	for name, scopes := range tokens {
		plain, hash, _ := NewToken()
		plains[name] = plain
		src.byHash[hash] = Identity{UserID: "u1", Scopes: scopes}
	}

	optIn := func(r *http.Request, _ Identity) bool { return r.URL.Path == "/file" }
	permissive := func(*http.Request, Identity) bool { return true }

	cases := []struct {
		name   string
		mw     *Middleware
		token  string
		method string
		path   string
		want   int
	}{
		{"propose on the opted-in route", (Middleware{Source: src}).WithPropose(optIn), "propose", "POST", "/file", 200},
		{"propose anywhere else", (Middleware{Source: src}).WithPropose(optIn), "propose", "POST", "/other", 403},
		{"propose DELETE on the route", (Middleware{Source: src}).WithPropose(optIn), "propose", "DELETE", "/other", 403},
		{"propose still reads", (Middleware{Source: src}).WithPropose(optIn), "propose", "GET", "/other", 200},
		{"no opt-in at all", &Middleware{Source: src}, "propose", "POST", "/file", 403},
		{"read-only token, permissive matcher", (Middleware{Source: src}).WithPropose(permissive), "read", "POST", "/file", 403},
		{"propose without read", (Middleware{Source: src}).WithPropose(permissive), "proposeNoRead", "POST", "/file", 403},
		{"bare propose", (Middleware{Source: src}).WithPropose(permissive), "bare", "POST", "/file", 403},
		{"malformed propose", (Middleware{Source: src}).WithPropose(permissive), "malformed", "POST", "/file", 403},
		{"write is unaffected", (Middleware{Source: src}).WithPropose(optIn), "write", "POST", "/other", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			h := tc.mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+plains[tc.token])
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if reached != (tc.want == 200) {
				t.Errorf("handler reached = %v with status %d", reached, rec.Code)
			}
		})
	}
}

// WithPropose returns a copy: the upload group and any other user of the
// original middleware must not start admitting propose tokens because the
// API group opted in.
func TestWithProposeLeavesTheOriginalUnchanged(t *testing.T) {
	orig := &Middleware{}
	_ = orig.WithPropose(func(*http.Request, Identity) bool { return true })
	if orig.propose != nil {
		t.Fatal("WithPropose mutated the middleware it was called on")
	}
}

func TestIdentityCarriesTheTokensIDAndName(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	plain, err := Mint(ctx, pool, MintRequest{Login: "ada", Name: "laptop claude"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	id, err := (&PGSource{Pool: pool}).IdentityForToken(ctx, HashToken(plain))
	if err != nil {
		t.Fatalf("IdentityForToken: %v", err)
	}
	var want string
	if err := pool.QueryRow(ctx, `SELECT id FROM tokens WHERE name = 'laptop claude'`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if id.TokenID != want || id.TokenName != "laptop claude" {
		t.Errorf("identity token = %q %q, want %q %q", id.TokenID, id.TokenName, want, "laptop claude")
	}
}
