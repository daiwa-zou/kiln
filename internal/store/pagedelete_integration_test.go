package store

import (
	"context"
	"errors"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// seedPage imports one page so there is something to delete.
func seedPage(t *testing.T, js *WikiStore, ws, path, slug string) {
	t.Helper()
	if err := js.Import(context.Background(), jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: []wiki.Page{page(path, slug, "concept", "A Concept", "# A Concept\n\nBody.\n")},
		UpsertSources: []diff.SourceRecord{{
			Key: diff.ModuleKey("m"), InputHash: "h", FilesWritten: []string{path},
		}},
	}); err != nil {
		t.Fatalf("seed page: %v", err)
	}
}

func TestDeletePageRemovesItAndRecordsWhy(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	seedPage(t, js, ws, "concepts/dispatch.md", "dispatch")

	slug, err := js.DeletePage(ctx, ws, "dispatch", "duplicate of the queue page", "")
	if err != nil {
		t.Fatalf("DeletePage: %v", err)
	}
	if slug != "dispatch" {
		t.Errorf("slug = %q", slug)
	}

	if live := livePages(t, js, ws); live["concepts/dispatch.md"] {
		t.Error("the page is still live after deletion")
	}

	// Soft, not destroyed: the retention window is what makes this reversible.
	var n int
	if err := js.pool.QueryRow(ctx, `
		SELECT count(*) FROM pages WHERE slug = 'dispatch' AND deleted_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("found %d soft-deleted rows, want 1", n)
	}

	// The suppression is what keeps it gone, and it reaches the pipeline
	// through the same channel as corrections.
	steering, err := js.LoadSteering(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	reason, ok := steering.Suppressed["dispatch"]
	if !ok {
		t.Fatal("the deletion did not reach steering; the next build would rewrite the page")
	}
	if reason != "duplicate of the queue page" {
		t.Errorf("reason = %q", reason)
	}
}

// TestDeletePageAddressableByPathOrSlug: a URL gives you one, a link the other.
func TestDeletePageAddressableByPathOrSlug(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()

	seedPage(t, js, ws, "concepts/dispatch.md", "dispatch")
	if _, err := js.DeletePage(ctx, ws, "concepts/dispatch.md", "", ""); err != nil {
		t.Fatalf("delete by path: %v", err)
	}

	// Deleting again reads as a missing page rather than silently suppressing
	// a name -- which would otherwise be a way to pre-empt pages the wiki has
	// not written yet.
	if _, err := js.DeletePage(ctx, ws, "dispatch", "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

func TestRestorePageBringsItBack(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	seedPage(t, js, ws, "concepts/dispatch.md", "dispatch")

	if _, err := js.DeletePage(ctx, ws, "dispatch", "mistake", ""); err != nil {
		t.Fatal(err)
	}
	restored, err := js.RestorePage(ctx, ws, "dispatch")
	if err != nil {
		t.Fatalf("RestorePage: %v", err)
	}
	if !restored {
		t.Error("restored = false, but the soft-deleted row was still there")
	}
	if live := livePages(t, js, ws); !live["concepts/dispatch.md"] {
		t.Error("the page did not come back")
	}

	steering, err := js.LoadSteering(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := steering.Suppressed["dispatch"]; still {
		t.Error("the suppression outlived the restore; builds would still refuse to write it")
	}

	if _, err := js.RestorePage(ctx, ws, "dispatch"); !errors.Is(err, ErrNotFound) {
		t.Errorf("restoring twice = %v, want ErrNotFound", err)
	}
}

// TestRestoreAfterSweepLiftsTheSuppressionAnyway: once the retention window
// closes there is no row to bring back, but the deletion must still be
// reversible -- otherwise a decision made in a moment becomes permanent 30 days
// later without anyone choosing that.
func TestRestoreAfterSweepLiftsTheSuppressionAnyway(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	seedPage(t, js, ws, "concepts/dispatch.md", "dispatch")

	if _, err := js.DeletePage(ctx, ws, "dispatch", "", ""); err != nil {
		t.Fatal(err)
	}
	// Stand in for the sweep.
	if _, err := js.pool.Exec(ctx, `DELETE FROM pages WHERE slug = 'dispatch'`); err != nil {
		t.Fatal(err)
	}

	restored, err := js.RestorePage(ctx, ws, "dispatch")
	if err != nil {
		t.Fatalf("RestorePage: %v", err)
	}
	if restored {
		t.Error("restored = true, but there was no row left to restore")
	}
	steering, _ := js.LoadSteering(ctx, ws)
	if _, still := steering.Suppressed["dispatch"]; still {
		t.Error("the suppression survived; the next build still would not write the page")
	}
}

func TestListDeletedPages(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	seedPage(t, js, ws, "concepts/dispatch.md", "dispatch")

	if _, err := js.DeletePage(ctx, ws, "dispatch", "duplicate", ""); err != nil {
		t.Fatal(err)
	}
	listed, err := js.ListDeletedPages(ctx, ws)
	if err != nil {
		t.Fatalf("ListDeletedPages: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d, want 1", len(listed))
	}
	got := listed[0]
	if got.Slug != "dispatch" || got.Reason != "duplicate" {
		t.Errorf("entry = %+v", got)
	}
	// The title comes from the soft-deleted row, so the list reads as names
	// rather than slugs while the page is still recoverable.
	if got.Title != "A Concept" || got.Path != "concepts/dispatch.md" {
		t.Errorf("entry lost its identity: %+v", got)
	}
	if got.Live {
		t.Error("a deleted page reported itself live")
	}
}

func TestDeletePageIsWorkspaceScoped(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	seedPage(t, js, ws, "concepts/dispatch.md", "dispatch")

	other, err := js.EnsureWorkspace(ctx, "test-org", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.DeletePage(ctx, other, "dispatch", "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant delete = %v, want ErrNotFound", err)
	}
	if live := livePages(t, js, ws); !live["concepts/dispatch.md"] {
		t.Error("a cross-tenant delete removed the owning workspace's page")
	}
}
