package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeSessions maps one known session hash to an identity + CSRF token.
type fakeSessions struct {
	hash string
	id   Identity
	csrf string
}

func (f *fakeSessions) IdentityForSession(_ context.Context, hash string) (Identity, string, error) {
	if hash == f.hash {
		return f.id, f.csrf, nil
	}
	return Identity{}, "", ErrUnauthenticated
}

// fakeTokens rejects everything, so cookie paths cannot accidentally pass on
// the token branch.
type fakeTokens struct{}

func (fakeTokens) IdentityForToken(context.Context, string) (Identity, error) {
	return Identity{}, ErrUnauthenticated
}

func sessionMiddleware(csrf string) (*Middleware, string) {
	const plain = "kiln_sessioncookievalue1234567890abcdefghij"
	return &Middleware{
		Source: fakeTokens{},
		Sessions: &fakeSessions{
			hash: HashToken(plain),
			id:   Identity{UserID: "u1", Scopes: []string{"read", "write"}},
			csrf: csrf,
		},
	}, plain
}

func do(m *Middleware, method, cookie, csrfHeader string) *httptest.ResponseRecorder {
	var handled bool
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(method, "/api/v1/workspaces", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	}
	if csrfHeader != "" {
		req.Header.Set(CSRFHeader, csrfHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	_ = handled
	return rec
}

func TestSessionCookieAuthenticatesReads(t *testing.T) {
	m, plain := sessionMiddleware("csrf-token")

	if rec := do(m, http.MethodGet, plain, ""); rec.Code != http.StatusNoContent {
		t.Errorf("GET with session cookie = %d, want pass-through", rec.Code)
	}
	if rec := do(m, http.MethodGet, "wrong-cookie-value-entirely-000000000000", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET with bogus cookie = %d, want 401", rec.Code)
	}
	if rec := do(m, http.MethodGet, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET with nothing = %d, want 401", rec.Code)
	}
}

func TestSessionMutationsRequireCSRF(t *testing.T) {
	m, plain := sessionMiddleware("csrf-token")

	// This is the M3 verification case: a CSRF probe on a mutating route
	// fails without the token.
	if rec := do(m, http.MethodPost, plain, ""); rec.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF header = %d, want 403", rec.Code)
	}
	if rec := do(m, http.MethodPost, plain, "wrong"); rec.Code != http.StatusForbidden {
		t.Errorf("POST with wrong CSRF = %d, want 403", rec.Code)
	}
	if rec := do(m, http.MethodPost, plain, "csrf-token"); rec.Code != http.StatusNoContent {
		t.Errorf("POST with matching CSRF = %d, want pass-through", rec.Code)
	}
	if rec := do(m, http.MethodDelete, plain, "csrf-token"); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE with matching CSRF = %d, want pass-through", rec.Code)
	}
}

func TestSessionWithoutStoredCSRFNeverMutates(t *testing.T) {
	// An empty stored token must not compare equal to an absent header.
	m, plain := sessionMiddleware("")
	if rec := do(m, http.MethodPost, plain, ""); rec.Code != http.StatusForbidden {
		t.Errorf("POST on csrf-less session = %d, want 403", rec.Code)
	}
	// Reads still work: the session is valid, just mutation-incapable.
	if rec := do(m, http.MethodGet, plain, ""); rec.Code != http.StatusNoContent {
		t.Errorf("GET on csrf-less session = %d, want pass-through", rec.Code)
	}
}

func TestBearerHeaderWinsOverCookie(t *testing.T) {
	m, plain := sessionMiddleware("csrf-token")

	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	// A request carrying both a (rejected) bearer token and a valid session
	// cookie must fail on the token: API clients keep exact token semantics.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces", nil)
	req.Header.Set("Authorization", "Bearer kiln_notarealtokenbutwellformed12345678901234")
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: plain})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad bearer + good cookie = %d, want 401 (header wins)", rec.Code)
	}
}

func TestTokenOnlyMiddlewareIgnoresCookies(t *testing.T) {
	// Sessions nil: the deployment never enabled sign-in, so a cookie is
	// noise, not a credential.
	m := &Middleware{Source: fakeTokens{}}
	if rec := do(m, http.MethodGet, "kiln_whatever00000000000000000000000000000", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie on token-only middleware = %d, want 401", rec.Code)
	}
}
