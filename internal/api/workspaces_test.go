package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
)

func TestWorkspaceCreateGrantsOwnershipToCreator(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	user := addMember(t, pool, wsID, "wanda", "") // a user in no org

	src.id = auth.Identity{UserID: user, Scopes: []string{"read", "write", "admin"}}

	// Before: signing in shows an empty list with nothing to click. That is
	// the gap this endpoint closes.
	var before []WorkspaceSummary
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces", "kiln_valid", nil, &before); code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if len(before) != 0 {
		t.Fatalf("new user already sees %d workspaces", len(before))
	}

	var created struct {
		Slug string `json:"slug"`
		Org  string `json:"org"`
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "handbook", "name": "Handbook"}, &created); code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", code)
	}

	// Creating it makes them an owner, so it is immediately visible and
	// manageable -- a bench they cannot administer would be no better than
	// the empty list.
	var after []WorkspaceSummary
	send(t, srv, http.MethodGet, "/api/v1/workspaces", "kiln_valid", nil, &after)
	if len(after) != 1 || after[0].Slug != "handbook" {
		t.Fatalf("after create the user sees %+v", after)
	}
	// Owning the org is what makes the admin surface reachable -- with the
	// admin scope, which a browser session carries and an automation token
	// opts into at mint time.
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/handbook/connectors", "kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("creator cannot manage sources on their own bench: %d", code)
	}

	// The scope requirement still bites: the same owner with a write-only
	// token may not reshape connectors.
	src.id = auth.Identity{UserID: user, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/handbook/connectors", "kiln_valid", nil, nil); code != http.StatusForbidden {
		t.Errorf("write-only token reached the admin surface: %d", code)
	}
}

// The security property: creating a workspace inside an org you do not own
// must fail, or any authenticated user could plant a bench in another
// tenant's org and administer it by virtue of having created it.
func TestWorkspaceCreateCannotInvadeAnotherOrg(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	outsider := addMember(t, pool, wsID, "otto-ws", "")

	src.id = auth.Identity{UserID: outsider, Scopes: []string{"read", "write"}}
	code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"org": "test-org", "slug": "trojan"}, nil)
	// 404, not 403: distinguishing "exists but not yours" from "does not
	// exist" would let a caller enumerate tenants.
	if code != http.StatusNotFound {
		t.Fatalf("invading another org = %d, want 404", code)
	}

	// Even a member who is not an owner may not add benches to the org.
	member := addMember(t, pool, wsID, "mia-ws", "member")
	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"org": "test-org", "slug": "member-bench"}, nil); code != http.StatusNotFound {
		t.Errorf("non-owner member created a bench in the org: %d", code)
	}

	// An owner can.
	owner := addMember(t, pool, wsID, "olive-ws", "owner")
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"org": "test-org", "slug": "owner-bench"}, nil); code != http.StatusCreated {
		t.Errorf("org owner could not create a bench: %d", code)
	}
}

func TestWorkspaceCreateValidation(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	user := addMember(t, pool, wsID, "vera-ws", "")
	src.id = auth.Identity{UserID: user, Scopes: []string{"read", "write"}}

	// Slugs land in URLs and generated links, so they are validated rather
	// than sanitized: silently rewriting input produces a bench the user
	// cannot find again.
	for _, bad := range []string{"", "Has Spaces", "trailing-", "-leading", "double--dash", "sl/ash", "a..b"} {
		if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
			map[string]any{"slug": bad}, nil); code != http.StatusBadRequest {
			t.Errorf("slug %q accepted with %d, want 400", bad, code)
		}
	}

	// Case is normalized rather than rejected: "Team-Handbook" is what a
	// person types, "team-handbook" is what belongs in a URL.
	var cased struct {
		Slug string `json:"slug"`
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "Mixed-Case"}, &cased); code != http.StatusCreated || cased.Slug != "mixed-case" {
		t.Errorf("mixed-case slug = %d %q, want 201 and lowercase", code, cased.Slug)
	}

	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "taken"}, nil); code != http.StatusCreated {
		t.Fatalf("first create = %d", code)
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "taken"}, nil); code != http.StatusConflict {
		t.Errorf("duplicate slug = %d, want 409", code)
	}

	// A write-scoped token is required: a read-only token must not create.
	src.id = auth.Identity{UserID: user, Scopes: []string{"read"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "read-only-attempt"}, nil); code != http.StatusForbidden {
		t.Errorf("read-only token created a bench: %d", code)
	}
}

func TestValidSlug(t *testing.T) {
	good := []string{"a", "handbook", "team-handbook", "v2", "a1-b2-c3"}
	for _, s := range good {
		if !validSlug(s) {
			t.Errorf("validSlug(%q) = false, want true", s)
		}
	}
	bad := []string{"", "-a", "a-", "a--b", "A", "a b", "a/b", "a_b", "ä"}
	for _, s := range bad {
		if validSlug(s) {
			t.Errorf("validSlug(%q) = true, want false", s)
		}
	}
	long := make([]byte, maxSlugLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if validSlug(string(long)) {
		t.Error("over-long slug accepted")
	}
}

// A workspace created through the API must be immediately usable, not a
// half-built row that 404s until someone runs a build against it.
func TestCreatedWorkspaceIsImmediatelyReadable(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	user := addMember(t, pool, wsID, "ida-ws", "")
	src.id = auth.Identity{UserID: user, Scopes: []string{"read", "write"}}

	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "fresh", "name": "Fresh Bench"}, nil); code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	for _, path := range []string{
		"/api/v1/workspaces/fresh",
		"/api/v1/workspaces/fresh/pages",
		"/api/v1/workspaces/fresh/runs",
	} {
		if code := send(t, srv, http.MethodGet, path, "kiln_valid", nil, nil); code != http.StatusOK {
			t.Errorf("GET %s on a fresh bench = %d, want 200", path, code)
		}
	}
	_ = context.Background()
}
