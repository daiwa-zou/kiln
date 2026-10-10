package store

import (
	"context"
	"strings"
	"testing"
)

// TestHotQueriesUseTheirIndexes seeds enough rows for the planner to care
// which access path it takes and checks that the lookups behind figure-blob
// cleanup and deletion-review handling use the indexes added for them rather
// than scanning the table. Each plan is logged, so `go test -v` shows what the
// database actually chose.
func TestHotQueriesUseTheirIndexes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	// Many tenants, each with plenty of figures and mostly-resolved review
	// history: the shape in which a lookup that is not scoped to one tenant, or
	// that cannot see past resolved rows, pays for everyone else's data.
	if _, err := pool.Exec(ctx, `
		WITH o AS (INSERT INTO orgs (name, slug) VALUES ('Perf','perf') RETURNING id)
		INSERT INTO workspaces (org_id, name, slug)
		SELECT o.id, 'ws' || n, 'ws' || n FROM o, generate_series(1, 20) n`); err != nil {
		t.Fatalf("seed workspaces: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO figures (workspace_id, source_key, ref, blob_key, content_type, width, height, size_bytes, sha256)
		SELECT w.id, 'doc' || (n % 500), 'r' || n, 'blob/' || w.id || '/' || md5(n::text),
		       'image/png', 10, 10, 100, md5(n::text)
		FROM workspaces w, generate_series(1, 2500) n`); err != nil {
		t.Fatalf("seed figures: %v", err)
	}
	// 2,500 deletion reviews per workspace, nearly all resolved, with a few
	// still live.
	if _, err := pool.Exec(ctx, `
		INSERT INTO review_items (workspace_id, kind, title, status)
		SELECT w.id, 'deletion', 'src/file' || n || '.md',
		       CASE WHEN n % 100 = 0 THEN 'approved' WHEN n % 100 = 1 THEN 'open' ELSE 'resolved' END
		FROM workspaces w, generate_series(1, 2500) n`); err != nil {
		t.Fatalf("seed review items: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE figures; ANALYZE review_items`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var ws, blobKey string
	if err := pool.QueryRow(ctx, `SELECT workspace_id, blob_key FROM figures LIMIT 1`).Scan(&ws, &blobKey); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		sql   string
		args  []any
		index string
		table string
	}{
		{
			name: "unreferencedBlobs",
			sql: `SELECT c FROM unnest($1::text[]) AS c
			      WHERE NOT EXISTS (SELECT 1 FROM figures f WHERE f.blob_key = c)`,
			args:  []any{[]string{blobKey, "blob/missing"}},
			index: "figures_blob_key_idx",
			table: "figures",
		},
		{
			name: "LoadApprovedDeletions",
			sql: `SELECT title FROM review_items
			      WHERE workspace_id = $1 AND kind = 'deletion' AND status = 'approved'
			      ORDER BY title`,
			args:  []any{ws},
			index: "review_items_live_title_idx",
			table: "review_items",
		},
		{
			name: "EnsureDeletionReviews",
			sql: `INSERT INTO review_items (workspace_id, kind, title, detail, actions)
			      SELECT $1, 'deletion', $2, $3, '["approve","keep"]'::jsonb
			      WHERE NOT EXISTS (
			          SELECT 1 FROM review_items
			          WHERE workspace_id = $1 AND kind = 'deletion' AND title = $2
			            AND status IN ('open','approved'))`,
			args:  []any{ws, "src/file100.md", "gone"},
			index: "review_items_live_title_idx",
			table: "review_items",
		},
		{
			name: "closeDeletionReviews",
			sql: `UPDATE review_items
			      SET status = 'resolved', resolved_at = now()
			      WHERE workspace_id = $1 AND kind = 'deletion'
			        AND title = ANY($2) AND status IN ('open', 'approved')`,
			args:  []any{ws, []string{"src/file100.md", "src/file200.md"}},
			index: "review_items_live_title_idx",
			table: "review_items",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A transaction, rolled back, so the INSERT and UPDATE cases are
			// planned (EXPLAIN without ANALYZE does not run them) and nothing
			// is left behind either way.
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			// Plan with the actual values, as a custom plan would be, rather
			// than the generic plan a cached statement might settle on.
			if _, err := tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`); err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query(ctx, "EXPLAIN "+tc.sql, tc.args...)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, line)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			text := strings.Join(plan, "\n")
			t.Logf("plan:\n%s", text)

			if !strings.Contains(text, tc.index) {
				t.Errorf("plan does not use %s", tc.index)
			}
			if strings.Contains(text, "Seq Scan on "+tc.table) {
				t.Errorf("plan sequentially scans %s", tc.table)
			}
		})
	}
}
