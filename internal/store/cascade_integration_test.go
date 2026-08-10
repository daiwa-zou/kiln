package store

import (
	"context"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// seedTwoDocumentWiki builds the shape every cascade question is really about:
// two uploaded documents, a page each that only its own document produced, and
// one concept page both of them wrote. Returns the two file row ids.
//
// alpha additionally has a section unit, because a document's sections are
// sources in the table and spans of the same file in reality -- the case where
// treating them as independent would leave a page nothing can regenerate.
func seedTwoDocumentWiki(t *testing.T, js *WikiStore, ws string) (alphaID, betaID string) {
	t.Helper()
	ctx := context.Background()

	alphaID, _, err := js.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "alpha.md", BlobKey: "blob-alpha",
		SizeBytes: 10, SHA256: "a1",
	})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	betaID, _, err = js.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "beta.md", BlobKey: "blob-beta",
		SizeBytes: 10, SHA256: "b1",
	})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	alphaKey := diff.DocKey(diff.UploadOrigin("alpha.md"))
	betaKey := diff.DocKey(diff.UploadOrigin("beta.md"))

	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{
			page("sources/alpha.md", "alpha", "source", "Alpha", "# Alpha\n"),
			page("sources/alpha-appendix.md", "alpha-appendix", "source", "Alpha Appendix", "# Appendix\n"),
			page("sources/beta.md", "beta", "source", "Beta", "# Beta\n"),
			page("concepts/shared.md", "shared", "concept", "Shared", "# Shared\n\nDrawn from Alpha and Beta.\n"),
		},
		UpsertSources: []diff.SourceRecord{
			{
				Key: alphaKey, InputHash: "ha",
				FilesWritten: []string{"sources/alpha.md", "concepts/shared.md"},
				BlobKeys:     []string{"blob-alpha", "blob-corpus"},
			},
			{
				Key: alphaKey + "#appendix", InputHash: "hs",
				FilesWritten: []string{"sources/alpha-appendix.md"},
				BlobKeys:     []string{"blob-alpha"},
			},
			{
				Key: betaKey, InputHash: "hb",
				FilesWritten: []string{"sources/beta.md", "concepts/shared.md"},
				BlobKeys:     []string{"blob-beta", "blob-corpus"},
			},
		},
	}); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	return alphaID, betaID
}

// livePages is the set of page paths a reader would see.
func livePages(t *testing.T, js *WikiStore, ws string) map[string]bool {
	t.Helper()
	pages, err := js.LoadPages(context.Background(), ws)
	if err != nil {
		t.Fatalf("LoadPages: %v", err)
	}
	out := map[string]bool{}
	for _, p := range pages {
		out[p.Path] = true
	}
	return out
}

func sourceByKey(t *testing.T, js *WikiStore, ws string) map[diff.Key]diff.SourceRecord {
	t.Helper()
	recs, err := js.LoadSources(context.Background(), ws)
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	out := map[diff.Key]diff.SourceRecord{}
	for _, r := range recs {
		out[r.Key] = r
	}
	return out
}

