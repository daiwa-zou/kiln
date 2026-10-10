package auth

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

// TestCookieSecure pins where the Secure attribute comes from. A configured
// public_url is authoritative: a client-supplied X-Forwarded-Proto must be
// able neither to switch Secure off behind an https origin nor to switch it
// on for a plain-http one. Only an unconfigured deployment falls back to the
// request.
func TestCookieSecure(t *testing.T) {
	tests := []struct {
		name      string
		publicURL string
		forwarded string
		want      bool
	}{
		{"https origin ignores a forwarded http claim", "https://kiln.example.com", "http", true},
		{"https origin with no header", "https://kiln.example.com", "", true},
		{"https scheme is case-insensitive", "HTTPS://kiln.example.com", "", true},
		{"http loopback origin ignores a forwarded https claim", "http://localhost:8080", "https", false},
		{"unconfigured follows forwarded https", "", "https", true},
		{"unconfigured, no header", "", "", false},
		{"blank is unconfigured", "  ", "https", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tt.forwarded)
			}
			if got := CookieSecure(r, tt.publicURL); got != tt.want {
				t.Errorf("CookieSecure(public_url=%q, X-Forwarded-Proto=%q) = %v, want %v",
					tt.publicURL, tt.forwarded, got, tt.want)
			}
		})
	}
}

func TestCookieSecureFromTLS(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://kiln.example.com/", nil)
	r.TLS = &tls.ConnectionState{}
	if !CookieSecure(r, "") {
		t.Error("an unconfigured deployment serving TLS itself must set Secure")
	}
	if CookieSecure(r, "http://127.0.0.1:8080") {
		t.Error("a configured http origin must win over the connection state")
	}
}

func TestSetSessionCookiesSecureFollowsPublicURL(t *testing.T) {
	for _, tt := range []struct {
		publicURL string
		forwarded string
		want      bool
	}{
		{"https://kiln.example.com", "http", true},
		{"http://localhost:8080", "https", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Forwarded-Proto", tt.forwarded)
		w := httptest.NewRecorder()
		SetSessionCookies(w, r, tt.publicURL, "sess", "csrf", time.Hour)
		cookies := w.Result().Cookies()
		if len(cookies) != 2 {
			t.Fatalf("want session and csrf cookies, got %d", len(cookies))
		}
		for _, c := range cookies {
			if c.Secure != tt.want {
				t.Errorf("public_url=%q forwarded=%q: cookie %s Secure=%v, want %v",
					tt.publicURL, tt.forwarded, c.Name, c.Secure, tt.want)
			}
		}
	}
}
