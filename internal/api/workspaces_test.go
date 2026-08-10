package api

import (
	"net/http"
	"testing"
)

// The delete route is the only thing in the API that destroys work outright,
// so the confirmation is part of its contract rather than a nicety the UI
// happens to implement. A caller that does not name the bench must not be able
// to delete it by accident.
func TestWorkspaceDeleteRequiresTheSlugBack(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	for _, tc := range []struct {
		name string
		body any
		want int
	}{
		{"no body at all", nil, http.StatusBadRequest},
		{"empty slug", map[string]string{"slug": ""}, http.StatusBadRequest},
		{"a different bench's slug", map[string]string{"slug": "not-demo"}, http.StatusBadRequest},
		{"the slug, with stray whitespace", map[string]string{"slug": "  demo  "}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo", "", tc.body, nil); code != tc.want {
				t.Fatalf("want %d, got %d", tc.want, code)
			}
		})
	}

	// It is really gone, and the reader is told so in the same words as a
	// bench that never existed.
	if code := get(t, srv, "/api/v1/workspaces/demo", nil); code != http.StatusNotFound {
		t.Fatalf("deleted bench still resolves: %d", code)
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo", "",
		map[string]string{"slug": "demo"}, nil); code != http.StatusNotFound {
		t.Fatalf("second delete: want 404, got %d", code)
	}
}

// Everything scoped to the bench goes with it. This is the cascade the store
// test proves at the SQL level, checked here through the surface a user
// actually touches.
func TestWorkspaceDeleteTakesItsPagesWithIt(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	var pages []map[string]any
	if code := get(t, srv, "/api/v1/workspaces/demo/pages", &pages); code != http.StatusOK {
		t.Fatalf("seeded pages should list: %d", code)
	}
	if len(pages) == 0 {
		t.Fatal("fixture wrote no pages, so this proves nothing")
	}

	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo", "",
		map[string]string{"slug": "demo"}, nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if code := get(t, srv, "/api/v1/workspaces/demo/pages", nil); code != http.StatusNotFound {
		t.Fatalf("pages outlived their bench: %d", code)
	}

	var list []map[string]any
	if code := get(t, srv, "/api/v1/workspaces", &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	for _, w := range list {
		if w["slug"] == "demo" {
			t.Fatal("deleted bench still appears in the workspace list")
		}
	}
}
