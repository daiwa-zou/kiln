package store

import (
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// inFlightRun is what a run that started before a delete hands to Import after
// it: alpha and beta both regenerated, each with its own page and a rewrite of
// the page they share. The request is built before the delete and applied
// after it, which is the whole race.
func inFlightRun(ws string) jobs.ImportRequest {
	alphaKey := diff.DocKey(diff.UploadOrigin("alpha.md"))
	betaKey := diff.DocKey(diff.UploadOrigin("beta.md"))
	return jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{
			page("sources/alpha.md", "alpha", "source", "Alpha", "# Alpha v2\n"),
			page("sources/alpha-appendix.md", "alpha-appendix", "source", "Alpha Appendix", "# Appendix v2\n"),
			page("sources/beta.md", "beta", "source", "Beta", "# Beta v2\n"),
			page("concepts/shared.md", "shared", "concept", "Shared", "# Shared v2\n"),
		},
		UpsertSources: []diff.SourceRecord{
			{
				Key: alphaKey, InputHash: "ha2",
				FilesWritten: []string{"sources/alpha.md", "concepts/shared.md"},
				BlobKeys:     []string{"blob-alpha"},
			},
			{
				Key: alphaKey + "#appendix", InputHash: "hs2",
				FilesWritten: []string{"sources/alpha-appendix.md"},
				BlobKeys:     []string{"blob-alpha"},
			},
			{
				Key: betaKey, InputHash: "hb2",
				FilesWritten: []string{"sources/beta.md", "concepts/shared.md"},
				BlobKeys:     []string{"blob-beta"},
			},
		},
		Index:    "# Index\n- alpha\n- beta\n",
		Overview: "# Overview\n",
		LogEntry: "## build\n",
	}
}

// uploadFile records an uploaded document, which is what makes a source keyed
// doc:upload:<path> live: a source whose file row is gone is a deleted document.
func uploadFile(t *testing.T, js *WikiStore, ws, path string) string {
	t.Helper()
	id, _, err := js.CreateFile(context.Background(), FileRow{
		WorkspaceID: ws, Path: path, BlobKey: "ws/" + ws + "/uploads/" + path,
		SizeBytes: 1, SHA256: "sha-" + path,
	})
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	return id
}

func pageBody(t *testing.T, js *WikiStore, ws, path string) string {
	t.Helper()
	pages, err := js.LoadPages(context.Background(), ws)
	if err != nil {
		t.Fatalf("LoadPages: %v", err)
	}
	for _, p := range pages {
		if p.Path == path {
			return p.Body
		}
	}
	return ""
}

