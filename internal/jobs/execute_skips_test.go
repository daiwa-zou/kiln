package jobs

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/connector/upload"
	webconn "github.com/daiwa-zou/kiln/internal/connector/web"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
)

// blankPDF is a one-page PDF with no text, the shape of a scanned document
// once the image is set aside. Hand-made so the repository carries no binary
// fixture: a header, a catalog, a page tree, one empty page and a trailer.
const blankPDF = "%PDF-1.4\n" +
	"1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
	"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
	"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]>>endobj\n" +
	"trailer<</Root 1 0 R>>\n%%EOF\n"

// popplerShims puts a pdftotext and a pdfimages on PATH that behave by file
// name, so the upload connector's real extractor runs without poppler (CI has
// none) and the test decides the outcome: a path containing "bad" extracts to a
// lone form feed, which is what pdftotext prints for a page with no text; one
// containing "nasty" fails with stderr that quotes the staged path, a NUL, an
// invalid byte, a bidi override and a URL with a token; anything else is prose.
// pdfimages fails for a path containing "nofig", so that document reads but its
// figures do not.
func popplerShims(t *testing.T, exclusive bool) {
	t.Helper()
	dir := t.TempDir()
	shim := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// pdftotext -layout -enc UTF-8 <file> -
	shim("pdftotext", `case "$4" in
  *nasty*) printf 'see https://files.test/p?token=SECRET \000 \377 \342\200\256x cannot open %s\nsecond line\n' "$4" >&2; exit 1 ;;
  *bad*)   printf '\f' ;;
  *)       printf 'Real text from a readable document.\n\n\f' ;;
esac
`)
	// pdfimages -list <file>
	shim("pdfimages", `case "$2" in
  *nofig*) echo "Syntax Error: broken image stream" >&2; exit 1 ;;
  *)       printf 'page num type width height color comp bpc enc interp object ID x-ppi y-ppi size ratio\n' ;;
esac
`)
	path := dir
	if !exclusive {
		path = dir + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	t.Setenv("PATH", path)
}

// executeDocs runs a docs-only dry run through Execute with the hook set and
// returns what the hook received. A dry run syncs and maps everything a build
// does and stops before the agent, which is where the skips are decided.
func executeDocs(t *testing.T, docs string, progress io.Writer) (received [][]DocSkip, err error) {
	t.Helper()
	p := testPipeline(newMemStore(), newScriptedRunner())
	_, err = p.Execute(context.Background(), ExecuteRequest{
		RunID: "run-skips", WorkspaceID: "ws-1", Trigger: "manual",
		Source:   SourceSpec{DocsDir: docs, Slug: "bench"},
		DryRun:   true,
		Progress: progress,
		OnDocSkips: func(_ context.Context, skips []DocSkip) {
			received = append(received, skips)
		},
	})
	return received, err
}

func TestExecuteDeliversTypedSkipsToTheHook(t *testing.T) {
	popplerShims(t, false)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"bad.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n"})

	// Progress nil is the server's call: the narration goes to io.Discard,
	// and the hook is the only way the skip gets out.
	received, err := executeDocs(t, docs, nil)
	if err != nil {
		t.Fatalf("a skipped document failed the run: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("OnDocSkips called %d times, want once", len(received))
	}
	want := []DocSkip{{Kind: connector.KindUpload, Subject: "bad.pdf", Class: upload.SkipNoText, Raw: "extracted no text"}}
	if !reflect.DeepEqual(received[0], want) {
		t.Errorf("skips = %+v, want %+v", received[0], want)
	}
}

func TestExecuteHookIsCalledOnceEvenWhenNothingWasSkipped(t *testing.T) {
	// A sink clears what an earlier build recorded, so it must hear "nothing"
	// too, and hear it exactly once per run.
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"ok.md": "# Fine\n\nReadable.\n"})
	received, err := executeDocs(t, docs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || len(received[0]) != 0 {
		t.Errorf("OnDocSkips calls = %+v, want one empty call", received)
	}
}

func TestExecuteWithoutTheHookStillDiscardsNarration(t *testing.T) {
	// The CLI-less path before this change: no Progress, no hook. It must
	// stay a working build, not a nil call.
	popplerShims(t, false)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"bad.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n"})
	p := testPipeline(newMemStore(), newScriptedRunner())
	if _, err := p.Execute(context.Background(), ExecuteRequest{
		RunID: "r", WorkspaceID: "ws-1", Trigger: "manual",
		Source: SourceSpec{DocsDir: docs, Slug: "bench"}, DryRun: true,
	}); err != nil {
		t.Fatalf("Execute without a hook: %v", err)
	}
}

