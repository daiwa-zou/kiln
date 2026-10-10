package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// linkedPage builds an entity page whose body links to each target slug.
func linkedPage(slug string, targets ...string) wiki.Page {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", slug)
	for _, t := range targets {
		fmt.Fprintf(&b, "See [[%s]].\n", t)
	}
	p := wiki.Page{Path: "entities/" + slug + ".md", Slug: slug, Body: b.String()}
	p.Meta.Type = "entity"
	p.Meta.Title = slug
	p.Meta.Created, p.Meta.Updated = "2026-07-27", "2026-07-27"
	return p
}

// ringPages returns n pages, page i linking to the next fanout pages around a
// ring, so every link resolves and every page has fanout inbound links too.
func ringPages(n, fanout int, extra ...string) []wiki.Page {
	slug := func(i int) string { return fmt.Sprintf("p%04d", i%n) }
	pages := make([]wiki.Page, 0, n)
	for i := 0; i < n; i++ {
		targets := make([]string, 0, fanout+len(extra))
		for k := 1; k <= fanout; k++ {
			targets = append(targets, slug(i+k))
		}
		if i == 0 {
			targets = append(targets, extra...)
		}
		pages = append(pages, linkedPage(slug(i), targets...))
	}
	return pages
}

// TestImportRewritesOnlyLinksWhoseResolutionChanged guards resolveLinks against
// touching every link row on every import. Postgres writes a new tuple version
// for each row an UPDATE matches whether or not a column differs, so the cost
// of an import must track what changed, not the size of the wiki.
func TestImportRewritesOnlyLinksWhoseResolutionChanged(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	var gotResolved, gotUnresolved int64
	s.onLinkResolve = func(resolved, unresolved int64) {
		gotResolved, gotUnresolved = resolved, unresolved
	}
	imp := func(req jobs.ImportRequest) {
		t.Helper()
		req.WorkspaceID = ws
		gotResolved, gotUnresolved = -1, -1
		if err := s.Import(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	countLinks := func(where string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM page_links WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	const pages, fanout = 200, 5 // 1,000 ring links, plus one dangling link
	imp(jobs.ImportRequest{RunID: "r1", UpsertPages: ringPages(pages, fanout, "late")})
	if gotResolved != pages*fanout {
		t.Fatalf("first import resolved %d links, want %d", gotResolved, pages*fanout)
	}
	if total, resolved := countLinks("true"), countLinks("resolved AND to_page_id IS NOT NULL"); total != pages*fanout+1 || resolved != pages*fanout {
		t.Fatalf("after first import: %d links, %d resolved; want %d and %d", total, resolved, pages*fanout+1, pages*fanout)
	}

	// Re-importing one unchanged page replaces its own outbound links (they come
	// back unresolved and must be re-resolved), but the other ~1,000 rows have
	// nothing to change.
	one := ringPages(pages, fanout, "late")[50]
	imp(jobs.ImportRequest{RunID: "r2", UpsertPages: []wiki.Page{one}})
	if gotResolved != fanout || gotUnresolved != 0 {
		t.Errorf("one-page reimport rewrote resolved=%d unresolved=%d links, want %d and 0",
			gotResolved, gotUnresolved, fanout)
	}

	// An import that changes nothing about resolution rewrites nothing.
	imp(jobs.ImportRequest{RunID: "r3"})
	if gotResolved != 0 || gotUnresolved != 0 {
		t.Errorf("no-op import rewrote resolved=%d unresolved=%d links, want 0 and 0", gotResolved, gotUnresolved)
	}

	// Creating the page a link was waiting for resolves exactly that link
	// (page 0's dangling "late"), plus the new page's own outbound link.
	imp(jobs.ImportRequest{RunID: "r4", UpsertPages: []wiki.Page{linkedPage("late", "p0001")}})
	if gotResolved != 2 || gotUnresolved != 0 {
		t.Errorf("late page rewrote resolved=%d unresolved=%d links, want 2 and 0", gotResolved, gotUnresolved)
	}

	// Deleting a page unresolves exactly its inbound links and re-resolves none.
	imp(jobs.ImportRequest{RunID: "r5", SoftDeletePages: []string{"entities/p0100.md"}})
	if gotResolved != 0 || gotUnresolved != fanout {
		t.Errorf("deleting a page rewrote resolved=%d unresolved=%d links, want 0 and %d",
			gotResolved, gotUnresolved, fanout)
	}

	// End state is what the old, rewrite-everything statement produced.
	if n := countLinks("NOT resolved AND to_page_id IS NOT NULL"); n != 0 {
		t.Errorf("%d unresolved links still carry a target", n)
	}
	if n := countLinks("resolved AND to_page_id IS NULL"); n != 0 {
		t.Errorf("%d resolved links have no target", n)
	}
	// The deleted page's own outbound rows remain and still point at live
	// pages, so only its five inbound links are unresolved.
	if n := countLinks("NOT resolved"); n != fanout {
		t.Errorf("%d unresolved links, want %d (the inbound links of the deleted page)", n, fanout)
	}
}
