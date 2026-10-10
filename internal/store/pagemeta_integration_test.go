package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

func seedLinkedPages(t *testing.T, js *WikiStore, ws string) {
	t.Helper()
	pages := []wiki.Page{
		page("concepts/retry-policy.md", "retry-policy", "concept", "Retry policy", "# Retry policy\n\nRETRY-BODY\n"),
		page("concepts/dispatch.md", "dispatch", "concept", "Dispatch", "# Dispatch\n\nSee [[retry-policy]] and [[not-written-yet]].\n"),
		page("concepts/scheduler.md", "scheduler", "concept", "Scheduler", "# Scheduler\n\nSee [[Concepts/Retry-Policy#backoff|retries]].\n"),
		page("concepts/other.md", "other", "concept", "Other", "# Other\n\nNo links here.\n"),
	}
	pages[0].Meta.Tags = []string{"resilience"}
	if err := js.Import(context.Background(), jobs.ImportRequest{
		WorkspaceID: ws,
		UpsertPages: pages,
		UpsertSources: []diff.SourceRecord{{
			Key: diff.ModuleKey("m"), InputHash: "h", FilesWritten: []string{"concepts/retry-policy.md"},
		}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestLoadPageMetaOmitsBodiesButKeepsEverythingElse: the metadata-only read is
// the same pages with the prose absent, frontmatter intact.
func TestLoadPageMetaOmitsBodiesButKeepsEverythingElse(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	seedLinkedPages(t, js, ws)

	full, err := js.LoadPages(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := js.LoadPageMeta(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != len(full) || len(meta) != 4 {
		t.Fatalf("metadata has %d pages, full has %d, want 4", len(meta), len(full))
	}
	for i := range meta {
		if meta[i].Body != "" {
			t.Errorf("%s: metadata load shipped a body: %q", meta[i].Path, meta[i].Body)
		}
		want := full[i]
		want.Body = ""
		if !reflect.DeepEqual(meta[i], want) {
			t.Errorf("%s: metadata differs from the full load:\n got %+v\nwant %+v", meta[i].Path, meta[i], want)
		}
	}
}

func TestLoadPagesBySlugReturnsOnlyTheNamedLivePages(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	seedLinkedPages(t, js, ws)

	got, err := js.LoadPagesBySlug(ctx, ws, []string{"retry-policy", "dispatch", "no-such-page"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Slug != "dispatch" || got[1].Slug != "retry-policy" {
		t.Fatalf("got %+v, want dispatch and retry-policy by path", got)
	}
	if got[1].Body != "# Retry policy\n\nRETRY-BODY\n" {
		t.Errorf("body missing: %q", got[1].Body)
	}

	if none, err := js.LoadPagesBySlug(ctx, ws, nil); err != nil || len(none) != 0 {
		t.Errorf("no slugs returned %v, %v", none, err)
	}

	if _, err := js.DeletePage(ctx, ws, "dispatch", "gone", ""); err != nil {
		t.Fatal(err)
	}
	got, err = js.LoadPagesBySlug(ctx, ws, []string{"dispatch"})
	if err != nil || len(got) != 0 {
		t.Errorf("a deleted page was loaded: %v, %v", got, err)
	}
}

// TestLinkingSlugsReadsTheLinkTable: aliases, directory prefixes and anchors
// all point at the same page, a link to a page that does not exist yet is
// still found, and a deleted referrer is not.
func TestLinkingSlugsReadsTheLinkTable(t *testing.T) {
	js, ws := jobStore(t)
	ctx := context.Background()
	seedLinkedPages(t, js, ws)

	got, err := js.LinkingSlugs(ctx, ws, "Retry-Policy")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dispatch", "scheduler"}; !reflect.DeepEqual(got, want) {
		t.Errorf("linkers of retry-policy = %v, want %v", got, want)
	}

	gap, err := js.LinkingSlugs(ctx, ws, "not-written-yet")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dispatch"}; !reflect.DeepEqual(gap, want) {
		t.Errorf("linkers of a gap = %v, want %v", gap, want)
	}

	if none, err := js.LinkingSlugs(ctx, ws, "other"); err != nil || len(none) != 0 {
		t.Errorf("an unlinked page has linkers %v, %v", none, err)
	}
	if none, err := js.LinkingSlugs(ctx, ws, ""); err != nil || len(none) != 0 {
		t.Errorf("an empty slug has linkers %v, %v", none, err)
	}

	if _, err := js.DeletePage(ctx, ws, "scheduler", "gone", ""); err != nil {
		t.Fatal(err)
	}
	got, err = js.LinkingSlugs(ctx, ws, "retry-policy")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"dispatch"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after deleting a linker: %v, want %v", got, want)
	}
}
