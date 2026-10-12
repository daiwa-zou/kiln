package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/naming"
	"github.com/daiwa-zou/kiln/internal/store"
)

// ingestFakeStore resolves every bench reference, so the list handler can be
// driven without a database.
type ingestFakeStore struct{ Store }

func (ingestFakeStore) ResolveWorkspace(_ context.Context, ref, _ string, _ bool) (store.WorkspaceRow, error) {
	return store.WorkspaceRow{ID: "ws-" + ref, Org: "o", Slug: ref}, nil
}

// ingestFakeFiles answers ListFilesWithIngest with canned rows and records
// the bench it was asked about.
type ingestFakeFiles struct {
	FileStore
	rows  []store.FileIngestRow
	bench store.FileIngestBench
	asked string
}

func (f *ingestFakeFiles) ListFilesWithIngest(_ context.Context, ws string) ([]store.FileIngestRow, store.FileIngestBench, error) {
	f.asked = ws
	return f.rows, f.bench, nil
}

func ingestServer(files *ingestFakeFiles) *httptest.Server {
	return httptest.NewServer((&Server{Store: ingestFakeStore{}, Files: files}).Router())
}

// getRaw fetches a path, optionally with a bearer token, and returns the
// trimmed body so tests can compare the wire shape itself.
func getRaw(t *testing.T, srv *httptest.Server, path, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, res.StatusCode, body)
	}
	return strings.TrimSpace(string(body))
}

// The response is a typed contract: the nine keys the UI already reads, with
// their old names and values, plus ingest with every member present (null
// rather than absent), so a client can read f.ingest.pages without guarding.
func TestFilesListJSONShape(t *testing.T) {
	uploaded := time.Date(2026, 10, 10, 9, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	readAt := time.Date(2026, 10, 11, 7, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	lastRun := readAt.Add(time.Minute)
	pages := 6
	class := "no_text"

	read := store.FileIngestRow{HasSource: true, Pages: &pages, ReadAt: &readAt, ContentAt: uploaded}
	read.FileRow = store.FileRow{ID: "f1", Path: "docs/a.pdf", DisplayName: "A", SizeBytes: 1234,
		SHA256: "aa", ContentType: "application/pdf", Enabled: true, CreatedAt: uploaded, UpdatedAt: uploaded}
	waiting := store.FileIngestRow{ContentAt: lastRun.Add(time.Hour)}
	waiting.FileRow = store.FileRow{ID: "f2", Path: "docs/b.xlsx", SizeBytes: 5, SHA256: "bb", Enabled: true,
		CreatedAt: lastRun.Add(time.Hour), UpdatedAt: lastRun.Add(time.Hour)}
	skipped := store.FileIngestRow{SkipClass: &class, SkipLive: true, ContentAt: uploaded}
	skipped.FileRow = store.FileRow{ID: "f3", Path: "scan.pdf", DisplayName: "Scan", SizeBytes: 7, SHA256: "cc",
		Enabled: true, CreatedAt: uploaded, UpdatedAt: uploaded}

	files := &ingestFakeFiles{rows: []store.FileIngestRow{read, waiting, skipped},
		bench: store.FileIngestBench{LastRun: &lastRun}}
	srv := ingestServer(files)
	defer srv.Close()

	body := getRaw(t, srv, "/api/v1/workspaces/demo/files", "")
	if files.asked != "ws-demo" {
		t.Errorf("handler asked for %q, want the resolved bench", files.asked)
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	wantKeys := []string{"contentType", "enabled", "id", "ingest", "name", "path", "sha256", "size", "updated", "uploaded"}
	wantIngestKeys := []string{"class", "pages", "readAt", "reason", "status"}
	for i, r := range rows {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
			t.Errorf("row %d keys = %v, want %v", i, keys, wantKeys)
		}
		var ing map[string]json.RawMessage
		if err := json.Unmarshal(r["ingest"], &ing); err != nil {
			t.Fatalf("row %d ingest: %v", i, err)
		}
		ikeys := make([]string, 0, len(ing))
		for k := range ing {
			ikeys = append(ikeys, k)
		}
		sort.Strings(ikeys)
		if strings.Join(ikeys, ",") != strings.Join(wantIngestKeys, ",") {
			t.Errorf("row %d ingest keys = %v, want %v (explicit nulls)", i, ikeys, wantIngestKeys)
		}
	}

	// The old keys, value for value, including the derived name and the
	// always-present contentType.
	for k, want := range map[string]string{
		"id": `"f1"`, "path": `"docs/a.pdf"`, "name": `"A"`, "size": `1234`, "sha256": `"aa"`,
		"contentType": `"application/pdf"`, "enabled": `true`,
		"uploaded": `"2026-10-10T07:30:00Z"`, "updated": `"2026-10-10T07:30:00Z"`,
	} {
		if got := string(rows[0][k]); got != want {
			t.Errorf("read row %s = %s, want %s", k, got, want)
		}
	}
	if got, want := string(rows[1]["name"]), `"`+naming.FromFilename("docs/b.xlsx")+`"`; got != want {
		t.Errorf("name without a display name = %s, want %s (derived from the filename)", got, want)
	}
	if got := string(rows[1]["contentType"]); got != `""` {
		t.Errorf("unset contentType = %s, want \"\" (always present)", got)
	}

	// The three states, with readAt in RFC 3339 UTC and explicit nulls.
	want := map[string]string{
		"f1": `{"status":"read","pages":6,"readAt":"2026-10-11T05:00:00Z","reason":null,"class":null}`,
		"f2": `{"status":"waiting","pages":null,"readAt":null,"reason":null,"class":null}`,
		"f3": `{"status":"skipped","pages":null,"readAt":null,"reason":"No text found, probably a scan. Upload a text version.","class":"no_text"}`,
	}
	for _, r := range rows {
		id := strings.Trim(string(r["id"]), `"`)
		if got := string(r["ingest"]); got != want[id] {
			t.Errorf("%s ingest = %s\nwant %s", id, got, want[id])
		}
	}
}

// An empty bench answers [] and never null: the UI iterates the body.
func TestFilesListEmptyIsArray(t *testing.T) {
	srv := ingestServer(&ingestFakeFiles{})
	defer srv.Close()
	if body := getRaw(t, srv, "/api/v1/workspaces/demo/files", ""); body != "[]" {
		t.Fatalf("empty list = %s, want []", body)
	}
}
