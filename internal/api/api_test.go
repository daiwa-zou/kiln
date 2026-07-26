package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// testServer builds a server over a freshly migrated database with one
// workspace. Skips when no database is configured, so the default suite stays
// hermetic.
// The returned string is the workspace ID; the API resolves the "demo" slug
// to it, which the slug-vs-UUID test relies on.
func testServer(t *testing.T) (*httptest.Server, *store.WikiStore, string) {
	t.Helper()

	dsn := os.Getenv("KILN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KILN_TEST_DATABASE_URL not set; skipping API integration test")
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

	srv := httptest.NewServer((&Server{Store: js, Writes: js, DB: &store.DB{Pool: pool}}).Router())
	t.Cleanup(srv.Close)
	return srv, js, wsID
}

func get(t *testing.T, srv *httptest.Server, path string, into any) int {
	t.Helper()

	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()

	if into != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return res.StatusCode
}

func seed(t *testing.T, js *store.WikiStore, ws string) {
	t.Helper()

	pages := []wiki.Page{
		{
			Path: "entities/ripple.md", Slug: "ripple",
			Meta: wiki.Frontmatter{
				Type: wiki.TypeEntity, Title: "Ripple", Updated: "2026-07-25",
				BuiltAtRef: "abc1234", Tags: []string{"go"},
			},
			Body: "# Ripple\n\nDispatches tasks and links to [[never-written]].\n",
		},
		{
			Path: "synthesis/architecture.md", Slug: "architecture",
			Meta: wiki.Frontmatter{Type: wiki.TypeSynthesis, Title: "Architecture", Updated: "2026-07-25"},
			Body: "# Architecture\n\nSee [[ripple]] for dispatch.\n",
		},
	}

	if err := js.Import(context.Background(), jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: pages,
		Index:       "# Wiki Index\n\n## Entities\n- [[entities/ripple|Ripple]]\n",
		Overview:    "---\ntype: overview\n---\n\n# Overview\n\nA demo wiki.\n",
		LogEntry:    "## [2026-07-25] build | demo @ abc1234\n",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
}

func TestHealthAnswersWithoutTheDatabase(t *testing.T) {
	// Liveness must not depend on Postgres: a failing readiness check should
	// not make an orchestrator kill a process that is merely waiting on it.
	srv := httptest.NewServer((&Server{}).Router())
	defer srv.Close()

	if code := get(t, srv, "/healthz", nil); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 with no database wired", code)
	}
}

type stubSource struct{ id auth.Identity }

func (s *stubSource) IdentityForToken(_ context.Context, hash string) (auth.Identity, error) {
	if hash == auth.HashToken("kiln_valid") {
		return s.id, nil
	}
	return auth.Identity{}, auth.ErrUnauthenticated
}

func TestAuthGuardsAPIRoutes(t *testing.T) {
	// Hermetic: rejection happens in the middleware, before any handler or
	// database is touched, so no Postgres is needed to prove the perimeter.
	srv := httptest.NewServer((&Server{
		Auth: &auth.Middleware{Source: &stubSource{id: auth.Identity{UserID: "u1", Scopes: []string{"read"}}}},
	}).Router())
	defer srv.Close()

	for _, path := range []string{
		"/api/v1/workspaces",
		"/api/v1/workspaces/demo/pages",
		"/api/v1/workspaces/demo/search?q=x",
	} {
		if code := get(t, srv, path, nil); code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, code)
		}
	}

	// The pre-auth allowlist is exactly liveness, readiness-shape, and version.
	if code := get(t, srv, "/api/v1/version", nil); code != http.StatusOK {
		t.Errorf("/api/v1/version = %d, want 200 without a token", code)
	}
	if code := get(t, srv, "/healthz", nil); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 without a token", code)
	}
}

