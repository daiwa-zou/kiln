package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// jobStore returns a store over a freshly migrated schema plus a workspace.
func jobStore(t *testing.T) (*WikiStore, string) {
	t.Helper()

	pool := testPool(t)
	freshSchema(t, pool)

	js := NewWikiStore(pool)
	wsID, err := js.EnsureWorkspace(context.Background(), "test-org", "test-ws", "Test Workspace")
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	return js, wsID
}

func page(path, slug, pageType, title, body string) wiki.Page {
	return wiki.Page{
		Path: path, Slug: slug,
		Meta: wiki.Frontmatter{
			Type: wiki.PageType(pageType), Title: title,
			Created: "2026-07-25", Updated: "2026-07-25",
		},
		Body: body,
	}
}

func TestEnsureWorkspaceIsIdempotent(t *testing.T) {
	js, first := jobStore(t)

	// Bootstrapping the same workspace twice must not create a duplicate, or a
	// repeated `kiln build` would fork the wiki.
	second, err := js.EnsureWorkspace(context.Background(), "test-org", "test-ws", "Test Workspace")
	if err != nil {
		t.Fatalf("second EnsureWorkspace: %v", err)
	}
	if first != second {
		t.Errorf("workspace IDs differ across calls: %s vs %s", first, second)
	}
}

func TestImportRoundTripsPages(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	p := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nDispatches tasks.\n")
	p.Meta.Tags = []string{"go", "service"}
	p.Meta.Sources = []string{"apps/ripple/go.mod"}

	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{p},
		UpsertSources: []diff.SourceRecord{{
			Key: diff.ModuleKey("ripple"), InputHash: "h1",
			FilesWritten: []string{"entities/ripple.md"},
		}},
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	pages, err := js.LoadPages(ctx, ws)
	if err != nil {
		t.Fatalf("LoadPages: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(pages))
	}

	got := pages[0]
	if got.Path != p.Path || got.Meta.Title != p.Meta.Title || got.Body != p.Body {
		t.Errorf("page did not round-trip: %+v", got)
	}
	// Frontmatter goes through jsonb, so a lost tag or source would mean the
	// encoding is dropping fields silently.
	if len(got.Meta.Tags) != 2 || len(got.Meta.Sources) != 1 {
		t.Errorf("frontmatter lost fields: tags=%v sources=%v", got.Meta.Tags, got.Meta.Sources)
	}

	sources, err := js.LoadSources(ctx, ws)
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(sources) != 1 || sources[0].InputHash != "h1" {
		t.Fatalf("sources = %+v", sources)
	}
	// files_written is what makes cascade deletion possible; losing it would
	// silently orphan every page the source produced.
	if len(sources[0].FilesWritten) != 1 {
		t.Errorf("FilesWritten = %v", sources[0].FilesWritten)
	}
}

func TestImportUpdatesRatherThanDuplicates(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	first := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nOriginal.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{first}}); err != nil {
		t.Fatalf("first Import: %v", err)
	}

	second := page("entities/ripple.md", "ripple", "entity", "Ripple v2", "# Ripple\n\nRewritten.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{second}}); err != nil {
		t.Fatalf("second Import: %v", err)
	}

	pages, err := js.LoadPages(ctx, ws)
	if err != nil {
		t.Fatalf("LoadPages: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("regeneration created %d pages; it should update in place", len(pages))
	}
	if pages[0].Meta.Title != "Ripple v2" {
		t.Errorf("title = %q, want the updated one", pages[0].Meta.Title)
	}
}

func TestImportSoftDeletesAndFreesTheSlug(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	p := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{p}}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, SoftDeletePages: []string{"entities/ripple.md"},
	}); err != nil {
		t.Fatalf("delete Import: %v", err)
	}

	pages, err := js.LoadPages(ctx, ws)
	if err != nil {
		t.Fatalf("LoadPages: %v", err)
	}
	if len(pages) != 0 {
		t.Fatalf("soft-deleted page still loads: %+v", pages)
	}

	// The tombstone must not block regenerating the same slug later.
	revived := page("entities/ripple.md", "ripple", "entity", "Ripple reborn", "# Ripple\n\nBack.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{revived}}); err != nil {
		t.Fatalf("re-import after soft delete: %v", err)
	}
	pages, _ = js.LoadPages(ctx, ws)
	if len(pages) != 1 || pages[0].Meta.Title != "Ripple reborn" {
		t.Errorf("pages after revival = %+v", pages)
	}
}

func TestImportResolvesLinksAndLeavesGapsUnresolved(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	linker := page("concepts/dispatch.md", "dispatch", "concept", "Dispatch",
		"# Dispatch\n\nHandled by [[ripple]] and also [[never-written]].\n")
	target := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n")

	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, UpsertPages: []wiki.Page{linker, target},
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	pool := js.pool
	var resolved, unresolved int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM page_links WHERE resolved`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM page_links WHERE NOT resolved`).Scan(&unresolved); err != nil {
		t.Fatal(err)
	}

	if resolved != 1 {
		t.Errorf("resolved links = %d, want 1", resolved)
	}
	// An unresolved link is kept deliberately: it is the cheapest gap signal
	// available, since the wiki has declared it wants a page that is missing.
	if unresolved != 1 {
		t.Errorf("unresolved links = %d, want 1 (the gap signal)", unresolved)
	}
}

