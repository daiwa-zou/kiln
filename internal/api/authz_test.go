package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// mutableSource authenticates the fixed token "kiln_valid" as whatever
// identity the test currently wants, so one server can play several callers.
type mutableSource struct{ id auth.Identity }

func (m *mutableSource) IdentityForToken(_ context.Context, hash string) (auth.Identity, error) {
	if hash == auth.HashToken("kiln_valid") {
		return m.id, nil
	}
	return auth.Identity{}, auth.ErrUnauthenticated
}

// authedServer is testServer with the auth middleware actually wired. The
// plain harness leaves Auth nil, which makes every caller an admin and every
// authorization branch dead code; this is the door into those branches.
func authedServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, string, *mutableSource) {
	t.Helper()

	dsn := os.Getenv("KILN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KILN_TEST_DATABASE_URL not set; skipping authz integration test")
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
	wsID, err := js.EnsureWorkspace(ctx, "test-org", "demo", "Demo")
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	src := &mutableSource{}
	keyring, err := crypto.NewKeyring(strings.Repeat("ab", 32)) // 64 hex chars
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	srv := httptest.NewServer((&Server{
		Store: js, Writes: js, Runs: js, Admin: js, Members: js, Keyring: keyring,
		DB:   &store.DB{Pool: pool},
		Auth: &auth.Middleware{Source: src},
	}).Router())
	t.Cleanup(srv.Close)
	return srv, pool, wsID, src
}