func TestVersionReportsSchema(t *testing.T) {
	srv, _, _ := testServer(t)

	var body struct {
		Version       string `json:"version"`
		SchemaVersion int    `json:"schemaVersion"`
	}
	if code := get(t, srv, "/api/v1/version", &body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	// The UI compares this against its own build, so a skewed pair is visible.
	if body.SchemaVersion != store.SchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", body.SchemaVersion, store.SchemaVersion)
	}
}

func TestPagesAndPageLookup(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	var list []PageSummary
	if code := get(t, srv, "/api/v1/workspaces/demo/pages", &list); code != http.StatusOK {
		t.Fatalf("pages status = %d", code)
	}
	if len(list) != 2 {
		t.Fatalf("got %d pages, want 2", len(list))
	}

	// A page is addressable by slug and by path: a wikilink carries a slug, the
	// tree carries a path, and both have to resolve.
	for _, ref := range []string{"ripple", "entities/ripple.md"} {
		var page map[string]any
		if code := get(t, srv, "/api/v1/workspaces/demo/pages/"+ref, &page); code != http.StatusOK {
			t.Fatalf("GET page by %q = %d", ref, code)
		}
		if page["title"] != "Ripple" {
			t.Errorf("title = %v for ref %q", page["title"], ref)
		}
	}

	if code := get(t, srv, "/api/v1/workspaces/demo/pages/nonexistent", nil); code != http.StatusNotFound {
		t.Errorf("missing page = %d, want 404", code)
	}
}

func TestArtifactEndpoints(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	for kind, want := range map[string]string{
		"index":    "## Entities",
		"overview": "A demo wiki",
		"log":      "build | demo",
	} {
		var body struct{ Body string }
		if code := get(t, srv, "/api/v1/workspaces/demo/"+kind, &body); code != http.StatusOK {
			t.Fatalf("%s status = %d", kind, code)
		}
		if !strings.Contains(body.Body, want) {
			t.Errorf("%s body missing %q:\n%s", kind, want, body.Body)
		}
	}
}

func TestSearch(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	var hits []SearchHit
	if code := get(t, srv, "/api/v1/workspaces/demo/search?q=dispatch", &hits); code != http.StatusOK {
		t.Fatalf("search status = %d", code)
	}
	if len(hits) == 0 {
		t.Fatal("search found nothing for a term present in a page body")
	}

	// An empty query returns an empty list rather than everything, so a stray
	// keystroke does not dump the whole wiki.
	var empty []SearchHit
	get(t, srv, "/api/v1/workspaces/demo/search?q=", &empty)
	if len(empty) != 0 {
		t.Errorf("empty query returned %d hits", len(empty))
	}
}

func TestGapsListsUnresolvedLinks(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	var gaps []map[string]any
	if code := get(t, srv, "/api/v1/workspaces/demo/gaps", &gaps); code != http.StatusOK {
		t.Fatalf("gaps status = %d", code)
	}

	// The seeded wiki links to a page that does not exist. That is the gap
	// signal, and it should surface without any extra machinery.
	var found bool
	for _, g := range gaps {
		if g["slug"] == "never-written" {
			found = true
		}
	}
	if !found {
		t.Errorf("gaps = %+v, want the unresolved link", gaps)
	}
}

func TestUnknownWorkspaceIs404(t *testing.T) {
	srv, _, _ := testServer(t)

	if code := get(t, srv, "/api/v1/workspaces/nonexistent/pages", nil); code != http.StatusNotFound {
		t.Errorf("unknown workspace = %d, want 404", code)
	}
}

func TestUIIsServedAndDeepLinksWork(t *testing.T) {
	srv, _, _ := testServer(t)

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}

	// A deep link must render the app rather than 404, so reloading on a page
	// URL works.
	if code := get(t, srv, "/some/deep/link", nil); code != http.StatusOK {
		t.Errorf("deep link = %d, want the app", code)
	}
	// An unknown API path is still an API error, not the HTML app.
	if code := get(t, srv, "/api/v1/nonexistent", nil); code != http.StatusNotFound {
		t.Errorf("unknown API path = %d, want 404", code)
	}
}
