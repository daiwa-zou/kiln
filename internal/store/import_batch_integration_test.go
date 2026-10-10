package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// TestImportBatchesAFirstBuild imports 500 pages of 10 links each in one call,
// the shape of a first build. The import holds an advisory lock for its whole
// transaction, so the round trips it spends there are time every other writer
// in the workspace waits; the elapsed times are logged so a regression to one
// statement per page and per link is visible, and the rows are checked so
// batching is known to have kept them identical.
func TestImportBatchesAFirstBuild(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	const pages, fanout = 500, 10
	req := jobs.ImportRequest{WorkspaceID: ws, RunID: "build", UpsertPages: ringPages(pages, fanout)}

	start := time.Now()
	if err := s.Import(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Logf("first build, %d pages x %d links: %v", pages, fanout, time.Since(start))

	assertRing := func(label string) {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT f.slug, t.slug, l.resolved
			FROM page_links l
			JOIN pages f ON f.id = l.from_page_id
			LEFT JOIN pages t ON t.id = l.to_page_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]bool{}
		for rows.Next() {
			var from string
			var to *string
			var resolved bool
			if err := rows.Scan(&from, &to, &resolved); err != nil {
				t.Fatal(err)
			}
			if !resolved || to == nil {
				t.Fatalf("%s: link from %s is not resolved", label, from)
			}
			got[from+">"+*to] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != pages*fanout {
			t.Fatalf("%s: %d distinct links, want %d", label, len(got), pages*fanout)
		}
		for i := 0; i < pages; i++ {
			for k := 1; k <= fanout; k++ {
				edge := fmt.Sprintf("p%04d>p%04d", i, (i+k)%pages)
				if !got[edge] {
					t.Fatalf("%s: missing link %s", label, edge)
				}
			}
		}
	}
	assertRing("first build")

	// A rebuild replaces each page's links rather than adding to them.
	start = time.Now()
	req.RunID = "rebuild"
	if err := s.Import(ctx, req); err != nil {
		t.Fatal(err)
	}
	t.Logf("identical rebuild: %v", time.Since(start))
	assertRing("rebuild")
}

// TestImportLastPageWinsForARepeatedSlug pins the semantics batching must keep:
// two pages sharing a slug in one import collapse onto one row, and the later
// page's links replace the earlier page's rather than joining them.
func TestImportLastPageWinsForARepeatedSlug(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, RunID: "dup",
		UpsertPages: []wiki.Page{
			linkedPage("same", "first", "shared"),
			linkedPage("other", "same"),
			linkedPage("same", "second", "shared"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, `
		SELECT l.to_slug FROM page_links l JOIN pages p ON p.id = l.from_page_id
		WHERE p.slug = 'same' ORDER BY l.to_slug`)
	if err != nil {
		t.Fatal(err)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := "second,shared"; strings.Join(got, ",") != want {
		t.Errorf("links of the repeated slug = %v, want %s", got, want)
	}
}
