package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// seedFigure stores one figure's bytes and row, and returns its id.
func seedFigure(t *testing.T, js *store.WikiStore, wsID, sourceKey, sha string, data []byte) string {
	t.Helper()
	ctx := context.Background()

	key := blob.FigureKey(wsID, sha)
	if err := lastMemBlobs.Put(ctx, key, strings.NewReader(string(data)), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReplaceFigures(ctx, wsID, sourceKey, []jobs.FigureRecord{{
		SourceKey: sourceKey, Ref: "p1-i0", BlobKey: key,
		ContentType: "image/png", Width: 480, Height: 320,
		SizeBytes: int64(len(data)), Page: 1,
		Caption: "Figure 1: revenue by quarter", SHA256: sha,
	}}); err != nil {
		t.Fatal(err)
	}
	stored, err := js.FiguresForSources(ctx, wsID, []string{sourceKey})
	if err != nil || len(stored) == 0 {
		t.Fatalf("seed figure: %v %+v", err, stored)
	}
	return stored[0].ID
}

func TestFigureIsServedWithItsBytes(t *testing.T) {
	srv, js, wsID := testServer(t)
	data := []byte("\x89PNG\r\n\x1a\n-pretend-chart")
	id := seedFigure(t, js, wsID, string(diff.DocKey(diff.UploadOrigin("report.pdf"))), "sha-chart", data)

	res, err := http.Get(srv.URL + "/api/v1/workspaces/demo/figures/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET figure = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
	// A document's image must never be sniffed into something executable.
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("figure served without nosniff")
	}
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want an immutable policy", cc)
	}

	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("body = %q, want the stored bytes", got)
	}
}

func TestFigureIsWorkspaceScopedOverHTTP(t *testing.T) {
	srv, js, wsID := testServer(t)
	ctx := context.Background()

	id := seedFigure(t, js, wsID, string(diff.DocKey(diff.UploadOrigin("report.pdf"))), "sha-chart", []byte("bytes"))
	if _, err := js.EnsureWorkspace(ctx, "test-org", "other", "Other"); err != nil {
		t.Fatal(err)
	}

	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/other/figures/"+id, "", nil, nil); code != http.StatusNotFound {
		t.Errorf("cross-workspace figure = %d, want 404", code)
	}
	// A figure id travels inside page HTML, so junk arrives routinely and must
	// not read as a server error.
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/figures/not-a-uuid", "", nil, nil); code != http.StatusNotFound {
		t.Errorf("malformed figure id = %d, want 404", code)
	}
}

// TestPageBodyResolvesFigureReferences: pages are stored with portable
// `figure:ID` references and must come back over the API as fetchable URLs.
func TestPageBodyResolvesFigureReferences(t *testing.T) {
	srv, js, wsID := testServer(t)
	ctx := context.Background()

	sourceKey := string(diff.DocKey(diff.UploadOrigin("report.pdf")))
	id := seedFigure(t, js, wsID, sourceKey, "sha-chart", []byte("bytes"))

	body := "# Report\n\nRevenue rose.\n\n![Revenue by quarter](figure:" + id + ")\n\n" +
		"![Gone](figure:00000000-0000-4000-8000-000000000000)\n"
	if err := js.Import(ctx, jobs.ImportRequest{
		WorkspaceID: wsID,
		UpsertPages: []wiki.Page{{
			Path: "sources/report.md", Slug: "report",
			Meta: wiki.Frontmatter{
				Type: "source", Title: "Report", Created: "2026-08-13", Updated: "2026-08-13",
			},
			Body: body,
		}},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}

	var page map[string]any
	if code := get(t, srv, "/api/v1/workspaces/demo/pages/sources/report.md", &page); code != http.StatusOK {
		t.Fatalf("GET page = %d", code)
	}
	got, _ := page["body"].(string)

	want := "![Revenue by quarter](/api/v1/workspaces/demo/figures/" + id + ")"
	if !strings.Contains(got, want) {
		t.Errorf("body did not resolve the known figure:\n%s", got)
	}
	// A reference to a figure that is not there stays in its literal form
	// rather than becoming a URL that would 404 as a broken image.
	if !strings.Contains(got, "![Gone](figure:00000000-0000-4000-8000-000000000000)") {
		t.Errorf("unknown figure reference was rewritten:\n%s", got)
	}
}

func TestFiguresListReportsWhatDocumentsContributed(t *testing.T) {
	srv, js, wsID := testServer(t)
	sourceKey := string(diff.DocKey(diff.UploadOrigin("report.pdf")))
	id := seedFigure(t, js, wsID, sourceKey, "sha-chart", []byte("bytes"))

	var listed []map[string]any
	if code := get(t, srv, "/api/v1/workspaces/demo/figures", &listed); code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d figures, want 1", len(listed))
	}
	if listed[0]["id"] != id {
		t.Errorf("id = %v, want %s", listed[0]["id"], id)
	}
	if listed[0]["caption"] != "Figure 1: revenue by quarter" {
		t.Errorf("caption = %v", listed[0]["caption"])
	}
	if listed[0]["url"] != "/api/v1/workspaces/demo/figures/"+id {
		t.Errorf("url = %v", listed[0]["url"])
	}
	if listed[0]["source"] != sourceKey {
		t.Errorf("source = %v, want %s", listed[0]["source"], sourceKey)
	}
}

// TestFigureRowWithoutBytesIs404: storage can lose an object. The row survives
// and the honest answer to a browser is that there is nothing to show.
func TestFigureRowWithoutBytesIs404(t *testing.T) {
	srv, js, wsID := testServer(t)
	sourceKey := string(diff.DocKey(diff.UploadOrigin("report.pdf")))
	id := seedFigure(t, js, wsID, sourceKey, "sha-chart", []byte("bytes"))

	if err := lastMemBlobs.Delete(context.Background(), blob.FigureKey(wsID, "sha-chart")); err != nil {
		t.Fatal(err)
	}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/figures/"+id, "", nil, nil); code != http.StatusNotFound {
		t.Errorf("figure with missing bytes = %d, want 404", code)
	}
}
