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
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// fakeStore satisfies Store for exercising the resolution logic without
// Postgres. Claim/fail/requeue are not used by these tests.
type fakeStore struct {
	connectors []store.ConnectorRow
	byID       map[string]store.ConnectorRow
	files      []store.FileRow
	reviews    []string

	// question is what a research run finds behind its run id, nil for a run
	// whose review item is gone; recorded captures the write-back.
	question *store.ResearchQuestion
	recorded []string
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
func (f *fakeStore) QueueDepth(context.Context) (map[string]int, error) {
	return map[string]int{"queued": 0, "running": 0}, nil
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
func (f *fakeStore) FileReview(_ context.Context, _, kind, title, _ string) error {
	f.reviews = append(f.reviews, kind+":"+title)
	return nil
}
func (f *fakeStore) ResearchQuestionFor(_ context.Context, _ string) (*store.ResearchQuestion, error) {
	if f.question == nil {
		return nil, store.ErrNotFound
	}
	return f.question, nil
}
func (f *fakeStore) RecordResearch(_ context.Context, reviewID, findings string, resolved bool) error {
	f.recorded = append(f.recorded, fmt.Sprintf("%s|%v|%s", reviewID, resolved, findings))
	return nil
}

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

// A document whose bytes are missing from object storage is an incident for
// that document, not for the bench. Failing the run would stop the
// repository, the web pages, and every healthy document from ingesting --
// turning a storage problem into a total outage.
func TestSourceSpecFilesModeSurvivesUnreadableBlob(t *testing.T) {
	blobs := memBlobs{
		"ws/ws1/uploads/ok": []byte("# Present\n"),
		// "ws/ws1/uploads/gone" is deliberately absent.
	}
	st := &fakeStore{
		connectors: []store.ConnectorRow{
			{ID: "c2", Kind: "upload", Name: "documents", Config: map[string]any{}},
		},
		files: []store.FileRow{
			{ID: "f1", Path: "present.md", BlobKey: "ws/ws1/uploads/ok", Enabled: true},
			{ID: "f2", Path: "vanished.md", BlobKey: "ws/ws1/uploads/gone", Enabled: true},
		},
	}
	w := &Worker{Store: st, Blobs: blobs}

	spec, ids, cleanup, err := w.sourceSpec(context.Background(), run("ws1"))
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("one unreadable blob failed the whole resolution: %v", err)
	}
	if ids.Upload != "c2" {
		t.Fatalf("upload connector not resolved: %+v", ids)
	}

	// The healthy document still staged.
	if _, err := os.Stat(filepath.Join(spec.DocsDir, "present.md")); err != nil {
		t.Errorf("healthy document not staged: %v", err)
	}

	// The unreadable one is reported as skipped, not missing: treating it as
	// gone would let the deletion cascade offer to remove its pages, turning
	// a storage incident into content loss.
	want := diff.DocKey(diff.UploadOrigin("vanished.md"))
	var found bool
	for _, k := range spec.SkippedKeys {
		if k == want {
			found = true
		}
	}
	if !found {
		t.Errorf("SkippedKeys = %v, want it to contain %s", spec.SkippedKeys, want)
	}
	if _, ok := spec.BlobKeys[string(want)]; ok {
		t.Error("unreadable document was attributed to the sync")
	}

	// And it is surfaced where a human looks, rather than swallowed.
	if len(st.reviews) != 1 {
		t.Fatalf("reviews = %v, want one storage review", st.reviews)
	}
	if !strings.Contains(st.reviews[0], "storage") || !strings.Contains(st.reviews[0], "unreadable") {
		t.Errorf("review %q does not name the problem", st.reviews[0])
	}
}

// truncatingBlobs serves one key through a reader that fails partway, the way
// a connection dropped mid-download does. The bytes delivered before the
// failure are real, which is what makes the half-written file convincing.
type truncatingBlobs struct {
	memBlobs
	failKey string
}

func (b truncatingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := b.memBlobs.Get(ctx, key)
	if err != nil || key != b.failKey {
		return rc, err
	}
	return io.NopCloser(io.MultiReader(
		io.LimitReader(rc, 8),
		errReader{fmt.Errorf("connection reset mid-download")},
	)), nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// A blob whose download dies partway must leave nothing behind. The extractor
// reads the staging directory rather than the file list, so a half-written
// file would be ingested as if it were the whole document -- and the bench
// would end up holding a page written from a fragment while the review queue
// reported that same document as skipped.
func TestSourceSpecFilesModeDiscardsATruncatedBlob(t *testing.T) {
	blobs := truncatingBlobs{
		memBlobs: memBlobs{
			"ws/ws1/uploads/ok":   []byte("# Present\n\nThis one downloads cleanly.\n"),
			"ws/ws1/uploads/half": []byte("# Truncated\n\nEverything past the first few bytes never arrives.\n"),
		},
		failKey: "ws/ws1/uploads/half",
	}
	st := &fakeStore{
		connectors: []store.ConnectorRow{
			{ID: "c2", Kind: "upload", Name: "documents", Config: map[string]any{}},
		},
		files: []store.FileRow{
			{ID: "f1", Path: "present.md", BlobKey: "ws/ws1/uploads/ok", Enabled: true},
			{ID: "f2", Path: "truncated.md", BlobKey: "ws/ws1/uploads/half", Enabled: true},
		},
	}
	w := &Worker{Store: st, Blobs: blobs}

	spec, _, cleanup, err := w.sourceSpec(context.Background(), run("ws1"))
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("a truncated blob failed the whole resolution: %v", err)
	}

	if _, err := os.Stat(filepath.Join(spec.DocsDir, "truncated.md")); !os.IsNotExist(err) {
		t.Errorf("truncated document survived in staging (stat err = %v); "+
			"it would be extracted as a complete document", err)
	}
	// The healthy document is untouched by its neighbor's failure.
	if _, err := os.Stat(filepath.Join(spec.DocsDir, "present.md")); err != nil {
		t.Errorf("healthy document not staged: %v", err)
	}

	// And the truncated one is accounted for the same way any unreadable
	// document is: skipped, not missing, so the cascade leaves its pages be.
	want := diff.DocKey(diff.UploadOrigin("truncated.md"))
	var found bool
	for _, k := range spec.SkippedKeys {
		if k == want {
			found = true
		}
	}
	if !found {
		t.Errorf("SkippedKeys = %v, want it to contain %s", spec.SkippedKeys, want)
	}
	if _, ok := spec.BlobKeys[string(want)]; ok {
		t.Error("truncated document was attributed to the sync")
	}
	if len(st.reviews) != 1 {
		t.Errorf("reviews = %v, want one storage review", st.reviews)
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

// A bench with no publish target is the default, so these stubs report exactly
// that: publishing is opt-in and the loop must be unaffected without it.
func (f *fakeStore) PublishTargetFor(context.Context, string) (store.PublishTarget, error) {
	return store.PublishTarget{}, store.ErrNotFound
}
func (f *fakeStore) MarkPublished(context.Context, string, string, string) error { return nil }
func (f *fakeStore) LoadArtifact(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *fakeStore) ListFigures(context.Context, string) ([]store.FigureRow, error) {
	return nil, nil
}
func (f *fakeStore) LoadPages(context.Context, string) ([]wiki.Page, error) { return nil, nil }
