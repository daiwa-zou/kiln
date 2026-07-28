package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/store"
)

// fakeStore satisfies Store for exercising the resolution logic without
// Postgres. Claim/fail/requeue are not used by these tests.
type fakeStore struct {
	connectors []store.ConnectorRow
	byID       map[string]store.ConnectorRow
	files      []store.FileRow
}

func (f *fakeStore) ClaimNextRun(context.Context, string) (*store.QueuedRun, error) {
	return nil, nil
}
func (f *fakeStore) FailRun(context.Context, string, string) error                { return nil }
func (f *fakeStore) RequeueStaleRuns(context.Context, time.Duration) (int, error) { return 0, nil }
func (f *fakeStore) MarkConnectorSync(context.Context, string, string) error      { return nil }
func (f *fakeStore) EnabledConnectors(_ context.Context, _ string) ([]store.ConnectorRow, error) {
	return f.connectors, nil
}
func (f *fakeStore) ConnectorByID(_ context.Context, id string) (*store.ConnectorRow, error) {
	c, ok := f.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &c, nil
}
func (f *fakeStore) LoadSealedCredential(_ context.Context, id string) (*store.SealedCredential, error) {
	return nil, store.ErrNotFound
}
func (f *fakeStore) PollDueConnectors(context.Context, time.Duration) ([]store.ConnectorRow, error) {
	return nil, nil
}
func (f *fakeStore) EnqueueRun(context.Context, string, string, string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeStore) ListFiles(context.Context, string) ([]store.FileRow, error) {
	return f.files, nil
}
func (f *fakeStore) WorkspaceBudgetUSD(context.Context, string) (*float64, error) { return nil, nil }
func (f *fakeStore) LastSuccessfulRef(context.Context, string) (string, error)    { return "", nil }
func (f *fakeStore) RequeueRun(context.Context, string) error                     { return nil }
func (f *fakeStore) Sweep(context.Context, time.Duration, time.Duration) (int64, error) {
	return 0, nil
}
func (f *fakeStore) SpendInWindow(context.Context, string, time.Duration) (float64, error) {
	return 0, nil
}
func (f *fakeStore) FileReview(context.Context, string, string, string, string) error { return nil }

func run(ws string) *store.QueuedRun {
	return &store.QueuedRun{ID: "r1", WorkspaceID: ws, WorkspaceSlug: "bench", Trigger: "manual"}
}

func TestSourceSpecResolvesGitAndUpload(t *testing.T) {
	repo, docs := t.TempDir(), t.TempDir()
	w := &Worker{
		Store: &fakeStore{connectors: []store.ConnectorRow{
			{ID: "c1", Kind: "git", Name: "code", Config: map[string]any{"path": repo}},
			{ID: "c2", Kind: "upload", Name: "docs", Config: map[string]any{"path": docs}},
		}},
		PermittedSourceRoots: []string{repo, docs},
	}

	spec, ids, _, err := w.sourceSpec(context.Background(), run("ws1"))
	if err != nil {
		t.Fatalf("sourceSpec: %v", err)
	}
	if spec.Path == "" || spec.DocsDir == "" {
		t.Errorf("spec incomplete: %+v", spec)
	}
	if spec.Slug != "bench" {
		t.Errorf("slug = %q, want workspace slug", spec.Slug)
	}
	if ids.Git != "c1" || ids.Upload != "c2" {
		t.Errorf("connector ids = %+v, want git c1 + upload c2", ids)
	}
}

// memBlobs is a tiny in-memory blob store for materialization tests.
type memBlobs map[string][]byte

func (m memBlobs) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m[key] = data
	return nil
}