func TestImportUnresolvesLinksToDeletedPages(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	linker := page("concepts/dispatch.md", "dispatch", "concept", "Dispatch",
		"# Dispatch\n\nSee [[ripple]].\n")
	target := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n")

	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, UpsertPages: []wiki.Page{linker, target},
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, SoftDeletePages: []string{"entities/ripple.md"},
	}); err != nil {
		t.Fatalf("delete Import: %v", err)
	}

	var stillResolved int
	if err := js.pool.QueryRow(ctx,
		`SELECT count(*) FROM page_links WHERE resolved`).Scan(&stillResolved); err != nil {
		t.Fatal(err)
	}
	if stillResolved != 0 {
		t.Error("a link to a deleted page is still marked resolved")
	}
}

func TestImportDropsSources(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertSources: []diff.SourceRecord{
			{Key: diff.ModuleKey("a"), InputHash: "h"},
			{Key: diff.ModuleKey("b"), InputHash: "h"},
		},
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, DropSources: []diff.Key{diff.ModuleKey("a")},
	}); err != nil {
		t.Fatalf("drop Import: %v", err)
	}

	sources, err := js.LoadSources(ctx, ws)
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(sources) != 1 || sources[0].Key != diff.ModuleKey("b") {
		t.Errorf("sources = %+v, want only module:b", sources)
	}
}

func TestImportIsAtomic(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	// A page whose type violates the CHECK constraint fails mid-transaction.
	// Nothing from the same request may survive, or a crash would leave an
	// index describing pages that were never stored.
	bad := page("entities/broken.md", "broken", "nonsense", "Broken", "# Broken\n\nBody.\n")
	good := page("entities/fine.md", "fine", "entity", "Fine", "# Fine\n\nBody.\n")

	err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{good, bad},
		UpsertSources: []diff.SourceRecord{
			{Key: diff.ModuleKey("x"), InputHash: "h"},
		},
	})
	if err == nil {
		t.Fatal("Import accepted an invalid page type")
	}

	pages, _ := js.LoadPages(ctx, ws)
	if len(pages) != 0 {
		t.Errorf("%d pages survived a failed import; it must be all-or-nothing", len(pages))
	}
	sources, _ := js.LoadSources(ctx, ws)
	if len(sources) != 0 {
		t.Errorf("%d sources survived a failed import", len(sources))
	}
}

func TestImportBumpsRevision(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	readRevision := func() int64 {
		var rev int64
		if err := js.pool.QueryRow(ctx,
			`SELECT revision FROM wikis WHERE workspace_id = $1`, ws).Scan(&rev); err != nil {
			t.Fatal(err)
		}
		return rev
	}

	before := readRevision()
	p := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{p}}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// The revision is what busts UI caches, so it has to move on every import.
	if after := readRevision(); after <= before {
		t.Errorf("revision did not advance: %d -> %d", before, after)
	}
}

func TestImportPersistsDerivedArtifacts(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	p := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n")
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{p},
		Index:       "# Wiki Index\n\n## Entities\n- [[entities/ripple|Ripple]]\n",
		Overview:    "---\ntype: overview\n---\n\n# Overview\n",
		LogEntry:    "## [2026-07-25] build | ws @ abc\n\n- Pages: 1 created\n",
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// These were computed and discarded before wiki_artifacts existed, which
	// meant a built wiki had pages and no way to navigate them.
	for kind, want := range map[string]string{
		"index":    "## Entities",
		"overview": "# Overview",
		"log":      "build | ws @ abc",
	} {
		got, err := js.LoadArtifact(ctx, ws, kind)
		if err != nil {
			t.Fatalf("LoadArtifact(%s): %v", kind, err)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s artifact missing %q:\n%s", kind, want, got)
		}
	}
}

func TestImportReplacesIndexButAppendsLog(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	for i, entry := range []string{
		"## [2026-07-24] build | ws @ aaa\n",
		"## [2026-07-25] build | ws @ bbb\n",
	} {
		if err := js.Import(ctx, jobs.ImportRequest{
			WorkspaceID: ws,
			Index:       fmt.Sprintf("# Wiki Index\n\nrevision %d\n", i),
			LogEntry:    entry,
		}); err != nil {
			t.Fatalf("Import %d: %v", i, err)
		}
	}

	// The index is a pure function of the page set, so only the newest is
	// correct and it is replaced wholesale.
	index, err := js.LoadArtifact(ctx, ws, "index")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(index, "revision 0") {
		t.Error("index was appended rather than replaced")
	}
	if !strings.Contains(index, "revision 1") {
		t.Errorf("index does not hold the newest version:\n%s", index)
	}

	// The log is history and cannot be recomputed, so both entries must survive.
	logBody, err := js.LoadArtifact(ctx, ws, "log")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"aaa", "bbb"} {
		if !strings.Contains(logBody, want) {
			t.Errorf("log lost entry %q:\n%s", want, logBody)
		}
	}
	if !strings.HasPrefix(logBody, wiki.LogHeader) {
		t.Errorf("log was not seeded with a header:\n%s", logBody)
	}
}

