package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/extract"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

func docFigure(ref, sha string, w, h int, caption string) extract.Figure {
	return extract.Figure{
		Ref: ref, SHA256: sha, Width: w, Height: h, Caption: caption,
		ContentType: "image/png", Page: 1, Data: []byte("png-bytes-" + sha),
	}
}

// TestBuildStoresFiguresAndOffersThemToTheModel is the whole feature end to
// end inside the pipeline: a document's pictures are stored before generation,
// and the unit's prompt names them so the model can decide which to show.
func TestBuildStoresFiguresAndOffersThemToTheModel(t *testing.T) {
	store := newMemStore()
	blobs := &fakeBlobDeleter{}

	docKey := diff.DocKey(diff.UploadOrigin("report.pdf"))
	runner := &structuredRunner{
		byUnit: map[string][]agent.GeneratedPage{
			string(docKey): {{
				Path: "sources/report.md", Type: "source", Title: "Report",
				Body: "# Report\n\n" + strings.Repeat("Revenue rose across every region. ", 6) +
					"\n\n![Revenue by quarter](figure:fig-sha-chart-0)\n",
			}},
		},
	}

	p := testPipeline(store, runner)
	p.Blobs = blobs

	m := testMap(mapper.Unit{Key: string(docKey), Slug: "report", Hash: "h1", Title: "Report"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""
	// A documents-only bench: the router knows this one document and no
	// modules, so the document is the only unit that runs and the
	// architecture synthesis is correctly absent.
	req.Router = diff.Router{DocPaths: map[string]diff.Key{"report.pdf": docKey}}
	req.Figures = map[string][]extract.Figure{
		string(docKey): {
			docFigure("p1-i0", "sha-chart", 480, 320, "Figure 1: revenue by quarter"),
			docFigure("p2-i1", "sha-photo", 400, 300, ""),
		},
	}

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status != StatusSucceeded {
		t.Fatalf("Status = %q, violations = %v", res.Summary.Status, res.Violations)
	}

	// Stored: bytes in the blob store, rows against the source.
	if len(blobs.put) != 2 {
		t.Errorf("stored %d figure blobs, want 2: %v", len(blobs.put), blobs.put)
	}
	stored, _ := store.FiguresForSources(context.Background(), req.WorkspaceID, []string{string(docKey)})
	if len(stored) != 2 {
		t.Fatalf("recorded %d figures, want 2", len(stored))
	}

	// Offered: the prompt names each id, its size, and the document's caption.
	prompt := runner.generatePrompt
	if !strings.Contains(prompt, "Figures available to this page") {
		t.Fatalf("generate prompt never mentioned figures:\n%s", prompt)
	}
	for _, want := range []string{
		"fig-sha-chart-0", "480x320", "Figure 1: revenue by quarter",
		"fig-sha-photo-1", "no caption in the document",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}

	// Cited: the page the model wrote kept its reference through validation.
	imp := store.lastImport()
	if imp == nil || len(imp.UpsertPages) == 0 {
		t.Fatal("nothing imported")
	}
	if !strings.Contains(imp.UpsertPages[0].Body, "figure:fig-sha-chart-0") {
		t.Errorf("the figure reference did not survive into the page:\n%s", imp.UpsertPages[0].Body)
	}
}

// TestBuildRejectsAnInventedFigureReference: an id the unit does not own must
// fail validation. A broken image is not a gap the wiki can signal -- it is
// just broken -- so this cannot be tolerated the way a dangling link is.
func TestBuildRejectsAnInventedFigureReference(t *testing.T) {
	store := newMemStore()
	blobs := &fakeBlobDeleter{}

	docKey := diff.DocKey(diff.UploadOrigin("report.pdf"))
	runner := &structuredRunner{
		byUnit: map[string][]agent.GeneratedPage{
			string(docKey): {{
				Path: "sources/report.md", Type: "source", Title: "Report",
				Body: "# Report\n\n" + strings.Repeat("Revenue rose across every region. ", 6) +
					"\n\n![Invented](figure:no-such-figure)\n",
			}},
		},
	}

	p := testPipeline(store, runner)
	p.Blobs = blobs
	p.MaxRetries = 0

	m := testMap(mapper.Unit{Key: string(docKey), Slug: "report", Hash: "h1", Title: "Report"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.ScratchDir = ""
	req.Router = diff.Router{DocPaths: map[string]diff.Key{"report.pdf": docKey}}
	req.Figures = map[string][]extract.Figure{
		string(docKey): {docFigure("p1-i0", "sha-chart", 480, 320, "Figure 1")},
	}

	res, err := p.Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.Summary.Status == StatusSucceeded {
		t.Fatal("a page citing a figure that does not exist was accepted")
	}

	var mentioned bool
	for _, v := range res.Violations {
		if strings.Contains(v.Reason, "no-such-figure") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("violations do not name the invented figure: %+v", res.Violations)
	}
}

func TestFiguresForUnitIncludesTheParentDocument(t *testing.T) {
	doc := "doc:upload:book.pdf"
	all := []FigureRecord{
		{ID: "a", SourceKey: doc},
		{ID: "b", SourceKey: doc + "#chapter-two"},
		{ID: "c", SourceKey: "doc:upload:other.pdf"},
	}

	// A section is a span of its document, so the document's figures are in
	// scope for it -- otherwise a split book could never show a picture.
	got := figuresForUnit(all, diff.Key(doc+"#chapter-two"))
	if len(got) != 2 {
		t.Fatalf("section sees %d figures, want its own and its parent's: %+v", len(got), got)
	}

	// The document itself does not inherit its sections' figures: it is the
	// whole, and a chapter's picture belongs to the chapter's page.
	got = figuresForUnit(all, diff.Key(doc))
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("document sees %+v, want only its own", got)
	}
}

func TestFigureSourceKeysCoversParents(t *testing.T) {
	got := figureSourceKeys([]diff.Key{
		"doc:upload:book.pdf#chapter-one",
		"doc:upload:book.pdf#chapter-two",
		"module:ripple",
	})

	want := map[string]bool{
		"doc:upload:book.pdf":             true,
		"doc:upload:book.pdf#chapter-one": true,
		"doc:upload:book.pdf#chapter-two": true,
		"module:ripple":                   true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d keys", got, len(want))
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
}

func TestStoreFiguresWithoutBlobStoreIsANoOp(t *testing.T) {
	store := newMemStore()
	p := testPipeline(store, &structuredRunner{})
	p.Blobs = nil

	docKey := string(diff.DocKey(diff.UploadOrigin("report.pdf")))
	p.storeFigures(context.Background(), BuildRequest{
		WorkspaceID: "w",
		Figures:     map[string][]extract.Figure{docKey: {docFigure("p1-i0", "sha", 480, 320, "")}},
	}, p.logger())

	stored, _ := store.FiguresForSources(context.Background(), "w", []string{docKey})
	if len(stored) != 0 {
		t.Errorf("recorded %d figures with no blob store to hold their bytes", len(stored))
	}
}

// TestStoreFiguresSkipsWhatIsAlreadyStored: a poll that changes nothing must
// not re-upload every picture on the bench or open a transaction per document.
func TestStoreFiguresSkipsWhatIsAlreadyStored(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	blobs := &fakeBlobDeleter{}
	p := testPipeline(store, &structuredRunner{})
	p.Blobs = blobs

	docA := string(diff.DocKey(diff.UploadOrigin("a.pdf")))
	docB := string(diff.DocKey(diff.UploadOrigin("b.pdf")))
	req := BuildRequest{
		WorkspaceID: "w",
		Figures: map[string][]extract.Figure{
			docA: {docFigure("p1-i0", "a1", 10, 10, "A one"), docFigure("p2-i0", "a2", 10, 10, "")},
			docB: {docFigure("p1-i0", "b1", 10, 10, "B one")},
		},
	}

	p.storeFigures(ctx, req, p.logger())
	if blobs.puts != 3 || store.replaceFigureCalls != 2 {
		t.Fatalf("first build: %d Puts, %d ReplaceFigures; want 3 and 2", blobs.puts, store.replaceFigureCalls)
	}

	// Nothing changed.
	p.storeFigures(ctx, req, p.logger())
	if blobs.puts != 3 || store.replaceFigureCalls != 2 {
		t.Errorf("unchanged build did work: %d Puts, %d ReplaceFigures; want 3 and 2",
			blobs.puts, store.replaceFigureCalls)
	}

	// Only B changed: it gains a figure. A must not be touched, and B's
	// existing picture must not be uploaded again.
	req.Figures[docB] = append(req.Figures[docB], docFigure("p3-i0", "b2", 10, 10, "B two"))
	p.storeFigures(ctx, req, p.logger())
	if blobs.puts != 4 {
		t.Errorf("Puts = %d, want 4: only the new figure should upload", blobs.puts)
	}
	if store.replaceFigureCalls != 3 {
		t.Errorf("ReplaceFigures calls = %d, want 3: only the changed document", store.replaceFigureCalls)
	}
	got, _ := store.FiguresForSources(ctx, "w", []string{docB})
	if len(got) != 2 {
		t.Errorf("B has %d recorded figures, want 2", len(got))
	}

	// A caption edit is a change even though no picture is: it must be
	// recorded, though nothing needs uploading.
	req.Figures[docA][0].Caption = "A one, revised"
	p.storeFigures(ctx, req, p.logger())
	if blobs.puts != 4 || store.replaceFigureCalls != 4 {
		t.Errorf("caption edit: %d Puts, %d ReplaceFigures; want 4 and 4", blobs.puts, store.replaceFigureCalls)
	}

	// A removed figure is a change, and its blob is freed.
	req.Figures[docA] = req.Figures[docA][:1]
	p.storeFigures(ctx, req, p.logger())
	if store.replaceFigureCalls != 5 || len(blobs.deleted) != 1 {
		t.Errorf("removal: %d ReplaceFigures, %d deleted; want 5 and 1", store.replaceFigureCalls, len(blobs.deleted))
	}
}