// TestDeleteFileRemovesThePagesItProduced is the whole point of the feature:
// after deleting a document, nothing in the wiki still claims to describe it.
func TestDeleteFileRemovesThePagesItProduced(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	alphaID, _ := seedTwoDocumentWiki(t, js, ws)

	before := livePages(t, js, ws)
	if len(before) != 4 {
		t.Fatalf("seeded %d pages, want 4: %v", len(before), before)
	}

	blobKey, c, err := js.DeleteFile(ctx, ws, alphaID)
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if blobKey != "blob-alpha" {
		t.Errorf("returned blob %q, want blob-alpha", blobKey)
	}

	after := livePages(t, js, ws)
	if after["sources/alpha.md"] {
		t.Error("sources/alpha.md survived the deletion of the document that produced it")
	}
	// The section's page goes with the document. A section is a span of the
	// same file, so leaving it would strand a page with no source to rebuild it.
	if after["sources/alpha-appendix.md"] {
		t.Error("the section page survived; sections must go with their document")
	}
	if !after["sources/beta.md"] {
		t.Error("beta's own page was removed by alpha's deletion")
	}
	// The shared page is claimed by a source that is still live. Removing it
	// would destroy a concept two documents built.
	if !after["concepts/shared.md"] {
		t.Error("the shared concept page was removed though beta still claims it")
	}

	if len(c.DeletePages) != 2 {
		t.Errorf("cascade removed %v, want alpha's two pages", c.DeletePages)
	}
	if len(c.RegeneratePages) != 1 || c.RegeneratePages[0] != "concepts/shared.md" {
		t.Errorf("RegeneratePages = %v, want the shared page", c.RegeneratePages)
	}
	// blob-corpus is still referenced by beta; only alpha's own bytes are free.
	if len(c.DeleteBlobs) != 1 || c.DeleteBlobs[0] != "blob-alpha" {
		t.Errorf("DeleteBlobs = %v, want only blob-alpha", c.DeleteBlobs)
	}

	srcs := sourceByKey(t, js, ws)
	alphaKey := diff.DocKey(diff.UploadOrigin("alpha.md"))
	if _, ok := srcs[alphaKey]; ok {
		t.Error("alpha's source row outlived the document")
	}
	if _, ok := srcs[alphaKey+"#appendix"]; ok {
		t.Error("alpha's section source row outlived the document")
	}

	// The surviving owner of the shared page carries the debt: its prose still
	// describes Alpha, and the next run has to rewrite it.
	beta := srcs[diff.DocKey(diff.UploadOrigin("beta.md"))]
	if !beta.NeedsRegen {
		t.Error("beta was not flagged for regeneration; the shared page still describes Alpha")
	}

	// The reader-facing counters move with the content, or the UI keeps
	// serving a cached wiki that no longer exists.
	var revision, pageCount int
	if err := js.pool.QueryRow(ctx,
		`SELECT revision, page_count FROM wikis WHERE workspace_id = $1`, ws).
		Scan(&revision, &pageCount); err != nil {
		t.Fatalf("read wiki counters: %v", err)
	}
	if pageCount != 2 {
		t.Errorf("page_count = %d, want 2", pageCount)
	}
	if revision < 2 {
		t.Errorf("revision = %d; the deletion did not bust reader caches", revision)
	}
}

