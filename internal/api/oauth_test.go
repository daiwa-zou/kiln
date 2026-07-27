package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/github"
	"github.com/daiwa-zou/kiln/internal/store"
)

// fakeGitHubServer serves the three endpoints sign-in touches.
func fakeGitHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_test"}`))
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":77,"login":"daiwa","email":"d@example.com","name":"Daiwa","avatar_url":"https://a.example/x.png"}`))
	})
	mux.HandleFunc("GET /user/installations", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"installations":[{"id":901,"account":{"login":"daiwa-zou","type":"User"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// decodeInto reads a response body as JSON.
func decodeInto(t *testing.T, res *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// oauthServer is a kiln API server with sign-in fully wired against a fake
// GitHub, sessions enabled on the auth middleware, and a browser-shaped
// client (cookie jar, no redirect following).
func oauthServer(t *testing.T) (*httptest.Server, *http.Client, *store.WikiStore) {
	t.Helper()

	dsn := os.Getenv("KILN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KILN_TEST_DATABASE_URL not set; skipping OAuth integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	js := store.NewWikiStore(pool)
	if _, err := js.EnsureWorkspace(ctx, "test-org", "demo", "Demo"); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	gh := fakeGitHubServer(t)

	source := &auth.PGSource{Pool: pool}
	srv := httptest.NewServer((&Server{
		Store: js, Writes: js, Runs: js, Admin: js,
		Users: js, SessionPool: pool, SessionTTL: time.Hour,
		GitHub: &github.Client{
			ClientID: "cid", ClientSecret: "shhh",
			BaseURL: gh.URL, APIBaseURL: gh.URL,
		},
		DB:   &store.DB{Pool: pool},
		Auth: &auth.Middleware{Source: source, Sessions: source},
	}).Router())
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return srv, client, js
}

// signIn walks the whole dance and leaves the session cookies in the jar.
func signIn(t *testing.T, srv *httptest.Server, client *http.Client) {
	t.Helper()

	res, err := client.Get(srv.URL + "/auth/github/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("login = %d, want 302", res.StatusCode)
	}
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("authorize redirect carries no state")
	}

	res, err = client.Get(srv.URL + "/auth/github/callback?code=x&state=" + state)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("callback = %d, want 302 to /", res.StatusCode)
	}
}

func csrfFrom(t *testing.T, client *http.Client, srv *httptest.Server) string {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == auth.CSRFCookie {
			return c.Value
		}
	}
	return ""
}

func TestOAuthRoundTrip(t *testing.T) {
	srv, client, js := oauthServer(t)
	signIn(t, srv, client)

	// The session cookie authenticates reads with no bearer token.
	res, err := client.Get(srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/me with session = %d, want 200", res.StatusCode)
	}
	var me struct {
		Login string `json:"login"`
		Admin bool   `json:"admin"`
	}
	decodeInto(t, res, &me)
	if me.Login != "daiwa" {
		t.Errorf("me = %+v", me)
	}
	// The first user a fresh deployment sees becomes the instance admin.
	if !me.Admin {
		t.Error("first user is not admin")
	}

	// Installations were mirrored during the callback.
	var n int
	if err := js.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM user_installations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("user_installations = %d, want 1", n)
	}
}

func TestOAuthStateMismatchRejected(t *testing.T) {
	srv, client, _ := oauthServer(t)

	// Start a login so a state cookie exists, then return a different state:
	// the forged callback must not mint a session.
	res, err := client.Get(srv.URL + "/auth/github/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	res, err = client.Get(srv.URL + "/auth/github/callback?code=x&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state = %d, want 400", res.StatusCode)
	}
	// No callback without any login at all, either.
	res2, err := client.Get(srv.URL + "/auth/github/callback?code=x&state=whatever")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("stateless callback = %d, want 400", res2.StatusCode)
	}
}

func TestSessionMutationsNeedCSRFEndToEnd(t *testing.T) {
	srv, client, _ := oauthServer(t)
	signIn(t, srv, client)

	body := strings.NewReader(`{"body":"Purpose set through a session."}`)
	target := srv.URL + "/api/v1/workspaces/demo/steering/purpose"

	// The M3 verification probe: cookie-authenticated mutation without the
	// CSRF header fails.
	req, _ := http.NewRequest(http.MethodPut, target, body)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation without CSRF = %d, want 403", res.StatusCode)
	}

	// With the double-submit header it goes through (the first user is
	// admin, so the role gate passes too).
	body = strings.NewReader(`{"body":"Purpose set through a session."}`)
	req, _ = http.NewRequest(http.MethodPut, target, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeader, csrfFrom(t, client, srv))
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("mutation with CSRF = %d, want 200", res.StatusCode)
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	srv, client, _ := oauthServer(t)
	signIn(t, srv, client)

	// Logout requires the CSRF pair even though it sits outside the wrap.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/logout", nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d, want 403", res.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/auth/logout", nil)
	req.Header.Set(auth.CSRFHeader, csrfFrom(t, client, srv))
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d", res.StatusCode)
	}

	// The server-side session is gone: even replaying the old cookie fails.
	res, err = client.Get(srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("/me after logout = %d, want 401", res.StatusCode)
	}
}

func TestRepeatSignInUpdatesNotDuplicates(t *testing.T) {
	srv, client, js := oauthServer(t)
	signIn(t, srv, client)
	signIn(t, srv, client)

	var users int
	if err := js.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE github_user_id = 77`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Errorf("users for gh id 77 = %d, want 1 (upsert, not insert)", users)
	}
}