func (m memBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("memblobs: %s not found", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m memBlobs) Delete(_ context.Context, key string) error {
	delete(m, key)
	return nil
}

func TestSourceSpecFilesModeStagesBlobs(t *testing.T) {
	blobs := memBlobs{
		"ws/ws1/uploads/f1": []byte("# Notes\n"),
		"ws/ws1/uploads/f2": []byte("nested body"),
	}
	w := &Worker{
		Store: &fakeStore{
			connectors: []store.ConnectorRow{
				{ID: "c2", Kind: "upload", Name: "documents", Config: map[string]any{}},
			},
			files: []store.FileRow{
				{ID: "f1", Path: "notes.md", BlobKey: "ws/ws1/uploads/f1", Enabled: true},
				{ID: "f2", Path: "guides/deep.md", BlobKey: "ws/ws1/uploads/f2", Enabled: true},
			},
		},
		// No PermittedSourceRoots on purpose: files mode does not read
		// worker-local paths, so the allowlist must not gate it.
		Blobs: blobs,
	}

	spec, ids, cleanup, err := w.sourceSpec(context.Background(), run("ws1"))
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("files-mode sourceSpec: %v", err)
	}
	if ids.Upload != "c2" || spec.DocsDir == "" {
		t.Fatalf("resolution: ids=%+v spec=%+v", ids, spec)
	}

	got, err := os.ReadFile(filepath.Join(spec.DocsDir, "notes.md"))
	if err != nil || string(got) != "# Notes\n" {
		t.Errorf("staged notes.md = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(spec.DocsDir, "guides", "deep.md")); err != nil {
		t.Errorf("nested staging: %v", err)
	}
	wantKey := string(diff.DocKey(diff.UploadOrigin("notes.md")))
	if keys := spec.BlobKeys[wantKey]; len(keys) != 1 || keys[0] != "ws/ws1/uploads/f1" {
		t.Errorf("BlobKeys[%s] = %v", wantKey, spec.BlobKeys[wantKey])
	}

	cleanup()
	if _, err := os.Stat(spec.DocsDir); !os.IsNotExist(err) {
		t.Errorf("cleanup left staging dir: %v", err)
	}
}

func TestSourceSpecFilesModeSkipsPausedFiles(t *testing.T) {
	blobs := memBlobs{
		"ws/ws1/uploads/f1": []byte("# Active\n"),
		"ws/ws1/uploads/f2": []byte("# Paused\n"),
	}
	w := &Worker{
		Store: &fakeStore{
			connectors: []store.ConnectorRow{
				{ID: "c2", Kind: "upload", Name: "documents", Config: map[string]any{}},
			},
			files: []store.FileRow{
				{ID: "f1", Path: "active.md", BlobKey: "ws/ws1/uploads/f1", Enabled: true},
				{ID: "f2", Path: "paused.md", BlobKey: "ws/ws1/uploads/f2", Enabled: false},
			},
		},
		Blobs: blobs,
	}

	spec, _, cleanup, err := w.sourceSpec(context.Background(), run("ws1"))
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("sourceSpec: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spec.DocsDir, "active.md")); err != nil {
		t.Errorf("active file not staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spec.DocsDir, "paused.md")); !os.IsNotExist(err) {
		t.Errorf("paused file was staged: %v", err)
	}
	// The paused key is reported as skipped -- the pipeline needs it to keep
	// deletion detection from reading the pause as a disappearance.
	want := diff.DocKey(diff.UploadOrigin("paused.md"))
	if len(spec.SkippedKeys) != 1 || spec.SkippedKeys[0] != want {
		t.Errorf("SkippedKeys = %v, want [%s]", spec.SkippedKeys, want)
	}
	if _, ok := spec.BlobKeys[string(want)]; ok {
		t.Error("paused file's blob attributed to the sync")
	}
}

func TestSourceSpecFilesModeWithZeroFilesIsEmptySync(t *testing.T) {
	w := &Worker{
		Store: &fakeStore{connectors: []store.ConnectorRow{
			{ID: "c2", Kind: "upload", Name: "documents", Config: map[string]any{}},
		}},
		Blobs: memBlobs{},
	}
	spec, _, cleanup, err := w.sourceSpec(context.Background(), run("ws1"))
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("zero-files sourceSpec: %v", err)
	}
	entries, err := os.ReadDir(spec.DocsDir)
	if err != nil || len(entries) != 0 {
		t.Errorf("staging dir entries = %v, %v; want empty", entries, err)
	}
}

func TestSourceSpecFilesModeWithoutBlobStoreFails(t *testing.T) {
	w := &Worker{
		Store: &fakeStore{connectors: []store.ConnectorRow{
			{ID: "c2", Kind: "upload", Name: "documents", Config: map[string]any{}},
		}},
	}
	_, _, cleanup, err := w.sourceSpec(context.Background(), run("ws1"))
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "object storage is not configured") {
		t.Fatalf("err = %v, want the storage configuration message", err)
	}
}

