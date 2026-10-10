package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
)

// do sends one request through h with an explicit Host and Content-Type.
func do(h http.Handler, method, host, contentType string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+"/healthz", strings.NewReader(`{}`))
	r.Host = host
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestDecodeBodyRequiresJSON is the check that holds in every auth mode: a
// text/plain POST is a CORS "simple request", so a page on any origin can send
// it to 127.0.0.1 without a preflight. Parsing it as JSON anyway is what let
// such a page create connectors and queue runs.
func TestDecodeBodyRequiresJSON(t *testing.T) {
	tests := []struct {
		contentType string
		want        int
	}{
		{"application/json", http.StatusOK},
		{"application/json; charset=utf-8", http.StatusOK},
		{"Application/JSON", http.StatusOK},
		{"text/plain", http.StatusUnsupportedMediaType},
		{"text/plain;charset=UTF-8", http.StatusUnsupportedMediaType},
		{"application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
		{"application/jsonx", http.StatusUnsupportedMediaType},
		{"", http.StatusUnsupportedMediaType},
	}
	for _, tt := range tests {
		t.Run(tt.contentType, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"b"}`))
			if tt.contentType != "" {
				r.Header.Set("Content-Type", tt.contentType)
			}
			w := httptest.NewRecorder()
			var dst struct {
				A string `json:"a"`
			}
			ok := decodeBody(w, r, &dst)
			if tt.want == http.StatusOK {
				if !ok || dst.A != "b" {
					t.Fatalf("decodeBody = %v, A = %q; want a decoded body", ok, dst.A)
				}
				return
			}
			if ok || w.Code != tt.want {
				t.Errorf("decodeBody = %v, status %d; want refusal with %d", ok, w.Code, tt.want)
			}
			if dst.A != "" {
				t.Errorf("a refused body must not be decoded, got A = %q", dst.A)
			}
		})
	}
}

func TestLocalHostAllowed(t *testing.T) {
	tests := []struct {
		host, public string // public is the configured public host name
		want         bool
	}{
		{"localhost", "", true},
		{"localhost:8080", "", true},
		{"LOCALHOST:8080", "", true},
		{"app.localhost:8080", "", true},
		{"127.0.0.1:8080", "", true},
		{"127.0.0.2", "", true},
		{"[::1]:8080", "", true},
		{"[::1]", "", true},
		{"kiln.example.com", "kiln.example.com", true},
		{"kiln.example.com:8443", "kiln.example.com", true},
		// Rebinding: the page's own name resolves to 127.0.0.1 but the
		// Host header still carries that name.
		{"attacker.example", "", false},
		{"attacker.example:8080", "kiln.example.com", false},
		{"localhost.attacker.example", "", false},
		{"127.0.0.1.attacker.example", "", false},
		{"10.0.0.5:8080", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := localHostAllowed(tt.host, tt.public); got != tt.want {
			t.Errorf("localHostAllowed(%q, public=%q) = %v, want %v", tt.host, tt.public, got, tt.want)
		}
	}
}

// TestRouterGuardsAuthDisabledServer exercises the wiring: with Auth nil the
// router refuses a foreign Host and any POST a form could have sent; with
// auth enabled the same requests pass through to the route (the 405 is the
// route table saying /healthz is GET-only, which is how we know the guard did
// not intercept).
func TestRouterGuardsAuthDisabledServer(t *testing.T) {
	open := (&Server{PublicURL: "https://kiln.example.com"}).Router()

	if w := do(open, http.MethodGet, "127.0.0.1:8080", "", nil); w.Code != http.StatusOK {
		t.Errorf("GET loopback = %d, want 200", w.Code)
	}
	if w := do(open, http.MethodGet, "kiln.example.com", "", nil); w.Code != http.StatusOK {
		t.Errorf("GET public host = %d, want 200", w.Code)
	}
	if w := do(open, http.MethodGet, "attacker.example:8080", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("GET rebound host = %d, want 403", w.Code)
	}
	if w := do(open, http.MethodPost, "127.0.0.1:8080", "text/plain", nil); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("POST text/plain = %d, want 415", w.Code)
	}
	if w := do(open, http.MethodPost, "127.0.0.1:8080", "", nil); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("POST with no Content-Type = %d, want 415", w.Code)
	}
	// A form's multipart body is also a simple request, so on its own the
	// content type is not enough; the UI's custom header is.
	if w := do(open, http.MethodPost, "127.0.0.1:8080", "multipart/form-data; boundary=x", nil); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("POST multipart without the client header = %d, want 415", w.Code)
	}
	hdr := map[string]string{"X-Requested-With": "kiln-ui"}
	if w := do(open, http.MethodPost, "127.0.0.1:8080", "multipart/form-data; boundary=x", hdr); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST multipart with the client header = %d, want it to reach the router (405)", w.Code)
	}
	if w := do(open, http.MethodPost, "127.0.0.1:8080", "application/json; charset=utf-8", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST json = %d, want it to reach the router (405)", w.Code)
	}
	// PUT/PATCH/DELETE cannot be sent without a preflight, so body-less UI
	// calls such as DELETE keep working without a Content-Type.
	if w := do(open, http.MethodDelete, "127.0.0.1:8080", "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE with no Content-Type = %d, want it to reach the router (405)", w.Code)
	}

	// With authentication on, the guard is not installed: the Host check is
	// about ambient trust in loopback, which a token-protected server lacks.
	guarded := (&Server{Auth: &auth.Middleware{}}).Router()
	if w := do(guarded, http.MethodGet, "attacker.example", "", nil); w.Code != http.StatusOK {
		t.Errorf("auth-enabled GET with foreign host = %d, want 200", w.Code)
	}
	if w := do(guarded, http.MethodPost, "attacker.example", "text/plain", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("auth-enabled POST text/plain = %d, want it to reach the router (405)", w.Code)
	}
}

// The GitHub webhook is called by GitHub through whatever hostname the tunnel
// has and authenticates by HMAC; the guard must leave it alone.
func TestLocalGuardSkipsWebhook(t *testing.T) {
	s := &Server{}
	h := s.localGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	r := httptest.NewRequest(http.MethodPost, "http://abc.tunnel.example/hooks/github", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("webhook = %d, want it passed through", w.Code)
	}
}
