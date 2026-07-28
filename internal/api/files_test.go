package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

// memBlobs is an in-memory blob.Store so upload tests need no filesystem or
// object store; tests reach into it to assert which blobs survived.
type memBlobs struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

// lastMemBlobs is the most recently built store; the shared harnesses have a
// fixed signature, so file tests reach storage through this hook.
var lastMemBlobs *memBlobs

func newMemBlobs() *memBlobs {
	m := &memBlobs{blobs: map[string][]byte{}}
	lastMemBlobs = m
	return m
}

func (m *memBlobs) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[key] = data
	return nil
}

func (m *memBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[key]
	if !ok {
		return nil, fmt.Errorf("memblobs: %s not found", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memBlobs) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, key)
	return nil
}

func (m *memBlobs) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.blobs)
}

// upload issues a multipart POST with one file part (preceded by an optional
// path field), returning the status code and decoded body.
func upload(t *testing.T, srv *httptest.Server, workspace, token, relPath, fileName string, content []byte) (int, map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if relPath != "" {
		if err := mw.WriteField("path", relPath); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/workspaces/"+workspace+"/files", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestFileUploadRoundTrip(t *testing.T) {
	srv, _, _ := testServer(t)
	blobs := lastMemBlobs

	content := []byte("# Design notes\n\nThe kiln fires at cone 6.\n")
	code, body := upload(t, srv, "demo", "", "notes/design.md", "design.md", content)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%v), want 201", code, body)
	}
	if body["path"] != "notes/design.md" {
		t.Errorf("path = %v", body["path"])
	}
	wantSum := sha256.Sum256(content)
	if body["sha256"] != hex.EncodeToString(wantSum[:]) {
		t.Errorf("sha256 = %v", body["sha256"])
	}
	if body["replaced"] != false {
		t.Errorf("replaced = %v, want false", body["replaced"])
	}
	if body["build"] != "pending" {
		t.Errorf("build = %v, want pending (no build-on-change connector)", body["build"])
	}
	if blobs.count() != 1 {
		t.Fatalf("blob count = %d, want 1", blobs.count())
	}

	var listed []map[string]any
	if code := get(t, srv, "/api/v1/workspaces/demo/files", &listed); code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if len(listed) != 1 || listed[0]["path"] != "notes/design.md" {
		t.Fatalf("list = %v", listed)
	}
	id, _ := listed[0]["id"].(string)

	// Replacing the same path reports the supersession and drops the old blob.
	code, body = upload(t, srv, "demo", "", "notes/design.md", "design.md", []byte("second firing"))
	if code != http.StatusCreated || body["replaced"] != true {
		t.Fatalf("replace = %d replaced=%v, want 201/true", code, body["replaced"])
	}
	if blobs.count() != 1 {
		t.Fatalf("blob count after replace = %d, want 1", blobs.count())
	}

	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/files/"+id, "", nil, nil); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	if blobs.count() != 0 {
		t.Fatalf("blob count after delete = %d, want 0", blobs.count())
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/files/"+id, "", nil, nil); code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", code)
	}
}

func TestFileUploadValidation(t *testing.T) {
	srv, _, _ := testServer(t)

	// Unsupported extension.
	if code, body := upload(t, srv, "demo", "", "", "malware.exe", []byte("x")); code != http.StatusBadRequest {
		t.Errorf("exe upload = %d (%v), want 400", code, body)
	}
	// Traversal in the declared path.
	if code, _ := upload(t, srv, "demo", "", "../../etc/cron.md", "x.md", []byte("x")); code != http.StatusBadRequest {
		t.Errorf("traversal upload = %d, want 400", code)
	}
	// Absolute path.
	if code, _ := upload(t, srv, "demo", "", "/etc/notes.md", "x.md", []byte("x")); code != http.StatusBadRequest {
		t.Errorf("absolute-path upload = %d, want 400", code)
	}
	// No file part at all.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/workspaces/demo/files",
		strings.NewReader("--x--"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("empty multipart = %d, want 400", res.StatusCode)
	}
}

func TestFileUploadTooLarge(t *testing.T) {
	srv, _, _ := testServer(t)
	// One byte over the request ceiling; the MaxBytesReader trips mid-stream.
	huge := bytes.Repeat([]byte("a"), maxUploadBytes+1)
	code, _ := upload(t, srv, "demo", "", "", "huge.txt", huge)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload = %d, want 413", code)
	}
}

