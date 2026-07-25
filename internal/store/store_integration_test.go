package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests run against a real Postgres, addressed by KILN_TEST_DATABASE_URL.
// They are skipped when it is unset so `go test ./...` stays hermetic:
//
//	docker run -d --name kiln-test -e POSTGRES_PASSWORD=kiln -e POSTGRES_USER=kiln \
//	  -e POSTGRES_DB=kiln -p 55432:5432 postgres:16-alpine
//	KILN_TEST_DATABASE_URL="postgres://kiln:kiln@localhost:55432/kiln?sslmode=disable" go test ./internal/store/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("KILN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("KILN_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// freshSchema drops and rebuilds public, so each test starts from empty.
func freshSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func TestMigrateAppliesFromEmpty(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	got, err := CurrentVersion(ctx, pool)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if got != SchemaVersion {
		t.Errorf("schema version = %d, want %d", got, SchemaVersion)
	}
	if err := CheckSchemaVersion(ctx, pool); err != nil {
		t.Errorf("CheckSchemaVersion: %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	applied, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second Migrate applied %v, want nothing", applied)
	}
}

func TestMigrateConcurrentlyIsSafe(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	// Reset without migrating, then race several migrations. The advisory lock
	// must serialize them; without it the concurrent CREATE TABLEs conflict.
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}

	const racers = 4
	errs := make(chan error, racers)
	for range racers {
		go func() {
			_, err := Migrate(ctx, pool)
			errs <- err
		}()
	}
	for range racers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != SchemaVersion {
		t.Errorf("schema_migrations has %d rows, want %d; a migration was applied twice", count, SchemaVersion)
	}
}

// seedWorkspace creates the org/workspace/wiki chain and returns the wiki id.
func seedWorkspace(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()

	var orgID, wsID, wikiID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (name, slug) VALUES ('Test','test') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (org_id, name, slug) VALUES ($1,'WS','ws') RETURNING id`,
		orgID).Scan(&wsID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO wikis (workspace_id) VALUES ($1) RETURNING id`, wsID).Scan(&wikiID); err != nil {
		t.Fatalf("insert wiki: %v", err)
	}
	return wikiID
}

func insertPage(t *testing.T, pool *pgxpool.Pool, wikiID, path, pageType, slug, title, body string) error {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO pages (wiki_id, path, type, slug, title, body, content_hash)
		 VALUES ($1,$2,$3,$4,$5,$6,'h')`,
		wikiID, path, pageType, slug, title, body)
	return err
}

func TestSlugUniquenessAppliesOnlyToLivePages(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	wikiID := seedWorkspace(t, pool)

	if err := insertPage(t, pool, wikiID, "entities/ripple.md", "entity", "ripple", "Ripple", "Body text."); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// A second live page with the same slug must be rejected.
	if err := insertPage(t, pool, wikiID, "entities/ripple.md", "entity", "ripple", "Dup", "x"); err == nil {
		t.Error("duplicate live slug was accepted")
	}

	// After a soft delete the slug is free again, so a regenerated page is not
	// blocked by its own tombstone.
	if _, err := pool.Exec(ctx, `UPDATE pages SET deleted_at = now() WHERE slug = 'ripple'`); err != nil {
		t.Fatal(err)
	}
	if err := insertPage(t, pool, wikiID, "entities/ripple.md", "entity", "ripple", "Ripple v2", "Rebuilt."); err != nil {
		t.Errorf("recreating a soft-deleted slug failed: %v", err)
	}
}

func TestPageTypeIsConstrained(t *testing.T) {
	pool := testPool(t)
	freshSchema(t, pool)
	wikiID := seedWorkspace(t, pool)

	if err := insertPage(t, pool, wikiID, "entities/x.md", "nonsense", "x", "X", "y"); err == nil {
		t.Error("an unknown page type was accepted")
	}

	for _, valid := range []string{"entity", "concept", "source", "query", "comparison", "synthesis", "overview"} {
		if err := insertPage(t, pool, wikiID, valid+"/p.md", valid, valid+"-p", "P", "body"); err != nil {
			t.Errorf("valid type %q was rejected: %v", valid, err)
		}
	}
}

func TestFullTextSearch(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	wikiID := seedWorkspace(t, pool)

	if err := insertPage(t, pool, wikiID, "entities/ripple.md", "entity", "ripple", "Ripple",
		"Ripple dispatches tasks to workers."); err != nil {
		t.Fatal(err)
	}
	if err := insertPage(t, pool, wikiID, "concepts/dispatch.md", "concept", "dispatch", "Dispatch",
		"Unrelated prose."); err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx,
		`SELECT slug FROM pages
		 WHERE deleted_at IS NULL AND search @@ plainto_tsquery('english', 'dispatch')
		 ORDER BY ts_rank(search, plainto_tsquery('english','dispatch')) DESC`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			t.Fatal(err)
		}
		got = append(got, slug)
	}

	if len(got) != 2 {
		t.Fatalf("search returned %v, want both pages", got)
	}
	// Title carries weight A and body weight B, so a title match must rank first.
	if got[0] != "dispatch" {
		t.Errorf("ranking = %v; a title match should outrank a body match", got)
	}
}

func TestFullTextSearchExcludesDeleted(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	wikiID := seedWorkspace(t, pool)

	if err := insertPage(t, pool, wikiID, "entities/ripple.md", "entity", "ripple", "Ripple",
		"Ripple dispatches tasks."); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE pages SET deleted_at = now()`); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pages
		 WHERE deleted_at IS NULL AND search @@ plainto_tsquery('english','dispatch')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("soft-deleted page still appears in search results (%d)", n)
	}
}

func TestUnresolvedLinksAreQueryable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	wikiID := seedWorkspace(t, pool)

	if err := insertPage(t, pool, wikiID, "concepts/dispatch.md", "concept", "dispatch", "Dispatch", "Body."); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO page_links (from_page_id, to_slug, resolved)
		 SELECT id, 'never-written', false FROM pages WHERE slug='dispatch'`); err != nil {
		t.Fatal(err)
	}

	// Unresolved links are the cheapest gap signal available: the wiki has
	// declared it wants a page that does not exist.
	var slug string
	if err := pool.QueryRow(ctx, `SELECT to_slug FROM page_links WHERE NOT resolved`).Scan(&slug); err != nil {
		t.Fatalf("query unresolved links: %v", err)
	}
	if slug != "never-written" {
		t.Errorf("to_slug = %q", slug)
	}
}

func TestWorkspaceDeletionCascades(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	wikiID := seedWorkspace(t, pool)

	if err := insertPage(t, pool, wikiID, "entities/ripple.md", "entity", "ripple", "Ripple", "Body."); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspaces`); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d pages survived their workspace", n)
	}
}