func TestExecuteStillNarratesSkipsForTheCLI(t *testing.T) {
	// kiln build reads the progress text; the typed hook is in addition to
	// it, so the line it has always printed is byte-for-byte the same.
	popplerShims(t, false)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"bad.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n"})
	var progress bytes.Buffer
	if _, err := executeDocs(t, docs, &progress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "  skipped bad.pdf: extracted no text\n") {
		t.Errorf("progress lost the skip line:\n%s", progress.String())
	}
}

func TestExecuteDoesNotDeliverAPartialSkipForADocumentThatProducedAPage(t *testing.T) {
	// Figures that could not be read are reported to the CLI, but the
	// document still has a page; a sink that heard about it would mark a
	// readable document as unread.
	popplerShims(t, false)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{"report-nofig.pdf": blankPDF, "bad.pdf": blankPDF})
	var progress bytes.Buffer
	received, err := executeDocs(t, docs, &progress)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), "skipped report-nofig.pdf: figures could not be read") {
		t.Fatalf("the partial skip is no longer narrated:\n%s", progress.String())
	}
	if len(received) != 1 || len(received[0]) != 1 || received[0][0].Subject != "bad.pdf" {
		t.Errorf("hook received %+v, want only bad.pdf", received)
	}
}

func TestExecuteClassifiesSkips(t *testing.T) {
	// PATH holds the shims and nothing else, so pandoc is missing for
	// certain: the classes a server build persists come from errors.Is on the
	// extract sentinels and must not depend on what the host has installed.
	popplerShims(t, true)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{
		"scan-bad.pdf":  blankPDF,
		"deck.docx":     "not really a docx",
		"nasty.pdf":     blankPDF,
		"ok.md":         "# Fine\n\nReadable.\n",
		"sub/other.pdf": "prose",
	})
	received, err := executeDocs(t, docs, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]DocSkip{}
	for _, s := range received[0] {
		got[s.Subject] = s
	}
	for path, class := range map[string]string{
		"scan-bad.pdf": upload.SkipNoText,
		"deck.docx":    upload.SkipExtractorMissing,
		"nasty.pdf":    upload.SkipExtractorError,
	} {
		if got[path].Class != class {
			t.Errorf("%s: class = %q, want %q (%+v)", path, got[path].Class, class, got[path])
		}
	}
	if _, ok := got["sub/other.pdf"]; ok {
		t.Error("a readable PDF was reported skipped")
	}
	// Raw is the first line as the converter wrote it: the URL with its token
	// and the staging path it quotes are both for the sink to reduce, so
	// there is one place that decides what a log line may carry.
	raw := got["nasty.pdf"].Raw
	if strings.Contains(raw, "second line") || !strings.Contains(raw, "https://files.test/p?token=SECRET") {
		t.Errorf("raw = %q, want the first line as written", raw)
	}
}

// The counts reach the overview through the build request, for the worker and
// the CLI alike (both go through Execute).
func TestBuildOverviewAccountsForSkippedUploads(t *testing.T) {
	store := newMemStore()
	runner := newScriptedRunner()
	runner.filesByAttempt[0] = map[string]string{"entities/ripple.md": validPage("entity", "Ripple")}
	p := testPipeline(store, runner)
	m := testMap(mapper.Unit{Key: "module:ripple", Slug: "ripple", Hash: "h"})
	req := testRequest(t, m, diff.ChangeSet{FullRebuild: true})
	req.UploadsStaged, req.UploadsSkipped = 3, 1

	if _, err := p.Build(context.Background(), req); err != nil {
		t.Fatalf("Build: %v", err)
	}
	imp := store.lastImport()
	if imp == nil {
		t.Fatal("nothing was imported")
	}
	if !strings.Contains(imp.Overview, "compiled from 1 code module and 2 of 3 uploaded documents, at `abc1234`; 1 could not be read (see Ingest).") {
		t.Errorf("overview does not account for the skipped upload:\n%s", imp.Overview)
	}
}

// The overview's "N of M uploaded documents" comes from the sync: every
// document listed was read or skipped, and a partial skip (figures only) is
// a read.
func TestMaterializeCountsUploadsForTheOverview(t *testing.T) {
	popplerShims(t, false)
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{
		"bad.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n", "report-nofig.pdf": blankPDF,
	})
	mat, err := materialize(context.Background(), io.Discard, SourceSpec{DocsDir: docs, Slug: "bench"})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer mat.Close()
	if mat.UploadsStaged != 3 || mat.UploadsSkipped != 1 {
		t.Errorf("uploads staged/skipped = %d/%d, want 3/1", mat.UploadsStaged, mat.UploadsSkipped)
	}
}

