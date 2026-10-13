package api

// The Ingest status end to end. This lives in package api rather than in
// internal/worker because the assertions go through GET /files and the
// derivation (deriveIngest) is private to this package; the worker package is
// a dependency of the test, not of the handler, so there is no import cycle.
//
// What is real: the Postgres store, the upload endpoint, the queue, the
// worker's RunOnce, the pipeline, the upload connector and its extractor
// dispatch. What is faked: the agent (the free fake runner), the blob store (in
// memory), and poppler (CI has none), replaced by two shell shims on PATH
// whose behaviour is decided by the file's bytes.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/worker"
)

// ingestBlankPDF is a one-page PDF with no text, the shape of a scan once its
// image is set aside; hand-made so the repository carries no binary fixture.
// The trailing comment lets a test change its sha without changing what the
// shim makes of it.
func ingestBlankPDF(variant string) []byte {
	return []byte("%PDF-1.4\n" +
		"1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
		"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
		"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]>>endobj\n" +
		"trailer<</Root 1 0 R>>\n%%EOF\n%" + variant + "\n")
}

// ingestPopplerShims puts a pdftotext and a pdfimages on PATH. pdftotext
// prints prose for a file whose bytes carry the marker TEXTY and a lone form
// feed (what poppler prints for a page with no text) otherwise.
func ingestPopplerShims(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	shim := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// pdftotext -layout -enc UTF-8 <file> -
	shim("pdftotext", `if grep -q TEXTY "$4"; then
  printf 'Real text from a readable document about kilns.\n\n\f'
else
  printf '\f'
fi
`)
	// pdfimages -list <file>
	shim("pdfimages", `printf 'page num type width height color comp bpc enc interp object ID x-ppi y-ppi size ratio\n'
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// lockedBuffer is a log sink the pipeline's goroutines can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines(msg string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, line := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) || strings.Contains(line, "msg="+msg+" ") {
			out = append(out, line)
		}
	}
	return out
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestIngestStatusEndToEnd(t *testing.T) {
	ingestPopplerShims(t)
	srv, js, wsID := testServer(t)
	blobs := lastMemBlobs
	ctx := context.Background()

	// A files-mode upload connector: no path in its config, so the worker
	// stages the bench's uploaded files out of the blob store.
	connID, err := js.CreateConnector(ctx, store.ConnectorRow{
		WorkspaceID: wsID, Kind: "upload", Name: "documents", Config: map[string]any{}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateConnector: %v", err)
	}

	var logs lockedBuffer
	w := &worker.Worker{
		Store: js,
		ID:    "w-ingest",
		Log:   slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Blobs: blobs,
		Pipeline: &jobs.Pipeline{
			Store:  js,
			Runner: agent.NewFakeRunner(agent.FakeOptions{}),
			Budget: jobs.Budget{AnalyzeUSD: 0.4, PageUSD: 1, RunUSD: 4},
			Model:  "fake",
		},
		SkipSchedulerDuties: true,
	}
	build := func() {
		t.Helper()
		if _, _, err := js.EnqueueRun(ctx, wsID, "manual", connID); err != nil {
			t.Fatalf("EnqueueRun: %v", err)
		}
		n, err := w.RunOnce(ctx)
		if err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v; want one run drained", n, err)
		}
		var status, errMsg string
		if err := js.Pool().QueryRow(ctx,
			`SELECT status, coalesce(error, '') FROM runs WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT 1`,
			wsID).Scan(&status, &errMsg); err != nil {
			t.Fatalf("read last run: %v", err)
		}
		if status != "succeeded" && status != "no_changes" && status != "partial" {
			t.Fatalf("the build ended %q (%s); a skipped document must not fail it\nlogs:\n%s", status, errMsg, logs.String())
		}
	}

	type ingest struct {
		Status string  `json:"status"`
		Pages  *int    `json:"pages"`
		ReadAt *string `json:"readAt"`
		Reason *string `json:"reason"`
		Class  *string `json:"class"`
	}
	list := func() map[string]ingest {
		t.Helper()
		body := getRaw(t, srv, "/api/v1/workspaces/demo/files", "")
		var rows []struct {
			Path   string `json:"path"`
			Ingest ingest `json:"ingest"`
		}
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("decode GET /files: %v\n%s", err, body)
		}
		out := map[string]ingest{}
		for _, r := range rows {
			out[r.Path] = r.Ingest
		}
		return out
	}
	put := func(name string, content []byte) {
		t.Helper()
		if code, body := upload(t, srv, "demo", "", "", name, content); code != 201 {
			t.Fatalf("upload %s = %d (%v)", name, code, body)
		}
	}
	wantSkipped := func(when string, got map[string]ingest) {
		t.Helper()
		r := got["scan.pdf"]
		if r.Status != "skipped" || r.Class == nil || *r.Class != "no_text" ||
			r.Reason == nil || *r.Reason != skipSentences["no_text"] || r.Pages != nil {
			t.Errorf("%s: scan.pdf = %+v, want skipped / no_text with the fixed sentence", when, r)
		}
	}
	wantReadMD := func(when string, got map[string]ingest) {
		t.Helper()
		r := got["notes.md"]
		if r.Status != "read" || r.Pages == nil || *r.Pages < 1 || r.ReadAt == nil || r.Reason != nil {
			t.Errorf("%s: notes.md = %+v, want read with at least one page", when, r)
		}
	}

	put("scan.pdf", ingestBlankPDF("v1"))
	put("notes.md", []byte("# Kiln notes\n\nThe kiln fires at cone 6, and the glaze matures there.\n"))

	// Before any build both documents are waiting.
	got := list()
	for _, p := range []string{"scan.pdf", "notes.md"} {
		if got[p].Status != "waiting" {
			t.Errorf("before a build: %s = %+v, want waiting", p, got[p])
		}
	}

	// Build 1: the scan is skipped for lack of text, the note is read.
	build()
	got = list()
	wantSkipped("build 1", got)
	wantReadMD("build 1", got)
	if n := len(logs.lines("document skipped")); n != 1 {
		t.Fatalf("build 1 logged %d skip lines, want exactly one:\n%s", n, logs.String())
	}
	first := logs.lines("document skipped")[0]
	for _, attr := range []string{"level=WARN", "path=scan.pdf", "class=no_text"} {
		if !strings.Contains(first, attr) {
			t.Errorf("first skip line lacks %s: %s", attr, first)
		}
	}
	var reviews int
	if err := js.Pool().QueryRow(ctx, `SELECT count(*) FROM review_items WHERE workspace_id = $1 AND (title ILIKE '%scan%' OR detail ILIKE '%scan.pdf%')`, wsID).Scan(&reviews); err != nil {
		t.Fatalf("count reviews: %v", err)
	} else if reviews != 0 {
		t.Errorf("a skipped scan filed %d review item(s)", reviews)
	}

	// Build 2, same bytes: the skip is kept and is not news. The repeat is a
	// Debug line, and the Warn count does not move.
	build()
	got = list()
	wantSkipped("build 2", got)
	wantReadMD("build 2", got)
	lines := logs.lines("document skipped")
	if len(lines) != 2 {
		t.Fatalf("after build 2 there are %d skip lines, want 2 (one Warn, one Debug):\n%s", len(lines), logs.String())
	}
	if !strings.Contains(lines[1], "level=DEBUG") {
		t.Errorf("the repeat was not logged at Debug: %s", lines[1])
	}

	// A replacement with new bytes is a different document: waiting until a
	// build has looked at it, even though it is still blank.
	put("scan.pdf", ingestBlankPDF("v2"))
	got = list()
	if r := got["scan.pdf"]; r.Status != "waiting" || r.Class != nil || r.Reason != nil {
		t.Errorf("after replacing the bytes: scan.pdf = %+v, want waiting with no stale reason", r)
	}
	wantReadMD("after the replacement", got)

	// The text version of it, uploaded at the same path, is waiting too; the
	// build reads it and the old skip is gone with it.
	put("scan.pdf", append(ingestBlankPDF("v3"), []byte("%TEXTY\n")...))
	if r := list()["scan.pdf"]; r.Status != "waiting" {
		t.Errorf("after uploading a text version: scan.pdf = %+v, want waiting", r)
	}
	build()
	got = list()
	if r := got["scan.pdf"]; r.Status != "read" || r.Pages == nil || *r.Pages < 1 || r.Reason != nil || r.Class != nil {
		t.Errorf("after the text version was built: scan.pdf = %+v, want read with pages and no reason", r)
	}
	wantReadMD("after the text version", got)
	if n := len(logs.lines("document skipped")); n != 2 {
		t.Errorf("a readable document logged a skip: %d lines\n%s", n, logs.String())
	}

	// And a blank replacement after that is skipped again, as news: a new sha
	// is a new fact, so it logs at Warn.
	put("scan.pdf", ingestBlankPDF("v4"))
	build()
	got = list()
	wantSkipped("blank again", got)
	lines = logs.lines("document skipped")
	if len(lines) != 3 || !strings.Contains(lines[2], "level=WARN") {
		t.Errorf("a new blank file should log a fresh Warn; lines = %v", lines)
	}

	// A file the connector leaves out by name (an AppleDouble shadow, a file
	// under .obsidian) is staged like any other, so it must come back as a
	// skip with its own reason rather than read "not read, press Ingest now"
	// for ever.
	put("._shadow.pdf", append(ingestBlankPDF("v5"), []byte("%TEXTY\n")...))
	if r := list()["._shadow.pdf"]; r.Status != "waiting" {
		t.Errorf("before a build: ._shadow.pdf = %+v, want waiting", r)
	}
	build()
	got = list()
	if r := got["._shadow.pdf"]; r.Status != "skipped" || r.Class == nil || *r.Class != "hidden_path" ||
		r.Reason == nil || *r.Reason != skipSentences["hidden_path"] {
		t.Errorf("hidden upload: ._shadow.pdf = %+v, want skipped / hidden_path with the fixed sentence", r)
	}
	wantReadMD("after the hidden upload", got)
	// One fresh Warn for it, ahead of the scan's Debug repeat.
	lines = logs.lines("document skipped")
	if len(lines) != 5 || !strings.Contains(lines[3], "class=hidden_path") || !strings.Contains(lines[3], "level=WARN") ||
		!strings.Contains(lines[4], "level=DEBUG") {
		t.Errorf("the hidden upload should log one fresh Warn before the repeat; lines = %v", lines)
	}
}
