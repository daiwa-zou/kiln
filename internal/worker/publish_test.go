package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

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

	// marked records every MarkPublished call, which is the only durable
	// evidence a push produced.
	marked []markedPublish
}

type markedPublish struct{ commit, err string }

func (p *publishStore) PublishTargetFor(context.Context, string) (store.PublishTarget, error) {
	return p.target, p.targetErr
}

func (p *publishStore) MarkPublished(_ context.Context, _, commit, publishErr string) error {
	p.marked = append(p.marked, markedPublish{commit: commit, err: publishErr})
	return nil
}

func (p *publishStore) LoadPages(context.Context, string) ([]wiki.Page, error) {
	return p.pages, nil
}

func (p *publishStore) LoadArtifact(_ context.Context, _, kind string) (string, error) {
	if body, ok := p.artifact[kind]; ok {
		return body, nil
	}
	return "", errors.New("no such artifact")
}

func (p *publishStore) ListFigures(context.Context, string) ([]store.FigureRow, error) {
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