func TestDeliverableSkipsCollapsesToTheLastEntryPerPath(t *testing.T) {
	skipped := []docSkipped{
		{subject: "a.pdf", skip: DocSkip{Subject: "a.pdf", Class: upload.SkipExtractorError, Partial: true}},
		{subject: "a.pdf", skip: DocSkip{Subject: "a.pdf", Class: upload.SkipNoText}},
		{subject: "b.pdf", skip: DocSkip{Subject: "b.pdf", Class: upload.SkipExtractorError}},
		{subject: "b.pdf", skip: DocSkip{Subject: "b.pdf", Class: upload.SkipTimedOut}},
		{subject: "read.pdf", skip: DocSkip{Subject: "read.pdf", Class: upload.SkipNoText}},
		// A partial skip is never delivered, whatever else is known about
		// the document: it says figures were lost, not that the text was.
		{subject: "figs.pdf", skip: DocSkip{Subject: "figs.pdf", Class: upload.SkipExtractorError, Partial: true}},
	}
	docs := []docmap.Doc{{Origin: "read.pdf"}}
	got := deliverableSkips(skipped, docs)
	want := []DocSkip{
		{Subject: "a.pdf", Class: upload.SkipNoText},
		{Subject: "b.pdf", Class: upload.SkipTimedOut},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deliverableSkips = %+v, want %+v", got, want)
	}
}

// stubWebConnector stands in for the web connector, whose address guard
// refuses the loopback httptest listens on (see stubDocConnector). It fetches
// the configured URLs itself and reports a non-200 the way fetchOne does, so
// the skip that reaches webPayload is a real 404 from a real server.
type stubWebConnector struct{}

const stubWebKind = "test-jobs-web"

func (stubWebConnector) Kind() string { return stubWebKind }
func (stubWebConnector) Sync(ctx context.Context, cfg connector.Config, _ string) (*connector.SourceSet, error) {
	p := &webconn.Payload{}
	for _, u := range webconn.URLsFrom(cfg) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			p.Skipped = append(p.Skipped, webconn.Skip{URL: u, Reason: "fetch: " + res.Status})
			continue
		}
		p.Docs = append(p.Docs, docmap.Doc{
			Key: string(diff.DocKey(diff.WebOrigin(u))), Path: "page.md", Title: "Page", Origin: u,
			Text: "# Page\n\nFetched prose.\n", Hash: "h",
		})
	}
	return &connector.SourceSet{Kind: stubWebKind, Native: p}, nil
}

func init() { connector.Register(stubWebConnector{}) }

func TestWebSkipsReachTheMaterialWithHostOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "<h1>Page</h1>")
	}))
	defer srv.Close()

	var progress bytes.Buffer
	res, err := syncDocSource(context.Background(), &progress, docSource{
		kind: stubWebKind, subject: "2 url(s)", prefix: "kiln-web-test-", noun: "page(s)",
		config:  connector.Config{"urls": []any{srv.URL + "/page", srv.URL + "/missing?token=SECRET"}},
		origin:  diff.WebOrigin,
		payload: webPayload,
	})
	if err != nil {
		t.Fatalf("syncDocSource: %v", err)
	}
	t.Cleanup(func() { (&material{staging: []string{res.Staging}}).Close() })

	m := &material{Map: fakeTestMap(), Router: diff.Router{DocPaths: map[string]diff.Key{}}}
	if err := m.absorb("", res); err != nil {
		t.Fatal(err)
	}
	want := []DocSkip{{Kind: connector.KindWeb, Subject: srv.URL, Class: upload.SkipExtractorError, Raw: "fetch: 404 Not Found"}}
	if !reflect.DeepEqual(m.Skips, want) {
		t.Errorf("material skips = %+v, want %+v", m.Skips, want)
	}
	// The CLI line keeps the full URL it has always printed.
	if !strings.Contains(progress.String(), "skipped "+srv.URL+"/missing?token=SECRET: fetch: 404 Not Found") {
		t.Errorf("progress line changed:\n%s", progress.String())
	}
}

func TestRedactURLsKeepsSchemeAndHostOnly(t *testing.T) {
	for in, want := range map[string]string{
		"fetch https://user:pw@example.com/path?token=SECRET failed": "fetch https://example.com failed",
		"GET http://127.0.0.1:8080/missing?x=1 (404)":                "GET http://127.0.0.1:8080 (404)",
		"no url here": "no url here",
		"two https://a.test/1 and https://b.test/2?k=v": "two https://a.test and https://b.test",
	} {
		if got := RedactURLs(in); got != want {
			t.Errorf("RedactURLs(%q) = %q, want %q", in, got, want)
		}
	}
}