// TestImportAfterDeleteDoesNotResurrectTheDocument is the race the advisory
// lock alone never closed: the lock orders the delete before the import, but an
// import that was already planned still carries the deleted document's source
// and pages, and used to write them straight back.
func TestImportAfterDeleteDoesNotResurrectTheDocument(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	alphaID, _ := seedTwoDocumentWiki(t, js, ws)
	// The run extracted a picture from alpha before the delete.
	alphaKey := string(diff.DocKey(diff.UploadOrigin("alpha.md")))
	if _, err := js.ReplaceFigures(ctx, ws, alphaKey, []jobs.FigureRecord{
		figure(alphaKey, "p1-i0", "sha-a", "blob-fig-a"),
	}); err != nil {
		t.Fatal(err)
	}

	run := inFlightRun(ws) // planned while alpha still existed

	if _, _, err := js.DeleteFile(ctx, ws, alphaID); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	if err := js.Import(ctx, run); err != nil {
		t.Fatalf("Import after delete: %v", err)
	}

	// X stays deleted.
	srcs := sourceByKey(t, js, ws)
	for k := range srcs {
		if strings.Contains(string(k), "alpha.md") {
			t.Errorf("source %s was resurrected by an import planned before the delete", k)
		}
	}
	live := livePages(t, js, ws)
	if live["sources/alpha.md"] || live["sources/alpha-appendix.md"] {
		t.Errorf("pages only alpha produced are live again: %v", live)
	}

	// Nothing references the deleted blob.
	for k, s := range srcs {
		for _, b := range s.BlobKeys {
			if b == "blob-alpha" {
				t.Errorf("source %s still references the deleted blob", k)
			}
		}
	}

	// The rest of the import still landed.
	if got := pageBody(t, js, ws, "sources/beta.md"); got != "# Beta v2\n" {
		t.Errorf("beta's page = %q, want the new version: the delete cost the whole run", got)
	}
	if got := srcs[diff.DocKey(diff.UploadOrigin("beta.md"))].InputHash; got != "hb2" {
		t.Errorf("beta's source hash = %q, want hb2", got)
	}

	// The page both documents wrote is kept for beta, but flagged: its prose
	// was written with alpha in view.
	if !live["concepts/shared.md"] {
		t.Error("the shared page beta still claims was dropped")
	}
	if !srcs[diff.DocKey(diff.UploadOrigin("beta.md"))].NeedsRegen {
		t.Error("beta owns a page written with the deleted document in view and was not flagged to rewrite it")
	}

	// The pictures the run recorded after the delete are refused, not stored.
	orphaned, err := js.ReplaceFigures(ctx, ws, alphaKey, []jobs.FigureRecord{
		figure(alphaKey, "p1-i0", "sha-a", "blob-fig-a"),
	})
	if err != nil {
		t.Fatalf("ReplaceFigures after delete: %v", err)
	}
	figs, err := js.ListFigures(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(figs) != 0 {
		t.Errorf("figures recreated for a deleted document: %+v", figs)
	}
	if len(orphaned) != 1 || orphaned[0] != "blob-fig-a" {
		t.Errorf("orphaned = %v, want the blob that was just written for nothing", orphaned)
	}
}

// TestImportSkipsSourcesWhoseConnectorWasDeleted: before the fence the insert
// hit the connector foreign key and the whole import failed, discarding every
// generated page after the spend was already ledgered.
func TestImportSkipsSourcesWhoseConnectorWasDeleted(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	repo, err := js.CreateConnector(ctx, ConnectorRow{
		WorkspaceID: ws, Kind: "git", Name: "repo", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	docs, err := js.CreateConnector(ctx, ConnectorRow{
		WorkspaceID: ws, Kind: "upload", Name: "docs", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	run := jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{
			page("modules/api.md", "api", "entity", "API", "# API\n"),
			page("modules/ui.md", "ui", "entity", "UI", "# UI\n"),
		},
		UpsertSources: []diff.SourceRecord{
			{Key: diff.ModuleKey("api"), InputHash: "h1", ConnectorID: repo,
				FilesWritten: []string{"modules/api.md"}},
			{Key: diff.ModuleKey("ui"), InputHash: "h2", ConnectorID: docs,
				FilesWritten: []string{"modules/ui.md"}},
		},
		Index: "# Index\n- api\n- ui\n",
	}

	// The admin deletes the repository while the run is generating. This is a
	// connector with no sources on record yet: its first sync is the run.
	if _, err := js.DeleteConnector(ctx, ws, repo); err != nil {
		t.Fatalf("DeleteConnector: %v", err)
	}

	if err := js.Import(ctx, run); err != nil {
		t.Fatalf("Import failed because a connector was deleted mid-run: %v", err)
	}

	srcs := sourceByKey(t, js, ws)
	if _, ok := srcs[diff.ModuleKey("api")]; ok {
		t.Error("a source of the deleted connector was recorded")
	}
	if _, ok := srcs[diff.ModuleKey("ui")]; !ok {
		t.Error("the other connector's source did not land")
	}
	live := livePages(t, js, ws)
	if live["modules/api.md"] {
		t.Error("a page only the deleted connector's source wrote was stored")
	}
	if !live["modules/ui.md"] {
		t.Error("the other connector's page did not land: the delete cost the whole run")
	}
}

// TestUploadSourceLiveMatchesSectionsAndHashedPaths: a section unit's key is
// "<doc key>#<section>", and a path may itself contain '#', so the match has to
// try every split rather than cutting at the first one.
func TestUploadSourceLiveMatchesSectionsAndHashedPaths(t *testing.T) {
	files := map[string]bool{"plan.md": true, "c#/notes.md": true}
	tests := []struct {
		key  string
		live bool
	}{
		{"doc:upload:plan.md", true},
		{"doc:upload:plan.md#appendix", true},
		{"doc:upload:c#/notes.md", true},
		{"doc:upload:c#/notes.md#intro", true},
		{"doc:upload:gone.md", false},
		{"doc:upload:gone.md#appendix", false},
		// Not an upload: never judged against the file table.
		{"module:api", true},
		{"doc:docs/readme.md", true},
	}
	for _, tt := range tests {
		if got := uploadSourceLive(tt.key, files); got != tt.live {
			t.Errorf("uploadSourceLive(%q) = %v, want %v", tt.key, got, tt.live)
		}
	}
}
