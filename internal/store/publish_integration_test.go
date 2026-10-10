package store

import (
	"context"
	"testing"
)

// TestPublishedRevisionTracksTheWiki is the contract the worker's skip leans
// on: the wiki revision moves on every change a mirror could show, including a
// page deleted by hand with no build, and the target remembers the revision
// only for as long as the destination it was pushed to is the one configured.
func TestPublishedRevisionTracksTheWiki(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	rev, err := js.WikiRevision(ctx, ws)
	if err != nil || rev != 0 {
		t.Fatalf("WikiRevision before any wiki = %d, %v; want 0", rev, err)
	}

	seedPage(t, js, ws, "concepts/dispatch.md", "dispatch")
	afterImport, err := js.WikiRevision(ctx, ws)
	if err != nil || afterImport <= rev {
		t.Fatalf("WikiRevision after import = %d, %v; want > %d", afterImport, err, rev)
	}

	if _, err := js.SetPublishTarget(ctx, PublishTarget{
		WorkspaceID: ws, RepoURL: "https://github.com/example/wiki.git", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	target := func() PublishTarget {
		t.Helper()
		got, err := js.PublishTargetFor(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := target().PublishedRevision; got != 0 {
		t.Errorf("a new target claims revision %d was published", got)
	}

	// A success records the revision it matched.
	if err := js.MarkPublished(ctx, ws, "c1", afterImport, ""); err != nil {
		t.Fatal(err)
	}
	if got := target().PublishedRevision; got != afterImport {
		t.Errorf("PublishedRevision = %d, want %d", got, afterImport)
	}

	// A failure must leave it alone, or the next build would skip a retry.
	if err := js.MarkPublished(ctx, ws, "", 0, "network unreachable"); err != nil {
		t.Fatal(err)
	}
	if got := target().PublishedRevision; got != afterImport {
		t.Errorf("a failed push moved PublishedRevision to %d", got)
	}

	// Deleting a page changes the mirror with no build, so it has to move the
	// revision.
	if _, err := js.DeletePage(ctx, ws, "dispatch", "duplicate", ""); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := js.WikiRevision(ctx, ws)
	if err != nil || afterDelete <= afterImport {
		t.Errorf("WikiRevision after delete = %d, %v; want > %d", afterDelete, err, afterImport)
	}

	// Toggling enabled is not a new destination.
	off := false
	if err := js.PatchPublishTarget(ctx, ws, PublishTargetPatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if got := target().PublishedRevision; got != afterImport {
		t.Errorf("disabling reset PublishedRevision to %d", got)
	}

	// Pointing at another repository is: it has been given nothing yet.
	other := "https://github.com/example/other.git"
	if err := js.PatchPublishTarget(ctx, ws, PublishTargetPatch{RepoURL: &other}); err != nil {
		t.Fatal(err)
	}
	if got := target().PublishedRevision; got != 0 {
		t.Errorf("a new repository kept PublishedRevision %d", got)
	}

	// And so is reconfiguring through the upsert.
	if err := js.MarkPublished(ctx, ws, "c2", afterDelete, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := js.SetPublishTarget(ctx, PublishTarget{
		WorkspaceID: ws, RepoURL: other, Branch: "docs", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := target().PublishedRevision; got != 0 {
		t.Errorf("SetPublishTarget kept PublishedRevision %d", got)
	}
}
