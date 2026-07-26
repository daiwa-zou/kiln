package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/auth"
)

func TestLimiterBurstAndRefill(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }

	// The full burst is available immediately; the next take is refused.
	for i := 0; i < writeBurst; i++ {
		if !l.take("u:1") {
			t.Fatalf("take %d refused inside the burst", i+1)
		}
	}
	if l.take("u:1") {
		t.Fatal("burst-exhausted bucket still granted a token")
	}

	// One refill interval buys exactly one token.
	now = now.Add(writeRefillEach)
	if !l.take("u:1") {
		t.Error("refilled token not granted")
	}
	if l.take("u:1") {
		t.Error("second token granted after a single refill interval")
	}

	// Buckets are per caller: another key is untouched.
	if !l.take("u:2") {
		t.Error("a different caller was throttled by the first one's bucket")
	}
}

func TestWriteLimiterAnswers429(t *testing.T) {
	l := newLimiter()
	handler := writeLimiter(l)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var last *httptest.ResponseRecorder
	for i := 0; i <= writeBurst; i++ {
		last = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "10.1.2.3:5555"
		handler.ServeHTTP(last, req)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("request %d = %d, want 429", writeBurst+1, last.Code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("429 carries no Retry-After")
	}
}

func TestLimiterKeyPrefersIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.1.2.3:5555"

	if got := limiterKey(req); got != "a:10.1.2.3" {
		t.Errorf("anonymous key = %q, want a:10.1.2.3", got)
	}

	// With an identity, the bucket follows the user across addresses. The
	// planted identity needs the read scope or the middleware 403s before the
	// context is ever populated.
	ctx := authContext(req.Context(), auth.Identity{UserID: "u-9", Scopes: []string{"read"}})
	if got := limiterKey(req.WithContext(ctx)); got != "u:u-9" {
		t.Errorf("authenticated key = %q, want u:u-9", got)
	}
}

// authContext plants an identity the way auth.Middleware does. The context
// key is unexported, so this round-trips through a real middleware pass.
func authContext(ctx context.Context, id auth.Identity) context.Context {
	var out context.Context
	m := &auth.Middleware{Source: plantSource{id}}
	h := m.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		out = r.Context()
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer kiln_"+repeatHex64())
	h.ServeHTTP(httptest.NewRecorder(), req)
	return out
}

type plantSource struct{ id auth.Identity }

func (p plantSource) IdentityForToken(context.Context, string) (auth.Identity, error) {
	return p.id, nil
}

func repeatHex64() string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
