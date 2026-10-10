package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/publish"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// publishStore is the slice of Store the publish path touches, with the rest
// inherited from fakeStore so this file only states what it exercises.
type publishStore struct {
	fakeStore

	target    store.PublishTarget
	targetErr error

	pages    []wiki.Page
	artifact map[string]string
	figures  []store.FigureRow

	// revision is what WikiRevision reports; revisionErr makes it fail.
	revision    int64
	revisionErr error

	// loads counts the expensive reads a publish makes, so a test can prove a
	// skipped publish made none of them.
	pageLoads, figureLoads, artifactLoads int

	// marked records every MarkPublished call, which is the only durable
	// evidence a push produced.
	marked []markedPublish
}

type markedPublish struct {
	commit   string
	revision int64
	err      string
}

func (p *publishStore) WikiRevision(context.Context, string) (int64, error) {
	return p.revision, p.revisionErr
}

func (p *publishStore) PublishTargetFor(context.Context, string) (store.PublishTarget, error) {
	return p.target, p.targetErr
}

func (p *publishStore) MarkPublished(_ context.Context, _, commit string, revision int64, publishErr string) error {
	p.marked = append(p.marked, markedPublish{commit: commit, revision: revision, err: publishErr})
	return nil
}

func (p *publishStore) LoadPages(context.Context, string) ([]wiki.Page, error) {
	p.pageLoads++
	return p.pages, nil
}

func (p *publishStore) LoadArtifact(_ context.Context, _, kind string) (string, error) {
	p.artifactLoads++
	if body, ok := p.artifact[kind]; ok {
		return body, nil
	}
	return "", errors.New("no such artifact")
}