func TestLoadArtifactMissingIsEmptyNotAnError(t *testing.T) {
	js, ws := jobStore(t)

	// A workspace that has never been built has no artifacts; callers should
	// not need to distinguish that from a failure.
	got, err := js.LoadArtifact(context.Background(), ws, "index")
	if err != nil {
		t.Errorf("LoadArtifact on an unbuilt workspace: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestLoadSteering(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	p := page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n\nBody.\n")
	if err := js.Import(ctx, jobs.ImportRequest{WorkspaceID: ws, UpsertPages: []wiki.Page{p}}); err != nil {
		t.Fatalf("Import: %v", err)
	}

	for kind, body := range map[string]string{
		"purpose": "Document the dispatch subsystem for on-call engineers.",
		"schema":  "One entity page per service.",
	} {
		if _, err := js.pool.Exec(ctx,
			`INSERT INTO steering_docs (workspace_id, kind, body) VALUES ($1,$2,$3)`,
			ws, kind, body); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := js.pool.Exec(ctx, `
		INSERT INTO page_corrections (page_id, body)
		SELECT id, 'Ripple does not own retry policy; beacon does.' FROM pages WHERE slug='ripple'`,
	); err != nil {
		t.Fatal(err)
	}

	steering, err := js.LoadSteering(ctx, ws)
	if err != nil {
		t.Fatalf("LoadSteering: %v", err)
	}
	if steering.Purpose == "" || steering.Schema == "" {
		t.Errorf("steering docs not loaded: %+v", steering)
	}
	// Corrections are how human knowledge survives regeneration, so they have
	// to reach the prompt keyed by the page they correct.
	if got := steering.Corrections["ripple"]; len(got) != 1 {
		t.Errorf("corrections for ripple = %v, want one", got)
	}
}

func TestRecordRun(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	err := js.RecordRun(ctx, jobs.RunSummary{
		WorkspaceID: ws, Trigger: "manual", Ref: "abc1234",
		Status: jobs.StatusSucceeded, CostUSD: 0.42, Tokens: 1500,
		Created: 3, Updated: 1,
		Items: []jobs.ItemSummary{
			{Key: diff.ModuleKey("ripple"), Status: jobs.StatusSucceeded, CostUSD: 0.21, Turns: 2},
			{Key: diff.ArchOverview, Status: jobs.StatusSucceeded, CostUSD: 0.21, Turns: 2},
		},
	})
	if err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	var runs, items int
	var spend float64
	if err := js.pool.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := js.pool.QueryRow(ctx, `SELECT count(*) FROM run_items`).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if err := js.pool.QueryRow(ctx, `SELECT coalesce(sum(amount_usd),0) FROM spend_ledger`).Scan(&spend); err != nil {
		t.Fatal(err)
	}

	if runs != 1 || items != 2 {
		t.Errorf("runs=%d items=%d, want 1 and 2", runs, items)
	}
	// Spend is ledgered separately so budget windows can be queried without
	// scanning run history.
	if spend != 0.42 {
		t.Errorf("spend = %v, want 0.42", spend)
	}
}

func TestRecordRunSkipsLedgerForFreeRuns(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	// A no-change run costs nothing and should not write a zero-value ledger row.
	if err := js.RecordRun(ctx, jobs.RunSummary{
		WorkspaceID: ws, Trigger: "sweep", Status: jobs.StatusNoChanges,
	}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	var entries int
	if err := js.pool.QueryRow(ctx, `SELECT count(*) FROM spend_ledger`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Errorf("a free run wrote %d ledger entries", entries)
	}
}

func TestLoadOnEmptyWorkspace(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	// A first build reads before anything exists; empty must not be an error.
	pages, err := js.LoadPages(ctx, ws)
	if err != nil || len(pages) != 0 {
		t.Errorf("LoadPages on empty: %v, %d pages", err, len(pages))
	}
	sources, err := js.LoadSources(ctx, ws)
	if err != nil || len(sources) != 0 {
		t.Errorf("LoadSources on empty: %v, %d sources", err, len(sources))
	}
	steering, err := js.LoadSteering(ctx, ws)
	if err != nil {
		t.Errorf("LoadSteering on empty: %v", err)
	}
	if steering.Corrections == nil {
		t.Error("Corrections map should be non-nil so callers need no guard")
	}
}
