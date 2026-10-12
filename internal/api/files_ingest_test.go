package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
)

// The handler wired to the real store: a source row keyed on the upload path
// reads as read with its page count, a recorded class comes back as its
// sentence, and a viewer sees byte for byte what a member sees (the route is a
// member-level read; the reason is a fixed sentence, never extractor text).
func TestFilesListIngestThroughTheStore(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	ctx := context.Background()

	member := addMember(t, pool, wsID, "mira-ingest", "member")
	viewer := addMember(t, pool, wsID, "vera-ingest", "viewer")
	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}
	for _, name := range []string{"read.md", "scan.pdf", "odd.pdf", "later.md"} {
		if code, body := upload(t, srv, "demo", "kiln_valid", "", name, []byte("bytes of "+name)); code != http.StatusCreated {
			t.Fatalf("upload %s = %d (%v)", name, code, body)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// The sync's trace of a read document, plus a section unit sharing a page.
	exec(`INSERT INTO sources (workspace_id, key, kind, input_hash, files_written, fetched_at)
	      VALUES ($1, 'doc:upload:read.md', 'doc', 'h1', '["sources/read.md", "concepts/shared.md"]', now()),
	             ($1, 'doc:upload:read.md#intro', 'doc', 'h2', '["sources/read-intro.md", "concepts/shared.md"]', now())`, wsID)
	// What the worker's sink records: a class token against the current bytes.
	exec(`UPDATE workspace_files SET skip_reason = 'no_text', skip_sha256 = sha256, skipped_at = now()
	      WHERE workspace_id = $1 AND path = 'scan.pdf'`, wsID)
	// A token the allow-list never produced, with characters JSON must escape:
	// the API echoes it as a string and falls back to the generic sentence.
	exec(`UPDATE workspace_files SET skip_reason = $2, skip_sha256 = sha256, skipped_at = now()
	      WHERE workspace_id = $1 AND path = 'odd.pdf'`, wsID, "<b>&\n\"odd\"")

	type ingest struct {
		Status string  `json:"status"`
		Pages  *int    `json:"pages"`
		ReadAt *string `json:"readAt"`
		Reason *string `json:"reason"`
		Class  *string `json:"class"`
	}
	list := func(token string) (string, map[string]ingest) {
		t.Helper()
		body := getRaw(t, srv, "/api/v1/workspaces/demo/files", token)
		var rows []struct {
			Path   string `json:"path"`
			Ingest ingest `json:"ingest"`
		}
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("decode: %v\n%s", err, body)
		}
		byPath := map[string]ingest{}
		for _, r := range rows {
			byPath[r.Path] = r.Ingest
		}
		return body, byPath
	}

	_, got := list("kiln_valid")
	if len(got) != 4 {
		t.Fatalf("rows = %d, want 4", len(got))
	}
	if r := got["read.md"]; r.Status != "read" || r.Pages == nil || *r.Pages != 3 || r.ReadAt == nil || r.Reason != nil || r.Class != nil {
		t.Errorf("read.md = %+v, want read with 3 pages and a readAt", r)
	}
	if r := got["scan.pdf"]; r.Status != "skipped" || r.Reason == nil || *r.Reason != skipSentences["no_text"] ||
		r.Class == nil || *r.Class != "no_text" || r.Pages != nil {
		t.Errorf("scan.pdf = %+v, want skipped with the no_text sentence", r)
	}
	if r := got["odd.pdf"]; r.Status != "skipped" || r.Class == nil || *r.Class != "<b>&\n\"odd\"" ||
		r.Reason == nil || *r.Reason != skipSentences["extractor_error"] {
		t.Errorf("odd.pdf = %+v, want the token echoed and the extractor_error sentence", r)
	}
	// No ingest has run: a document nothing read is waiting, not passed over.
	if r := got["later.md"]; r.Status != "waiting" || r.Pages != nil || r.ReadAt != nil || r.Reason != nil || r.Class != nil {
		t.Errorf("later.md = %+v, want waiting with nulls", r)
	}

	// A build that began after the upload and read nothing of it: passed over,
	// cause unrecorded. The read document is unaffected.
	exec(`INSERT INTO runs (workspace_id, trigger, status, started_at) VALUES ($1, 'manual', 'succeeded', now())`, wsID)
	memberBody, got := list("kiln_valid")
	if r := got["later.md"]; r.Status != "skipped" || r.Reason == nil || *r.Reason != skipSentenceUnrecorded || r.Class != nil {
		t.Errorf("later.md after a build = %+v, want skipped with the unrecorded sentence and a null class", r)
	}
	if r := got["read.md"]; r.Status != "read" {
		t.Errorf("read.md after a build = %+v, want still read", r)
	}

	src.id = auth.Identity{UserID: viewer, Scopes: []string{"read"}}
	if viewerBody, _ := list("kiln_valid"); viewerBody != memberBody {
		t.Errorf("viewer body differs from the member's:\n%s\n%s", viewerBody, memberBody)
	}
}