func (p *publishStore) ListFigures(context.Context, string) ([]store.FigureRow, error) {
	p.figureLoads++
	return p.figures, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPublishIsSkippedWhenTheBenchDoesNotPublish: publishing is opt-in, and the
// overwhelmingly common case must cost nothing and record nothing.
func TestPublishIsSkippedWhenTheBenchDoesNotPublish(t *testing.T) {
	st := &publishStore{targetErr: store.ErrNotFound}
	w := &Worker{Store: st, Log: quietLogger()}

	w.publishAfterBuild(context.Background(), "ws", "demo", "manual", quietLogger())

	if len(st.marked) != 0 {
		t.Errorf("a bench with no target recorded %v", st.marked)
	}
}

// TestPublishIsSkippedWhenDisabled: a configured but disabled target is a
// deliberate pause, not a thing to attempt and fail.
func TestPublishIsSkippedWhenDisabled(t *testing.T) {
	st := &publishStore{target: store.PublishTarget{
		RepoURL: "https://github.com/example/wiki.git", Branch: "main", Enabled: false,
	}}
	w := &Worker{Store: st, Log: quietLogger()}

	w.publishAfterBuild(context.Background(), "ws", "demo", "manual", quietLogger())

	if len(st.marked) != 0 {
		t.Errorf("a disabled target was attempted: %v", st.marked)
	}
}

// TestPublishFailureIsRecordedNotRaised is the property the whole design rests
// on: the build has already succeeded, so a push that cannot happen is written
// down rather than thrown.
func TestPublishFailureIsRecordedNotRaised(t *testing.T) {
	st := &publishStore{
		// A remote the clone policy refuses, so the push fails without
		// touching the network.
		target: store.PublishTarget{
			RepoURL: "https://127.0.0.1/example/wiki.git", Branch: "main", Enabled: true,
		},
		pages: []wiki.Page{{
			Path: "concepts/a.md", Slug: "a",
			Meta: wiki.Frontmatter{Type: "concept", Title: "A"}, Body: "# A\n",
		}},
	}
	w := &Worker{Store: st, Log: quietLogger()}

	// Returns normally: publishAfterBuild has no error to give back, by design.
	w.publishAfterBuild(context.Background(), "ws", "demo", "manual", quietLogger())

	if len(st.marked) != 1 {
		t.Fatalf("recorded %v, want one failure", st.marked)
	}
	if st.marked[0].err == "" {
		t.Error("the failure was recorded as a success")
	}
	if st.marked[0].commit != "" {
		t.Errorf("a failed push recorded commit %q", st.marked[0].commit)
	}
	// The reason has to be diagnosable from the target row alone.
	if !strings.Contains(st.marked[0].err, "public address") {
		t.Errorf("recorded reason is not actionable: %q", st.marked[0].err)
	}
}

// countingBlobs counts reads, because the point of skipping a publish is that
// no image is pulled out of object storage to build a tree nobody will push.
type countingBlobs struct {
	memBlobs
	gets int
}

func (c *countingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets++
	return c.memBlobs.Get(ctx, key)
}

// fakePush swaps the network push for a counter, restoring it afterwards.
func fakePush(t *testing.T, err error) *int {
	t.Helper()
	var calls int
	orig := pushToRepository
	pushToRepository = func(context.Context, publish.PushOptions) (publish.Result, error) {
		calls++
		if err != nil {
			return publish.Result{}, err
		}
		return publish.Result{Commit: "abc123"}, nil
	}
	t.Cleanup(func() { pushToRepository = orig })
	return &calls
}

func publishableStore() (*publishStore, *countingBlobs) {
	blobs := &countingBlobs{memBlobs: memBlobs{"k1": []byte("png")}}
	return &publishStore{
		target: store.PublishTarget{
			RepoURL: "https://github.com/example/wiki.git", Branch: "main", Enabled: true,
		},
		pages: []wiki.Page{{
			Path: "concepts/a.md", Slug: "a",
			Meta: wiki.Frontmatter{Type: "concept", Title: "A"}, Body: "# A\n",
		}},
		figures: []store.FigureRow{{ID: "f1", BlobKey: "k1", ContentType: "image/png"}},
	}, blobs
}

// TestPublishSkipsAnUnchangedRevision: a build that changed nothing must not
// load the wiki, read its images, or touch the remote.
func TestPublishSkipsAnUnchangedRevision(t *testing.T) {
	pushes := fakePush(t, nil)
	st, blobs := publishableStore()
	st.revision = 7
	st.target.PublishedRevision = 7
	w := &Worker{Store: st, Blobs: blobs, Log: quietLogger()}

	w.publishAfterBuild(context.Background(), "ws", "demo", "poll", quietLogger())

	if *pushes != 0 {
		t.Errorf("pushed %d times at an unchanged revision", *pushes)
	}
	if st.pageLoads != 0 || st.figureLoads != 0 || st.artifactLoads != 0 || blobs.gets != 0 {
		t.Errorf("loaded %d page sets, %d figure lists, %d artifacts, %d blobs; want none",
			st.pageLoads, st.figureLoads, st.artifactLoads, blobs.gets)
	}
	if len(st.marked) != 0 {
		t.Errorf("a skipped publish recorded %v", st.marked)
	}
}

// TestPublishRunsWhenTheRevisionMoved covers the cases that must still publish:
// a build that changed pages, and a page deleted by hand, which moves the
// revision with no build at all.
func TestPublishRunsWhenTheRevisionMoved(t *testing.T) {
	pushes := fakePush(t, nil)
	st, blobs := publishableStore()
	st.revision = 8
	st.target.PublishedRevision = 7
	w := &Worker{Store: st, Blobs: blobs, Log: quietLogger()}

	w.publishAfterBuild(context.Background(), "ws", "demo", "manual", quietLogger())

	if *pushes != 1 {
		t.Fatalf("pushed %d times, want 1", *pushes)
	}
	if blobs.gets == 0 {
		t.Error("the figures were never read for a real publish")
	}
	if len(st.marked) != 1 || st.marked[0].err != "" || st.marked[0].revision != 8 || st.marked[0].commit != "abc123" {
		t.Errorf("recorded %+v, want a success stamped with revision 8", st.marked)
	}
}

// TestPublishRecordsTheRevisionWhenTheRepositoryAlreadyMatched: finding nothing
// to commit is still a successful sync at this revision, and recording it is
// what lets the next build skip.
func TestPublishRecordsTheRevisionWhenTheRepositoryAlreadyMatched(t *testing.T) {
	fakePush(t, publish.ErrNothingToPush)
	st, blobs := publishableStore()
	st.revision = 3
	w := &Worker{Store: st, Blobs: blobs, Log: quietLogger()}

	w.publishAfterBuild(context.Background(), "ws", "demo", "manual", quietLogger())

	if len(st.marked) != 1 || st.marked[0].err != "" || st.marked[0].revision != 3 {
		t.Errorf("recorded %+v, want a success at revision 3", st.marked)
	}
}

// TestFailedPublishDoesNotAdvanceTheRevision: a push that failed must be tried
// again by the next build even though nothing new was written meanwhile.
func TestFailedPublishDoesNotAdvanceTheRevision(t *testing.T) {
	fakePush(t, errors.New("remote unreachable"))
	st, blobs := publishableStore()
	st.revision = 9
	st.target.PublishedRevision = 8
	w := &Worker{Store: st, Blobs: blobs, Log: quietLogger()}

	w.publishAfterBuild(context.Background(), "ws", "demo", "manual", quietLogger())

	if len(st.marked) != 1 || st.marked[0].err == "" || st.marked[0].revision != 0 {
		t.Errorf("recorded %+v, want a failure carrying no revision", st.marked)
	}
}

// TestPublishDoesNotSkipWhenTheRevisionIsUnknown: if the check itself cannot be
// answered the safe move is the old behaviour -- push -- and recording
// "unknown" so that it never counts as current.
func TestPublishDoesNotSkipWhenTheRevisionIsUnknown(t *testing.T) {
	pushes := fakePush(t, nil)
	st, blobs := publishableStore()
	st.revisionErr = errors.New("db down")
	st.target.PublishedRevision = 5
	w := &Worker{Store: st, Blobs: blobs, Log: quietLogger()}

	w.publishAfterBuild(context.Background(), "ws", "demo", "manual", quietLogger())

	if *pushes != 1 {
		t.Errorf("pushed %d times, want 1", *pushes)
	}
	if len(st.marked) != 1 || st.marked[0].revision != 0 {
		t.Errorf("recorded %+v, want revision 0", st.marked)
	}
}

// TestFirstPublishOfAnEmptyBenchStillRuns: revision zero means no wiki yet, and
// never-published is also zero. They must not be mistaken for "current".
func TestFirstPublishOfAnEmptyBenchStillRuns(t *testing.T) {
	pushes := fakePush(t, nil)
	st, blobs := publishableStore()
	w := &Worker{Store: st, Blobs: blobs, Log: quietLogger()}

	w.publishAfterBuild(context.Background(), "ws", "demo", "manual", quietLogger())

	if *pushes != 1 {
		t.Errorf("pushed %d times, want 1", *pushes)
	}
}

func TestPublishableFiguresSkipsUnreadableBlobs(t *testing.T) {
	st := &publishStore{figures: []store.FigureRow{
		{ID: "good", BlobKey: "k1", ContentType: "image/png"},
		{ID: "gone", BlobKey: "missing", ContentType: "image/png"},
	}}
	w := &Worker{
		Store: st,
		Blobs: memBlobs{"k1": []byte("png")},
		Log:   quietLogger(),
	}

	got, err := w.publishableFigures(context.Background(), "ws")
	if err != nil {
		t.Fatalf("publishableFigures: %v", err)
	}
	// One unreadable image must not withhold the wiki; its page keeps the
	// unresolved reference instead.
	if len(got) != 1 || got[0].ID != "good" {
		t.Errorf("got %+v, want only the readable figure", got)
	}
}

// TestPublishableFiguresWithoutObjectStorage: a deployment with no blob store
// publishes prose and no images rather than failing.
func TestPublishableFiguresWithoutObjectStorage(t *testing.T) {
	w := &Worker{Store: &publishStore{}, Log: quietLogger()}

	got, err := w.publishableFigures(context.Background(), "ws")
	if err != nil || got != nil {
		t.Errorf("got %v, %v; want no figures and no error", got, err)
	}
}

func TestPublishTokenIsEmptyWithoutACredential(t *testing.T) {
	w := &Worker{Store: &publishStore{}, Log: quietLogger()}

	token, err := w.publishToken(context.Background(), store.PublishTarget{})
	if err != nil || token != "" {
		t.Errorf("got %q, %v; want an unauthenticated push", token, err)
	}
}

func TestCommitMessageNamesTheBenchAndTheTrigger(t *testing.T) {
	msg := commitMessage("demo", "webhook", 12)

	subject, body, found := strings.Cut(msg, "\n")
	if !found {
		t.Fatalf("message has no body:\n%s", msg)
	}
	if !strings.Contains(subject, "demo") || !strings.Contains(subject, "12 pages") {
		t.Errorf("subject = %q", subject)
	}
	if !strings.Contains(body, "webhook") {
		t.Errorf("body does not record the trigger:\n%s", body)
	}
	// A bench with no slug still gets a usable subject rather than a stray gap.
	if s := commitMessage("", "manual", 3); strings.Contains(s, "  ") {
		t.Errorf("unnamed bench produced %q", s)
	}
}

func TestReadBlob(t *testing.T) {
	blobs := memBlobs{"k": []byte("bytes")}

	got, err := readBlob(context.Background(), blobs, "k")
	if err != nil || string(got) != "bytes" {
		t.Errorf("readBlob = %q, %v", got, err)
	}
	if _, err := readBlob(context.Background(), blobs, "absent"); err == nil {
		t.Error("reading a missing blob succeeded")
	}
}
