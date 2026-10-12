package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/connector/upload"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
)

// blankPDF is a one-page PDF with no text; see the jobs package's copy.
const blankPDF = "%PDF-1.4\n" +
	"1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
	"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
	"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]>>endobj\n" +
	"trailer<</Root 1 0 R>>\n%%EOF\n"

// popplerShims puts a pdftotext and a pdfimages on PATH that behave by file
// name: a path containing "bad" extracts to a lone form feed (a page with no
// text); one containing "nasty" fails with stderr that quotes the staged path,
// a NUL, an invalid byte, a bidi override and a URL carrying a token; anything
// else is prose. CI has no poppler, so the real extractor runs against these.
func popplerShims(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	shim := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shim("pdftotext", `case "$4" in
  *nasty*) printf 'see https://files.test/p?token=SECRET \000 \377 \342\200\256x cannot open %s\nsecond line\n' "$4" >&2; exit 1 ;;
  *bad*)   printf '\f' ;;
  *)       printf 'Real text from a readable document.\n\n\f' ;;
esac
`)
	shim("pdfimages", `printf 'page num type width height\n'
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// debugLogger is bufferLogger at Debug, so a repeat's line can be seen.
func debugLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// filesWorker wires a loopWorker whose pinned connector is an upload
// connector in files mode over the given rows, with the bytes in memBlobs.
func filesWorker(t *testing.T, st *loopStore, files map[string]string) (*Worker, *bytes.Buffer) {
	t.Helper()
	popplerShims(t)
	w, _ := loopWorker(t, st, &pipelineStore{}, agent.FakeOptions{})
	st.pinned = &store.ConnectorRow{ID: "c2", Kind: "upload", Name: "documents", Config: map[string]any{}}
	blobs := memBlobs{}
	st.files = nil
	for path, body := range files {
		key := "ws/ws/uploads/" + path
		blobs[key] = []byte(body)
		st.files = append(st.files, store.FileRow{
			ID: path, Path: path, BlobKey: key, SHA256: "sha-" + path, Enabled: true,
		})
	}
	w.Blobs = blobs
	var buf *bytes.Buffer
	w.Log, buf = debugLogger()
	return w, buf
}

func filesRun(id string) *store.QueuedRun {
	return &store.QueuedRun{ID: id, WorkspaceID: "ws", WorkspaceSlug: "bench", Trigger: "manual", ConnectorID: "c2"}
}

// logLines returns the log lines carrying msg.
func logLines(buf *bytes.Buffer, msg string) []string {
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			out = append(out, line)
		}
	}
	return out
}

func TestServerBuildRecordsAndLogsASkippedUploadWithoutAReview(t *testing.T) {
	st := newLoopStore()
	w, buf := filesWorker(t, st, map[string]string{"bad.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n"})

	w.process(context.Background(), filesRun("r1"))

	if len(st.failed) != 0 {
		t.Fatalf("a skipped document failed the run: %v", st.failed)
	}
	// Recorded against the staged sha, with the document that read listed so
	// a stale skip on it clears.
	if len(st.skipCalls) != 1 {
		t.Fatalf("RecordFileSkips called %d times, want once", len(st.skipCalls))
	}
	want := recordedSkips{
		skips:     []store.FileSkip{{Path: "bad.pdf", SHA256: "sha-bad.pdf", Class: upload.SkipNoText}},
		readPaths: []string{"ok.md"},
	}
	if !reflect.DeepEqual(st.skipCalls[0], want) {
		t.Errorf("RecordFileSkips(%+v), want %+v", st.skipCalls[0], want)
	}
	// One Warn naming the run, the bench, the path and the class.
	warns := logLines(buf, "document skipped")
	if len(warns) != 1 {
		t.Fatalf("skip lines = %d, want 1:\n%s", len(warns), buf.String())
	}
	for _, attr := range []string{"level=WARN", "run=r1", "workspace=bench", "path=bad.pdf", "class=no_text", `reason="extracted no text"`} {
		if !strings.Contains(warns[0], attr) {
			t.Errorf("skip line lacks %s: %s", attr, warns[0])
		}
	}
	// A scan is a fact about the document, not a question for the queue.
	if len(st.reviews) != 0 {
		t.Errorf("a skip filed a review: %v", st.reviews)
	}
	if body := scrapeMetrics(t, w); !strings.Contains(body, `kiln_documents_skipped_total{class="no_text",kind="upload"} 1`) {
		t.Errorf("metric not counted:\n%s", body)
	}
}

func TestSkipSinkErrorIsLoggedAndDoesNotFailTheRun(t *testing.T) {
	st := newLoopStore()
	st.skipErr = errors.New("db down")
	w, buf := filesWorker(t, st, map[string]string{"bad.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n"})

	w.process(context.Background(), filesRun("r1"))

	if len(st.failed) != 0 {
		t.Fatalf("a sink error failed the run: %v", st.failed)
	}
	errs := logLines(buf, "recording document skips failed; the Ingest page will not show why they were not read")
	if len(errs) != 1 {
		t.Fatalf("error lines = %d, want 1:\n%s", len(errs), buf.String())
	}
	for _, attr := range []string{"level=ERROR", "run=r1", "workspace=bench", "count=1", `error="db down"`} {
		if !strings.Contains(errs[0], attr) {
			t.Errorf("error line lacks %s: %s", attr, errs[0])
		}
	}
	// Without an answer from the store the skip is still news: logged loud.
	if warns := logLines(buf, "document skipped"); len(warns) != 1 || !strings.Contains(warns[0], "level=WARN") {
		t.Errorf("skip lines after a sink error = %v, want one Warn", warns)
	}
}

func TestRepeatedSkipLogsAtDebugAndIsNotCountedAgain(t *testing.T) {
	st := newLoopStore()
	w, buf := filesWorker(t, st, map[string]string{"bad.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n"})

	w.process(context.Background(), filesRun("r1"))
	buf.Reset()
	w.process(context.Background(), filesRun("r2"))

	lines := logLines(buf, "document skipped")
	if len(lines) != 1 || !strings.Contains(lines[0], "level=DEBUG") || !strings.Contains(lines[0], "run=r2") {
		t.Errorf("second run's skip lines = %v, want one Debug line", lines)
	}
	if body := scrapeMetrics(t, w); !strings.Contains(body, `kiln_documents_skipped_total{class="no_text",kind="upload"} 1`) {
		t.Errorf("a repeat was counted again:\n%s", body)
	}
}

// scansWithOK is a bench of n blank scans (bad01.pdf ...) and one readable
// document.
func scansWithOK(n int) map[string]string {
	files := map[string]string{"ok.md": "# Fine\n\nReadable.\n"}
	for i := 1; i <= n; i++ {
		files[fmt.Sprintf("bad%02d.pdf", i)] = blankPDF
	}
	return files
}

// addFile uploads one more document to a filesWorker's bench between runs.
func addFile(t *testing.T, w *Worker, st *loopStore, path, body string) {
	t.Helper()
	blobs, ok := w.Blobs.(memBlobs)
	if !ok {
		t.Fatalf("worker blobs are %T, want memBlobs", w.Blobs)
	}
	key := "ws/ws/uploads/" + path
	blobs[key] = []byte(body)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.files = append(st.files, store.FileRow{ID: path, Path: path, BlobKey: key, SHA256: "sha-" + path, Enabled: true})
}

func TestSkipLogIsCappedPerRun(t *testing.T) {
	st := newLoopStore()
	w, buf := filesWorker(t, st, scansWithOK(25))

	w.process(context.Background(), filesRun("r1"))

	if n := len(logLines(buf, "document skipped")); n != skipLogLines {
		t.Errorf("skip lines = %d, want the cap of %d", n, skipLogLines)
	}
	// The summary says what it folded, by class, so an operator reading at
	// the default level still learns what kind of skips they were.
	more := logLines(buf, "more documents skipped")
	if len(more) != 1 || !strings.Contains(more[0], "count=5") || !strings.Contains(more[0], `classes="no_text=5"`) {
		t.Errorf("summary line = %v, want one with count=5 and classes no_text=5", more)
	}
	// Every skip was recorded, cap or not: the cap is on the log.
	if len(st.skipCalls) != 1 || len(st.skipCalls[0].skips) != 25 {
		t.Errorf("recorded skips = %d, want 25", len(st.skipCalls[0].skips))
	}
}

// A new scan is news and goes first, however many known scans sort before it:
// the cap must never fold the one line that says something happened.
func TestNewSkipIsLoggedBeforeRepeats(t *testing.T) {
	st := newLoopStore()
	w, buf := filesWorker(t, st, scansWithOK(22))
	w.process(context.Background(), filesRun("r1"))
	buf.Reset()

	// Sorts after every recorded scan, so without the ordering it is the 23rd
	// line and falls into the summary.
	addFile(t, w, st, "bad99-new.pdf", blankPDF)
	w.process(context.Background(), filesRun("r2"))

	lines := logLines(buf, "document skipped")
	if len(lines) != skipLogLines {
		t.Fatalf("skip lines = %d, want the cap of %d:\n%s", len(lines), skipLogLines, buf.String())
	}
	if !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "path=bad99-new.pdf") {
		t.Errorf("first line = %s, want the new scan at Warn", lines[0])
	}
	warns := 0
	for _, l := range lines {
		if strings.Contains(l, "level=WARN") {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("Warn lines = %d, want 1: only the new scan is news", warns)
	}
	more := logLines(buf, "more documents skipped")
	if len(more) != 1 || !strings.Contains(more[0], "count=3") || !strings.Contains(more[0], `classes="no_text=3"`) {
		t.Errorf("summary line = %v, want count=3 of no_text", more)
	}
}

// At the default level a repeat produces no line, so it must not spend the
// cap either: a bench of twenty-five stable scans used to raise "more
// documents skipped" on every rebuild while naming nothing, and a new scan
// behind them was never named at all.
func TestRepeatsSpendNoLogCapAtTheDefaultLevel(t *testing.T) {
	st := newLoopStore()
	w, _ := filesWorker(t, st, scansWithOK(25))
	w.process(context.Background(), filesRun("r1"))

	var buf bytes.Buffer
	w.Log = slog.New(slog.NewTextHandler(&buf, nil))
	w.process(context.Background(), filesRun("r2"))
	if lines := logLines(&buf, "document skipped"); len(lines) != 0 {
		t.Errorf("a rebuild of stable scans logged %d skip lines at Info", len(lines))
	}
	if more := logLines(&buf, "more documents skipped"); len(more) != 0 {
		t.Errorf("a rebuild of stable scans raised a summary: %v", more)
	}

	// The red's reproduction: one new scan behind the repeats.
	addFile(t, w, st, "bad99-new.pdf", blankPDF)
	buf.Reset()
	w.process(context.Background(), filesRun("r3"))
	lines := logLines(&buf, "document skipped")
	if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "path=bad99-new.pdf") {
		t.Errorf("skip lines = %v, want exactly one Warn naming bad99-new.pdf", lines)
	}
	if more := logLines(&buf, "more documents skipped"); len(more) != 0 {
		t.Errorf("one new scan raised a summary: %v", more)
	}
}

// An upload connector reading a worker-local folder has no file rows, so the
// store cannot say what is news; the worker remembers instead, or every
// rebuild of a folder with one scan would Warn and count it again.
func TestPathModeSkipIsLoggedAndCountedOnce(t *testing.T) {
	popplerShims(t)
	dir := t.TempDir()
	for name, body := range map[string]string{"bad.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := newLoopStore()
	w, _ := loopWorker(t, st, &pipelineStore{}, agent.FakeOptions{})
	st.pinned = &store.ConnectorRow{ID: "c2", Kind: "upload", Name: "folder", Config: map[string]any{"path": dir}}
	w.PermittedSourceRoots = []string{dir}
	var buf *bytes.Buffer
	w.Log, buf = debugLogger()

	w.process(context.Background(), filesRun("r1"))
	w.process(context.Background(), filesRun("r2"))

	lines := logLines(buf, "document skipped")
	if len(lines) != 2 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[1], "level=DEBUG") {
		t.Errorf("skip lines = %v, want one Warn then one Debug", lines)
	}
	if len(st.skipCalls) != 0 {
		t.Errorf("a path-mode connector has no rows to record on, yet RecordFileSkips was called: %+v", st.skipCalls)
	}
	if body := scrapeMetrics(t, w); !strings.Contains(body, `kiln_documents_skipped_total{class="no_text",kind="upload"} 1`) {
		t.Errorf("a repeat of an unrecorded skip was counted again:\n%s", body)
	}
}

// The seen set is bounded: a worker that outlives thousands of distinct
// unrecorded skips starts over rather than growing, and a skip the reset
// forgot is news again.
func TestSkipSeenSetIsBounded(t *testing.T) {
	w := &Worker{}
	first := skipSeenKey{workspace: "ws", kind: "upload", subject: "scan-0.pdf", class: "no_text"}
	if !w.markSkipSeen(first) {
		t.Fatal("a never-seen skip is not news")
	}
	if w.markSkipSeen(first) {
		t.Fatal("a skip seen a moment ago is news again")
	}
	for i := 1; i < 3*maxSkipSeen; i++ {
		w.markSkipSeen(skipSeenKey{workspace: "ws", kind: "upload", subject: fmt.Sprintf("scan-%d.pdf", i), class: "no_text"})
	}
	if n := len(w.skipSeen.seen); n > maxSkipSeen {
		t.Errorf("seen set grew to %d (> %d)", n, maxSkipSeen)
	}
	if !w.markSkipSeen(first) {
		t.Error("the first skip is still seen after the set was reset")
	}
}

func TestSkipLogSanitisesTheExtractorText(t *testing.T) {
	st := newLoopStore()
	w, buf := filesWorker(t, st, map[string]string{"nasty.pdf": blankPDF, "ok.md": "# Fine\n\nReadable.\n"})

	w.process(context.Background(), filesRun("r1"))

	warns := logLines(buf, "document skipped")
	if len(warns) != 1 || !strings.Contains(warns[0], "class=extractor_error") {
		t.Fatalf("skip lines = %v, want one extractor_error", warns)
	}
	line := warns[0]
	// Raw and as slog's text handler would escape them.
	for _, leak := range []string{"SECRET", "kiln-files-", os.TempDir(), "second line",
		"\x00", `\x00`, "\u202e", `\u202e`, "\ufffd", `\ufffd`, `\xff`} {
		if strings.Contains(line, leak) {
			t.Errorf("log line carries %q: %s", leak, line)
		}
	}
	// What survives: the host of the URL and the document's own name.
	for _, keep := range []string{"https://files.test", "cannot open nasty.pdf"} {
		if !strings.Contains(line, keep) {
			t.Errorf("log line lost %q: %s", keep, line)
		}
	}
	// The class token is what was persisted, never the text.
	if got := st.skipCalls[0].skips; len(got) != 1 || got[0].Class != upload.SkipExtractorError {
		t.Errorf("recorded %+v, want the class token only", got)
	}
}

func TestSanitizeSkipRaw(t *testing.T) {
	jail := filepath.Join(os.TempDir(), "kiln-files-123")
	raw := "extract: a.pdf (pdftotext): exit status 1 (see https://u:p@files.test/p?token=SECRET \x00 \xff \u202ex" +
		" cannot open " + jail + "/sub/a.pdf and " + filepath.Join(os.TempDir(), "kiln-docs-9", "sub", "a.md") +
		"\tmore\nsecond line"
	got := sanitizeSkipRaw(raw, jail)
	want := "extract: a.pdf (pdftotext): exit status 1 (see https://files.test x cannot open sub/a.pdf and sub/a.md more"
	if got != want {
		t.Errorf("sanitizeSkipRaw =\n%q\nwant\n%q", got, want)
	}
	if long := sanitizeSkipRaw(strings.Repeat("é", 400), ""); len([]rune(long)) != maxSkipRawRunes {
		t.Errorf("cap = %d runes, want %d", len([]rune(long)), maxSkipRawRunes)
	}
}

func TestWebSkipsAreLoggedWithTheHostAndNeverPersisted(t *testing.T) {
	st := newLoopStore()
	w := &Worker{Store: st}
	log, buf := debugLogger()
	staged := map[string]string{"ok.md": "sha-ok"}
	sink := w.docSkipSink(filesRun("r1"), jobs.SourceConnectors{Web: "cw"}, "", staged, log.With("run", "r1", "workspace", "bench"))

	sink(context.Background(), []jobs.DocSkip{{
		Kind: "web", Subject: "https://example.com", Class: upload.SkipExtractorError,
		Raw: "fetch: 404 Not Found",
	}})

	lines := logLines(buf, "page skipped")
	if len(lines) != 1 {
		t.Fatalf("web skip lines = %v, want 1", lines)
	}
	for _, attr := range []string{"level=WARN", "run=r1", "workspace=bench", "connector=cw", "host=https://example.com", "class=extractor_error"} {
		if !strings.Contains(lines[0], attr) {
			t.Errorf("web line lacks %s: %s", attr, lines[0])
		}
	}
	// The one write clears the read upload's stale skip and records nothing
	// for the page: there is no row to record it on.
	if len(st.skipCalls) != 1 || len(st.skipCalls[0].skips) != 0 || !reflect.DeepEqual(st.skipCalls[0].readPaths, []string{"ok.md"}) {
		t.Errorf("RecordFileSkips calls = %+v, want one with no skips and ok.md read", st.skipCalls)
	}
	if body := scrapeMetrics(t, w); !strings.Contains(body, `kiln_documents_skipped_total{class="extractor_error",kind="web"} 1`) {
		t.Errorf("web skip not counted:\n%s", body)
	}

	// The same page on the next rebuild is a repeat: Debug, and not counted.
	buf.Reset()
	sink(context.Background(), []jobs.DocSkip{{
		Kind: "web", Subject: "https://example.com", Class: upload.SkipExtractorError,
		Raw: "fetch: 404 Not Found",
	}})
	if lines := logLines(buf, "page skipped"); len(lines) != 1 || !strings.Contains(lines[0], "level=DEBUG") {
		t.Errorf("repeat web skip lines = %v, want one Debug line", lines)
	}
	if body := scrapeMetrics(t, w); !strings.Contains(body, `kiln_documents_skipped_total{class="extractor_error",kind="web"} 1`) {
		t.Errorf("a repeated web skip was counted again:\n%s", body)
	}
}

func TestSkipSinkRunsOnADetachedContext(t *testing.T) {
	// The build's context may already be canceled when the sink runs (a
	// drain that landed during the sync); the record must still be written.
	st := newLoopStore()
	w := &Worker{Store: st}
	log, _ := debugLogger()
	sink := w.docSkipSink(filesRun("r1"), jobs.SourceConnectors{}, "", map[string]string{"bad.pdf": "sha"}, log)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink(ctx, []jobs.DocSkip{{Kind: "upload", Subject: "bad.pdf", Class: upload.SkipNoText}})

	if len(st.skipCalls) != 1 {
		t.Fatalf("RecordFileSkips calls = %d, want 1", len(st.skipCalls))
	}
}
