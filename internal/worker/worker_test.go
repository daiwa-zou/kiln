package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/store"
)

// fakeStore satisfies Store for exercising the resolution logic without
// Postgres. Claim/fail/requeue are not used by these tests.
type fakeStore struct {
	connectors []store.ConnectorRow
	byID       map[string]store.ConnectorRow
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
func (f *fakeStore) WorkspaceBudgetUSD(context.Context, string) (*float64, error) { return nil, nil }
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

	spec, gitID, _, err := w.sourceSpec(context.Background(), run("ws1"))
	if err != nil {
		t.Fatalf("sourceSpec: %v", err)
	}
	if spec.Path == "" || spec.DocsDir == "" {
		t.Errorf("spec incomplete: %+v", spec)
	}
	if spec.Slug != "bench" {
		t.Errorf("slug = %q, want workspace slug", spec.Slug)
	}
	if gitID != "c1" {
		t.Errorf("git connector id = %q, want c1", gitID)
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
			name: "upload without git",
			store: &fakeStore{connectors: []store.ConnectorRow{
				{ID: "c2", Kind: "upload", Name: "docs", Config: map[string]any{"path": repo}},
			}},
			roots: []string{repo},
			want:  "no git connector",
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
	spec, gitID, _, err := w.sourceSpec(context.Background(), r)
	if err != nil {
		t.Fatalf("pinned sourceSpec: %v", err)
	}
	if gitID != "pin" || spec.Path == "" {
		t.Errorf("pinned resolution: id=%q spec=%+v", gitID, spec)
	}
}
