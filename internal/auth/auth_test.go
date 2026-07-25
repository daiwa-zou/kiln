package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSource struct {
	byHash map[string]Identity
}

func (f *fakeSource) IdentityForToken(_ context.Context, hash string) (Identity, error) {
	if id, ok := f.byHash[hash]; ok {
		return id, nil
	}
	return Identity{}, ErrUnauthenticated
}

func newHandler(t *testing.T, src Source) (http.Handler, *bool) {
	t.Helper()
	reached := false
	m := &Middleware{Source: src}
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if _, ok := FromContext(r.Context()); !ok {
			t.Error("identity missing from request context")
		}
		w.WriteHeader(http.StatusOK)
	}))
	return h, &reached
}

func TestNewTokenShapeAndHash(t *testing.T) {
	plain, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Errorf("token %q lacks the %q prefix", plain, TokenPrefix)
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(hash))
	}
	if HashToken(plain) != hash {
		t.Error("HashToken does not round-trip NewToken's hash")
	}

	plain2, _, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if plain == plain2 {
		t.Error("two minted tokens are identical")
	}
}

func TestMiddlewareAcceptsValidToken(t *testing.T) {
	plain, hash, _ := NewToken()
	h, reached := newHandler(t, &fakeSource{byHash: map[string]Identity{
		hash: {UserID: "u1", Scopes: []string{"read"}},
	}})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	if !*reached {
		t.Error("handler was never reached")
	}
}

func TestMiddlewareRejectsMissingAndBadTokens(t *testing.T) {
	_, hash, _ := NewToken()
	h, reached := newHandler(t, &fakeSource{byHash: map[string]Identity{
		hash: {UserID: "u1", Scopes: []string{"read"}},
	}})

	tests := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Basic abc"},
		{"empty bearer", "Bearer "},
		{"not a kiln token", "Bearer sk-something-else"},
		{"unknown token", "Bearer " + TokenPrefix + "deadbeef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate challenge")
			}
		})
	}
	if *reached {
		t.Error("handler was reached without valid credentials")
	}
}

func TestMiddlewareEnforcesScope(t *testing.T) {
	plain, hash, _ := NewToken()
	h, reached := newHandler(t, &fakeSource{byHash: map[string]Identity{
		hash: {UserID: "u1", Scopes: []string{"read"}},
	}})

	// A read-scoped token must not authorize a mutation.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if *reached {
		t.Error("handler was reached despite the missing scope")
	}
}

func TestIdentityHasScope(t *testing.T) {
	id := Identity{Scopes: []string{"read", "write"}}
	if !id.HasScope("read") || !id.HasScope("write") {
		t.Error("granted scopes not reported")
	}
	if id.HasScope("admin") {
		t.Error("ungranted scope reported as present")
	}
}