// TestDeleteFileSoftDeletesForRecovery holds the line the review queue used to
// hold. Deletion is immediate now, but it is still recoverable for the
// retention window -- that is what makes doing it without a second question
// defensible.
func TestDeleteFileSoftDeletesForRecovery(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	alphaID, _ := seedTwoDocumentWiki(t, js, ws)
	if _, _, err := js.DeleteFile(ctx, ws, alphaID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	var n int
	if err := js.pool.QueryRow(ctx, `
		SELECT count(*) FROM pages
		WHERE path = 'sources/alpha.md' AND deleted_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count soft-deleted: %v", err)
	}
	if n != 1 {
		t.Fatalf("found %d soft-deleted rows for alpha, want 1; the page was destroyed outright", n)
	}
}

// TestDeleteFileUnresolvesLinksIntoRemovedPages: a link to a page that no
// longer exists is the wiki's gap signal, and it only works if the deletion
// puts the link back into the unresolved state.
func TestDeleteFileUnresolvesLinksIntoRemovedPages(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	alphaID, _ := seedTwoDocumentWiki(t, js, ws)

	// Re-import beta's page with a link to alpha, so there is a resolved link
	// pointing at a page the deletion is about to take.
	linking := page("sources/beta.md", "beta", "source", "Beta", "# Beta\n\nSee [[alpha]].\n")
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws, UpsertPages: []wiki.Page{linking},
	}); err != nil {
		t.Fatalf("link import: %v", err)
	}

	var resolved bool
	if err := js.pool.QueryRow(ctx, `
		SELECT resolved FROM page_links WHERE to_slug = 'alpha'`).Scan(&resolved); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if !resolved {
		t.Fatal("link to alpha did not resolve before the deletion; test cannot prove anything")
	}

	if _, _, err := js.DeleteFile(ctx, ws, alphaID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	if err := js.pool.QueryRow(ctx, `
		SELECT resolved FROM page_links WHERE to_slug = 'alpha'`).Scan(&resolved); err != nil {
		t.Fatalf("read link after delete: %v", err)
	}
	if resolved {
		t.Error("the link still points at a deleted page; it should read as a gap")
	}
}

// TestDeleteFileClosesItsOpenDeletionReview: a source can already be the
// subject of a review filed when it went missing from a sync. Deleting it
// answers that question, and leaving it open would queue a decision about
// something that no longer exists.
func TestDeleteFileClosesItsOpenDeletionReview(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	alphaID, _ := seedTwoDocumentWiki(t, js, ws)
	alphaKey := diff.DocKey(diff.UploadOrigin("alpha.md"))

	if err := js.EnsureDeletionReviews(ctx, ws, []jobs.DeletionCandidate{
		{Key: alphaKey, Detail: "Source is no longer present in the map."},
	}); err != nil {
		t.Fatalf("EnsureDeletionReviews: %v", err)
	}

	if _, _, err := js.DeleteFile(ctx, ws, alphaID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	var status string
	if err := js.pool.QueryRow(ctx, `
		SELECT status FROM review_items WHERE kind = 'deletion' AND title = $1`,
		string(alphaKey)).Scan(&status); err != nil {
		t.Fatalf("read review: %v", err)
	}
	if status != "resolved" {
		t.Errorf("review status = %q, want resolved", status)
	}
}

// TestDeleteFileWithNoPagesLeavesTheWikiAlone: deleting a document that was
// uploaded but never built must not bump the revision or touch content.
func TestDeleteFileWithNoPagesLeavesTheWikiAlone(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	seedTwoDocumentWiki(t, js, ws)

	unbuilt, _, err := js.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "gamma.md", BlobKey: "blob-gamma", SizeBytes: 3, SHA256: "g1",
	})
	if err != nil {
		t.Fatalf("create gamma: %v", err)
	}

	var before int
	if err := js.pool.QueryRow(ctx,
		`SELECT revision FROM wikis WHERE workspace_id = $1`, ws).Scan(&before); err != nil {
		t.Fatalf("read revision: %v", err)
	}

	if _, c, err := js.DeleteFile(ctx, ws, unbuilt); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	} else if len(c.DeletePages) != 0 {
		t.Errorf("deleting an unbuilt document removed %v", c.DeletePages)
	}

	var after int
	if err := js.pool.QueryRow(ctx,
		`SELECT revision FROM wikis WHERE workspace_id = $1`, ws).Scan(&after); err != nil {
		t.Fatalf("read revision: %v", err)
	}
	if after != before {
		t.Errorf("revision moved %d -> %d for a deletion that changed nothing", before, after)
	}
	if len(livePages(t, js, ws)) != 4 {
		t.Error("pages changed when deleting a document that had produced none")
	}
}

// TestDeleteConnectorRemovesItsPages covers the other way a source leaves: the
// connector that syncs it is removed. The source rows cascade away in the
// schema, so the pages must be planned before the connector row goes or the
// record of what they wrote is gone with it.
func TestDeleteConnectorRemovesItsPages(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	connID, err := js.CreateConnector(ctx, ConnectorRow{
		WorkspaceID: ws, Kind: "git", Name: "repo", Config: map[string]any{},
		TriggerMode: "manual", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConnector: %v", err)
	}

	// One source from the connector, one uploaded document, and a concept page
	// both wrote -- so the test also proves the connector's deletion is scoped
	// to its own sources.
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{
			page("entities/ripple.md", "ripple", "entity", "Ripple", "# Ripple\n"),
			page("sources/notes.md", "notes", "source", "Notes", "# Notes\n"),
			page("concepts/shared.md", "shared", "concept", "Shared", "# Shared\n"),
		},
		UpsertSources: []diff.SourceRecord{
			{
				Key: diff.ModuleKey("ripple"), InputHash: "h1", ConnectorID: connID,
				FilesWritten: []string{"entities/ripple.md", "concepts/shared.md"},
				BlobKeys:     []string{"blob-repo"},
			},
			{
				Key: diff.DocKey(diff.UploadOrigin("notes.md")), InputHash: "h2",
				FilesWritten: []string{"sources/notes.md", "concepts/shared.md"},
			},
		},
	}); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	c, err := js.DeleteConnector(ctx, ws, connID)
	if err != nil {
		t.Fatalf("DeleteConnector: %v", err)
	}
	if len(c.DeletePages) != 1 || c.DeletePages[0] != "entities/ripple.md" {
		t.Errorf("DeletePages = %v, want only the connector's own page", c.DeletePages)
	}
	if len(c.DeleteBlobs) != 1 || c.DeleteBlobs[0] != "blob-repo" {
		t.Errorf("DeleteBlobs = %v, want blob-repo", c.DeleteBlobs)
	}

	after := livePages(t, js, ws)
	if after["entities/ripple.md"] {
		t.Error("the connector's page survived its deletion")
	}
	if !after["sources/notes.md"] || !after["concepts/shared.md"] {
		t.Errorf("deleting a connector took pages it did not exclusively own: %v", after)
	}

	srcs := sourceByKey(t, js, ws)
	if _, ok := srcs[diff.ModuleKey("ripple")]; ok {
		t.Error("the connector's source row survived")
	}
	notes := srcs[diff.DocKey(diff.UploadOrigin("notes.md"))]
	if !notes.NeedsRegen {
		t.Error("the surviving owner of the shared page was not flagged for regeneration")
	}

	conns, err := js.ListConnectors(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 0 {
		t.Errorf("connector row survived: %+v", conns)
	}
}

// TestDeleteConnectorIsWorkspaceScoped: a cross-tenant id must read as absent,
// and must not have destroyed anything on the way to finding that out.
func TestDeleteConnectorIsWorkspaceScoped(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	seedTwoDocumentWiki(t, js, ws)
	other, err := js.EnsureWorkspace(ctx, "test-org", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	connID, err := js.CreateConnector(ctx, ConnectorRow{
		WorkspaceID: ws, Kind: "git", Name: "repo", Config: map[string]any{},
		TriggerMode: "manual", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := js.DeleteConnector(ctx, other, connID); err == nil {
		t.Fatal("cross-tenant delete succeeded")
	}
	if len(livePages(t, js, ws)) != 4 {
		t.Error("a failed cross-tenant delete changed the owning workspace's pages")
	}
}

// TestRegenerationFlagClearsOnRebuild: the flag is a debt, and importing the
// regenerated page is what pays it. A flag that survived its own rebuild would
// make every subsequent run pay to rewrite the same page.
func TestRegenerationFlagClearsOnRebuild(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	alphaID, _ := seedTwoDocumentWiki(t, js, ws)
	if _, _, err := js.DeleteFile(ctx, ws, alphaID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	betaKey := diff.DocKey(diff.UploadOrigin("beta.md"))
	if !sourceByKey(t, js, ws)[betaKey].NeedsRegen {
		t.Fatal("beta was not flagged; test cannot prove the clear")
	}

	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{
			page("concepts/shared.md", "shared", "concept", "Shared", "# Shared\n\nDrawn from Beta.\n"),
		},
		UpsertSources: []diff.SourceRecord{{
			Key: betaKey, InputHash: "hb",
			FilesWritten: []string{"sources/beta.md", "concepts/shared.md"},
		}},
	}); err != nil {
		t.Fatalf("rebuild import: %v", err)
	}

	if sourceByKey(t, js, ws)[betaKey].NeedsRegen {
		t.Error("the flag survived the rebuild it was asking for")
	}
}
