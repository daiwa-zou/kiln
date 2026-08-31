package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

func seedDeletablePage(t *testing.T, js *store.WikiStore, wsID string) {
	t.Helper()
	if err := js.Import(context.Background(), jobs.ImportRequest{
		WorkspaceID: wsID,
		UpsertPages: []wiki.Page{{
			Path: "concepts/dispatch.md", Slug: "dispatch",
			Meta: wiki.Frontmatter{
				Type: "concept", Title: "Dispatch", Created: "2026-08-14", Updated: "2026-08-14",
			},
			Body: "# Dispatch\n\nHow work is handed to workers.\n",
		}},
		UpsertSources: []diff.SourceRecord{{
			Key: diff.ModuleKey("m"), InputHash: "h",
			FilesWritten: []string{"concepts/dispatch.md"},
		}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestPageDeleteAndRestoreRoundTrip(t *testing.T) {
	srv, js, wsID := testServer(t)
	seedDeletablePage(t, js, wsID)

	var deleted map[string]any
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/pages/concepts/dispatch.md",
		"", map[string]any{"reason": "duplicate of the queue page"}, &deleted); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	if deleted["slug"] != "dispatch" || deleted["staysDeleted"] != true {
		t.Errorf("delete response = %v", deleted)
	}

	// Gone from the read surface immediately.
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/pages/concepts/dispatch.md", "", nil, nil); code != http.StatusNotFound {
		t.Errorf("GET deleted page = %d, want 404", code)
	}
	var listed []map[string]any
	if code := get(t, srv, "/api/v1/workspaces/demo/pages", &listed); code != http.StatusOK {
		t.Fatal("list pages failed")
	}
	for _, p := range listed {
		if p["slug"] == "dispatch" {
			t.Error("a deleted page is still in the page list")
		}
	}

	// Visible as deleted, with the reason, so the decision can be reconsidered.
	var gone []map[string]any
	if code := get(t, srv, "/api/v1/workspaces/demo/pages-deleted", &gone); code != http.StatusOK {
		t.Fatalf("deleted list = %d", code)
	}
	if len(gone) != 1 || gone[0]["slug"] != "dispatch" {
		t.Fatalf("deleted list = %v", gone)
	}
	if gone[0]["reason"] != "duplicate of the queue page" {
		t.Errorf("reason = %v", gone[0]["reason"])
	}
	if gone[0]["title"] != "Dispatch" {
		t.Errorf("title = %v, want the page's own name", gone[0]["title"])
	}

	// And it comes back.
	var restored map[string]any
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/pages-deleted/dispatch/restore",
		"", nil, &restored); code != http.StatusOK {
		t.Fatalf("restore = %d", code)
	}
	if restored["pageBack"] != true {
		t.Errorf("restore response = %v, want pageBack true", restored)
	}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/pages/concepts/dispatch.md", "", nil, nil); code != http.StatusOK {
		t.Errorf("GET restored page = %d, want 200", code)
	}
}

func TestPageDeleteWithoutAReasonIsFine(t *testing.T) {
	srv, js, wsID := testServer(t)
	seedDeletablePage(t, js, wsID)

	// No body at all: making someone justify a deletion to their own wiki is
	// friction with no payoff.
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/pages/dispatch", "", nil, nil); code != http.StatusOK {
		t.Fatalf("delete without reason = %d", code)
	}
	var gone []map[string]any
	get(t, srv, "/api/v1/workspaces/demo/pages-deleted", &gone)
	if len(gone) != 1 || gone[0]["reason"] != "" {
		t.Errorf("deleted list = %v", gone)
	}
}

func TestPageDeleteOfAbsentPageIs404(t *testing.T) {
	srv, _, _ := testServer(t)

	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/pages/concepts/nope.md", "", nil, nil); code != http.StatusNotFound {
		t.Errorf("delete missing page = %d, want 404", code)
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/pages-deleted/nope/restore", "", nil, nil); code != http.StatusNotFound {
		t.Errorf("restore missing = %d, want 404", code)
	}
}

// TestPageDeleteAuthorizationByRole: deleting a page sits with corrections, not
// with deleting a bench. A viewer must not; a member may.
func TestPageDeleteAuthorizationByRole(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	js := store.NewWikiStore(pool)
	seedDeletablePage(t, js, wsID)

	viewer := addMember(t, pool, wsID, "vera-pages", "viewer")
	member := addMember(t, pool, wsID, "mira-pages", "member")

	src.id = auth.Identity{UserID: viewer, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/pages/dispatch", "kiln_valid", nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer delete = %d, want 403", code)
	}
	// Viewers still see what has been deleted; it is a read.
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/pages-deleted", "kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("viewer read of deleted list = %d, want 200", code)
	}

	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/pages/dispatch", "kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("member delete = %d, want 200", code)
	}
}