func TestFileUploadUnavailableWithoutBlobStore(t *testing.T) {
	// testServer migrates the schema and seeds the workspace; the bare server
	// shares its store but has no blob store wired.
	_, js, _ := testServer(t)
	bare := httptest.NewServer((&Server{
		Store: js, Writes: js, Runs: js, Admin: js, Files: js,
	}).Router())
	defer bare.Close()
	code, body := upload(t, bare, "demo", "", "", "notes.md", []byte("x"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("upload without blobs = %d (%v), want 503", code, body)
	}
}

func TestFileUploadAuthorizationByRole(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)

	viewer := addMember(t, pool, wsID, "vera-files", "viewer")
	member := addMember(t, pool, wsID, "mira-files", "member")
	outsider := addMember(t, pool, wsID, "otto-files", "")

	src.id = auth.Identity{UserID: viewer, Scopes: []string{"read", "write"}}
	if code, _ := upload(t, srv, "demo", "kiln_valid", "", "a.md", []byte("x")); code != http.StatusForbidden {
		t.Errorf("viewer upload = %d, want 403", code)
	}
	// Viewers still read the file list.
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/files", "kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("viewer list = %d, want 200", code)
	}

	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}
	code, body := upload(t, srv, "demo", "kiln_valid", "", "a.md", []byte("x"))
	if code != http.StatusCreated {
		t.Errorf("member upload = %d (%v), want 201", code, body)
	}

	src.id = auth.Identity{UserID: outsider, Scopes: []string{"read", "write"}}
	if code, _ := upload(t, srv, "demo", "kiln_valid", "", "b.md", []byte("x")); code != http.StatusNotFound {
		t.Errorf("outsider upload = %d, want 404 (workspace invisible)", code)
	}

	// No token at all.
	if code, _ := upload(t, srv, "demo", "", "", "c.md", []byte("x")); code != http.StatusUnauthorized {
		t.Errorf("anonymous upload = %d, want 401", code)
	}
}

func TestFilePauseRoundTrip(t *testing.T) {
	srv, _, _ := testServer(t)

	code, body := upload(t, srv, "demo", "", "", "pausable.md", []byte("x"))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d", code)
	}
	id, _ := body["id"].(string)

	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/files/"+id, "",
		map[string]any{"enabled": false}, nil); code != http.StatusOK {
		t.Fatalf("pause = %d, want 200", code)
	}
	var listed []map[string]any
	get(t, srv, "/api/v1/workspaces/demo/files", &listed)
	if len(listed) != 1 || listed[0]["enabled"] != false {
		t.Fatalf("list after pause: %v", listed)
	}
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/files/"+id, "",
		map[string]any{"enabled": true}, nil); code != http.StatusOK {
		t.Fatalf("resume = %d", code)
	}
	// Missing enabled field is a 400, not a silent no-op.
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/files/"+id, "",
		map[string]any{}, nil); code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d, want 400", code)
	}
}

func TestFileDeleteIsWorkspaceScoped(t *testing.T) {
	srv, js, _ := testServer(t)
	ctx := context.Background()

	if _, err := js.EnsureWorkspace(ctx, "test-org", "other", "Other"); err != nil {
		t.Fatal(err)
	}
	code, body := upload(t, srv, "demo", "", "", "scoped.md", []byte("x"))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d", code)
	}
	id, _ := body["id"].(string)
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/other/files/"+id, "", nil, nil); code != http.StatusNotFound {
		t.Fatalf("cross-workspace delete = %d, want 404", code)
	}
}

func TestUploadEnqueuesBuildWhenConnectorOptsIn(t *testing.T) {
	srv, js, wsID := testServer(t)
	ctx := context.Background()

	if _, err := js.CreateConnector(ctx, store.ConnectorRow{
		WorkspaceID: wsID, Kind: "upload", Name: "documents",
		Config: map[string]any{}, TriggerMode: "webhook", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	code, body := upload(t, srv, "demo", "", "", "queued.md", []byte("x"))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%v)", code, body)
	}
	if body["build"] != "queued" {
		t.Fatalf("build = %v, want queued", body["build"])
	}
	runs, err := js.ListRuns(ctx, wsID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Trigger != "upload" {
		t.Fatalf("runs = %+v, want one upload-triggered run", runs)
	}

	// A second upload debounces onto the waiting run instead of stacking.
	if _, body := upload(t, srv, "demo", "", "", "more.md", []byte("y")); body["build"] != "queued" {
		t.Fatalf("second build = %v", body["build"])
	}
	runs, _ = js.ListRuns(ctx, wsID, 10, 0)
	if len(runs) != 1 {
		t.Fatalf("runs after second upload = %d, want 1 (debounced)", len(runs))
	}
}

func TestSanitizeUploadPath(t *testing.T) {
	good := map[string]string{
		"a.md":                "a.md",
		"docs/guide.pdf":      "docs/guide.pdf",
		"docs//guide.pdf":     "docs/guide.pdf",
		`win\style\note.md`:   "win/style/note.md",
		" trimmed.md ":        "trimmed.md",
		"./relative/notes.md": "relative/notes.md",
	}
	for in, want := range good {
		got, err := sanitizeUploadPath(in)
		if err != nil || got != want {
			t.Errorf("sanitize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "/abs.md", "../up.md", "a/../../up.md", "..", "ctrl\x00.md",
		strings.Repeat("a", maxUploadPathBytes+1)}
	for _, in := range bad {
		if _, err := sanitizeUploadPath(in); err == nil {
			t.Errorf("sanitize(%q) accepted", in)
		}
	}
}