func TestSourceSpecFailsClosed(t *testing.T) {
	repo := t.TempDir()

	cases := []struct {
		name  string
		store *fakeStore
		roots []string
		want  string
	}{
		{
			name:  "no connectors",
			store: &fakeStore{},
			roots: []string{repo},
			want:  "no enabled connector",
		},
		{
			name: "path outside allowlist",
			store: &fakeStore{connectors: []store.ConnectorRow{
				{ID: "c1", Kind: "git", Name: "code", Config: map[string]any{"path": t.TempDir()}},
			}},
			roots: []string{repo},
			want:  "outside every permitted root",
		},
		{
			name: "empty allowlist denies local paths",
			store: &fakeStore{connectors: []store.ConnectorRow{
				{ID: "c1", Kind: "git", Name: "code", Config: map[string]any{"path": repo}},
			}},
			roots: nil,
			want:  "permitted_source_roots is empty",
		},
		{
			name: "missing path",
			store: &fakeStore{connectors: []store.ConnectorRow{
				{ID: "c1", Kind: "git", Name: "code", Config: map[string]any{}},
			}},
			roots: []string{repo},
			want:  "no path configured",
		},
		{
			name: "unknown kind",
			store: &fakeStore{connectors: []store.ConnectorRow{
				{ID: "c9", Kind: "carrier-pigeon", Name: "rss", Config: map[string]any{"path": repo}},
			}},
			roots: []string{repo},
			want:  "unsupported kind",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{Store: tc.store, PermittedSourceRoots: tc.roots}
			_, _, _, err := w.sourceSpec(context.Background(), run("ws1"))
			if err == nil {
				t.Fatal("sourceSpec succeeded; want failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestSourceSpecPinnedConnector(t *testing.T) {
	repo := t.TempDir()
	w := &Worker{
		Store: &fakeStore{
			byID: map[string]store.ConnectorRow{
				"pin": {ID: "pin", Kind: "git", Name: "code", Config: map[string]any{"path": repo}},
			},
			// Enabled connectors deliberately empty: a pinned run must not
			// consult them.
		},
		PermittedSourceRoots: []string{repo},
	}

	r := run("ws1")
	r.ConnectorID = "pin"
	spec, ids, _, err := w.sourceSpec(context.Background(), r)
	if err != nil {
		t.Fatalf("pinned sourceSpec: %v", err)
	}
	if ids.Git != "pin" || spec.Path == "" {
		t.Errorf("pinned resolution: ids=%+v spec=%+v", ids, spec)
	}
}

func TestSourceSpecAllowsUploadOnlyWorkspace(t *testing.T) {
	docs := t.TempDir()
	w := &Worker{
		Store: &fakeStore{connectors: []store.ConnectorRow{
			{ID: "c2", Kind: "upload", Name: "docs", Config: map[string]any{"path": docs}},
		}},
		PermittedSourceRoots: []string{docs},
	}
	spec, ids, _, err := w.sourceSpec(context.Background(), run("ws1"))
	if err != nil {
		t.Fatalf("upload-only sourceSpec: %v", err)
	}
	if spec.Path != "" || spec.DocsDir == "" || ids.Upload != "c2" {
		t.Errorf("upload-only resolution: ids=%+v spec=%+v", ids, spec)
	}
}
