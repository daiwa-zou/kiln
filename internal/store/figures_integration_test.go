package store

import (
	"context"
	"errors"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

func figure(sourceKey, ref, sha, blobKey string) jobs.FigureRecord {
	return jobs.FigureRecord{
		SourceKey: sourceKey, Ref: ref, SHA256: sha, BlobKey: blobKey,
		ContentType: "image/png", Width: 480, Height: 320, SizeBytes: 1024,
		Page: 1, Caption: "Figure 1: revenue", Ordinal: 0,
	}
}

func TestReplaceFiguresIsTheDocumentsCurrentSet(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	key := string(diff.DocKey(diff.UploadOrigin("report.pdf")))

	orphaned, err := js.ReplaceFigures(ctx, ws, key, []jobs.FigureRecord{
		figure(key, "p1-i0", "sha-chart", "blob-chart"),
		figure(key, "p2-i1", "sha-photo", "blob-photo"),
	})
	if err != nil {
		t.Fatalf("ReplaceFigures: %v", err)
	}
	if len(orphaned) != 0 {
		t.Errorf("first write orphaned %v", orphaned)
	}

	first, err := js.FiguresForSources(ctx, ws, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("stored %d figures, want 2", len(first))
	}
	for _, f := range first {
		if f.ID == "" {
			t.Errorf("figure %s has no id; a page could not reference it", f.Ref)
		}
	}
	chartID := ""
	for _, f := range first {
		if f.SHA256 == "sha-chart" {
			chartID = f.ID
		}
	}

	// The document is edited: the photo is gone, the chart is unchanged, and a
	// new diagram is added.
	orphaned, err = js.ReplaceFigures(ctx, ws, key, []jobs.FigureRecord{
		figure(key, "p1-i0", "sha-chart", "blob-chart"),
		figure(key, "p3-i2", "sha-diagram", "blob-diagram"),
	})
	if err != nil {
		t.Fatalf("second ReplaceFigures: %v", err)
	}
	if len(orphaned) != 1 || orphaned[0] != "blob-photo" {
		t.Errorf("orphaned = %v, want only the removed photo's blob", orphaned)
	}

	second, err := js.FiguresForSources(ctx, ws, []string{key})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 {
		t.Fatalf("after edit there are %d figures, want 2", len(second))
	}
	// The unchanged chart keeps its id, or every rebuild would break the pages
	// already pointing at it.
	for _, f := range second {
		if f.SHA256 == "sha-chart" && f.ID != chartID {
			t.Errorf("the unchanged chart's id moved %s -> %s", chartID, f.ID)
		}
	}
}

// TestReplaceFiguresKeepsBlobsTwoDocumentsShare: blob keys are content
// digests, so the same chart in two documents is one blob. Removing it from
// one document must not blank it on the other's page.
func TestReplaceFiguresKeepsBlobsTwoDocumentsShare(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	a := string(diff.DocKey(diff.UploadOrigin("a.pdf")))
	b := string(diff.DocKey(diff.UploadOrigin("b.pdf")))

	if _, err := js.ReplaceFigures(ctx, ws, a, []jobs.FigureRecord{
		figure(a, "p1-i0", "sha-shared", "blob-shared"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReplaceFigures(ctx, ws, b, []jobs.FigureRecord{
		figure(b, "p1-i0", "sha-shared", "blob-shared"),
	}); err != nil {
		t.Fatal(err)
	}

	// a drops the shared chart; b still has it.
	orphaned, err := js.ReplaceFigures(ctx, ws, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 0 {
		t.Errorf("orphaned %v, but the other document still references those bytes", orphaned)
	}

	// Once b drops it too, the bytes are genuinely dead.
	orphaned, err = js.ReplaceFigures(ctx, ws, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 1 || orphaned[0] != "blob-shared" {
		t.Errorf("orphaned = %v, want the now-unreferenced blob", orphaned)
	}
}

// TestDeletingASourceTakesItsFigures ties figures into the deletion cascade:
// the pictures from a deleted PDF must stop being served.
func TestDeletingASourceTakesItsFigures(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	alphaID, _ := seedTwoDocumentWiki(t, js, ws)
	alphaKey := string(diff.DocKey(diff.UploadOrigin("alpha.md")))
	betaKey := string(diff.DocKey(diff.UploadOrigin("beta.md")))

	if _, err := js.ReplaceFigures(ctx, ws, alphaKey, []jobs.FigureRecord{
		figure(alphaKey, "p1-i0", "sha-alpha-chart", "blob-alpha-chart"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReplaceFigures(ctx, ws, betaKey, []jobs.FigureRecord{
		figure(betaKey, "p1-i0", "sha-beta-chart", "blob-beta-chart"),
	}); err != nil {
		t.Fatal(err)
	}

	_, cascade, err := js.DeleteFile(ctx, ws, alphaID)
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	var freed bool
	for _, b := range cascade.DeleteBlobs {
		if b == "blob-alpha-chart" {
			freed = true
		}
		if b == "blob-beta-chart" {
			t.Error("the surviving document's figure blob was freed")
		}
	}
	if !freed {
		t.Errorf("DeleteBlobs = %v, want alpha's figure blob among them", cascade.DeleteBlobs)
	}

	left, err := js.FiguresForSources(ctx, ws, []string{alphaKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the deleted document still has %d figures on record", len(left))
	}
	kept, err := js.FiguresForSources(ctx, ws, []string{betaKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Errorf("the surviving document lost its figures")
	}
}

func TestFigureIsWorkspaceScoped(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	key := string(diff.DocKey(diff.UploadOrigin("report.pdf")))

	if _, err := js.ReplaceFigures(ctx, ws, key, []jobs.FigureRecord{
		figure(key, "p1-i0", "sha-chart", "blob-chart"),
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := js.FiguresForSources(ctx, ws, []string{key})
	if err != nil || len(stored) != 1 {
		t.Fatalf("setup: %v %+v", err, stored)
	}

	other, err := js.EnsureWorkspace(ctx, "test-org", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Figure(ctx, other, stored[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant read = %v, want ErrNotFound", err)
	}
	if _, err := js.Figure(ctx, ws, stored[0].ID); err != nil {
		t.Errorf("same-tenant read failed: %v", err)
	}

	// A figure id travels in page URLs, so junk arrives routinely. It must
	// read as absent rather than as a server error.
	if _, err := js.Figure(ctx, ws, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed id = %v, want ErrNotFound", err)
	}
}