// addMember creates a user and joins them to the workspace's org with a role
// (or leaves them org-less when role is empty), returning the user id.
func addMember(t *testing.T, pool *pgxpool.Pool, wsID, login, role string) string {
	t.Helper()
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (login) VALUES ($1) RETURNING id`, login).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if role != "" {
		if _, err := pool.Exec(ctx, `
			INSERT INTO org_members (org_id, user_id, role)
			SELECT ws.org_id, $2, $3 FROM workspaces ws WHERE ws.id = $1`,
			wsID, userID, role); err != nil {
			t.Fatal(err)
		}
	}
	return userID
}

func TestWriteAuthorizationByRole(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)

	viewer := addMember(t, pool, wsID, "vera", "viewer")
	member := addMember(t, pool, wsID, "mira", "member")
	outsider := addMember(t, pool, wsID, "otto", "") // a user, but not in the org

	body := map[string]string{"body": "Purpose text."}
	put := func() int {
		return send(t, srv, http.MethodPut, "/api/v1/workspaces/demo/steering/purpose", "kiln_valid", body, nil)
	}

	// A viewer can read but must not write.
	src.id = auth.Identity{UserID: viewer, Scopes: []string{"read", "write"}}
	if code := put(); code != http.StatusForbidden {
		t.Errorf("viewer write = %d, want 403", code)
	}

	// The historical default role writes: locking existing deployments out of
	// the feature they upgraded for would be worse.
	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}
	if code := put(); code != http.StatusOK {
		t.Errorf("member write = %d, want 200", code)
	}

	// A non-member cannot even see the workspace: 404, not 403, so slugs
	// cannot be probed across org boundaries.
	src.id = auth.Identity{UserID: outsider, Scopes: []string{"read", "write"}}
	if code := put(); code != http.StatusNotFound {
		t.Errorf("outsider write = %d, want 404 (workspace invisible)", code)
	}

	// An admin writes without any membership row at all.
	src.id = auth.Identity{UserID: outsider, Admin: true, Scopes: []string{"read", "write"}}
	if code := put(); code != http.StatusOK {
		t.Errorf("admin write = %d, want 200", code)
	}
}

func TestWorkspaceVisibilityScoping(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	ctx := context.Background()

	// A second org+workspace the test user does not belong to.
	if _, err := store.NewWikiStore(pool).EnsureWorkspace(ctx, "other-org", "other", "Other"); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	member := addMember(t, pool, wsID, "mira", "member")

	var list []WorkspaceSummary
	src.id = auth.Identity{UserID: member, Scopes: []string{"read"}}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces", "kiln_valid", nil, &list); code != http.StatusOK {
		t.Fatalf("GET workspaces = %d", code)
	}
	if len(list) != 1 || list[0].Slug != "demo" {
		t.Errorf("member sees %+v, want exactly [demo]", list)
	}

	src.id = auth.Identity{UserID: member, Admin: true, Scopes: []string{"read"}}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces", "kiln_valid", nil, &list); code != http.StatusOK {
		t.Fatalf("GET workspaces admin = %d", code)
	}
	if len(list) != 2 {
		t.Errorf("admin sees %d workspaces, want 2", len(list))
	}
}

func TestScopeFailsClosedWithoutIdentity(t *testing.T) {
	// scope() with auth enabled but no identity in context must report
	// nothing visible -- the defensive branch behind the middleware.
	s := &Server{Auth: &auth.Middleware{}}
	uid, admin := s.scope(httptest.NewRequest(http.MethodGet, "/", nil))
	if uid != "" || admin {
		t.Errorf("scope without identity = (%q, %v), want fail-closed", uid, admin)
	}
}

func TestConditionalRequestsAndPaginationClamp(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/workspaces/demo/pages", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	tag := res.Header.Get("ETag")
	if tag == "" {
		t.Fatal("pages response carries no ETag")
	}

	// The revision-based tag must produce a 304 on revisit...
	req.Header.Set("If-None-Match", tag)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotModified {
		t.Errorf("matching If-None-Match = %d, want 304", res.StatusCode)
	}

	// ...and an import bumps the revision, busting the tag.
	if err := js.Import(context.Background(), jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{{
			Path: "concepts/cache.md", Slug: "cache",
			Meta: wiki.Frontmatter{Type: wiki.TypeConcept, Title: "Cache", Updated: "2026-07-26"},
			Body: "# Cache\n\nBody.\n",
		}},
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("stale If-None-Match after import = %d, want 200", res.StatusCode)
	}

	// The limit ceiling clamps rather than erroring.
	if code := get(t, srv, "/api/v1/workspaces/demo/pages?limit=99999&offset=1", nil); code != http.StatusOK {
		t.Errorf("oversized limit = %d, want 200 (clamped)", code)
	}
}

func TestCORSAllowlistIsExactMatch(t *testing.T) {
	srv := httptest.NewServer((&Server{CORSOrigins: []string{"https://app.example"}}).Router())
	defer srv.Close()

	// acao returns the Access-Control-Allow-Origin echoed for an Origin.
	acao := func(origin string) string {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.Header.Get("Access-Control-Allow-Origin")
	}

	if got := acao("https://app.example"); got != "https://app.example" {
		t.Errorf("allowlisted origin got ACAO %q", got)
	}
	// Reflecting arbitrary origins would undo the point of an allowlist.
	if got := acao("https://evil.example"); got != "" {
		t.Errorf("unlisted origin got ACAO %q, want none", got)
	}

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/api/v1/workspaces", nil)
	req.Header.Set("Origin", "https://app.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("preflight = %d, want 204", res.StatusCode)
	}
}

func TestReadyAnswers503WithoutDatabase(t *testing.T) {
	srv := httptest.NewServer((&Server{}).Router())
	defer srv.Close()

	if code := get(t, srv, "/readyz", nil); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz without DB = %d, want 503", code)
	}
}

// errStore fails every read, to prove handler errors do not leak detail.
type errStore struct{}

var errLeaky = errors.New("pg: connection to server at 10.0.0.5 failed")

func (errStore) ListWorkspaces(context.Context, string, bool) ([]store.WorkspaceRow, error) {
	return nil, errLeaky
}
func (errStore) ResolveWorkspace(context.Context, string, string, bool) (store.WorkspaceRow, error) {
	return store.WorkspaceRow{}, errLeaky
}
func (errStore) Graph(context.Context, string, int) ([]store.GraphNode, []store.GraphEdge, int, error) {
	return nil, nil, 0, errLeaky
}
func (errStore) LoadPageSummaries(context.Context, string, int, int) ([]store.PageInfo, error) {
	return nil, errLeaky
}
func (errStore) LoadPage(context.Context, string, string) (wiki.Page, error) {
	return wiki.Page{}, errLeaky
}
func (errStore) Search(context.Context, string, string, int, int) ([]store.SearchHit, error) {
	return nil, errLeaky
}
func (errStore) Gaps(context.Context, string, int, int) ([]store.Gap, error) { return nil, errLeaky }
func (errStore) LoadArtifact(context.Context, string, string) (string, error) {
	return "", errLeaky
}

func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	srv := httptest.NewServer((&Server{Store: errStore{}}).Router())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/v1/workspaces")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	buf := make([]byte, 4096)
	n, _ := res.Body.Read(buf)
	body := string(buf[:n])
	if body == "" || strings.Contains(body, "10.0.0.5") {
		t.Errorf("error body %q leaks connection detail", body)
	}
}
