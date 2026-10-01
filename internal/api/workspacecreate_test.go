package api

import (
	"net/http"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
)

// Self-serve bench creation is the door a first-time user comes through: sign
// in, belong to no org, and have something to click. Nothing about it was
// covered -- not the handler, not the slug rule, and not the store's
// authorization, which is the half that keeps a bench out of someone else's
// org.

func TestWorkspaceCreateMakesAUsableBench(t *testing.T) {
	srv, _, _ := testServer(t)

	var made struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Org  string `json:"org"`
		Name string `json:"name"`
	}
	code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "",
		map[string]any{"slug": "Team-Handbook", "name": "Team Handbook"}, &made)
	if code != http.StatusCreated {
		t.Fatalf("POST workspaces = %d, want 201", code)
	}
	// Case is normalized because that is what anyone typing it expects.
	if made.Slug != "team-handbook" {
		t.Errorf("slug = %q, want it lowercased", made.Slug)
	}
	// No org given: the bench's own slug becomes one, so a first-time user
	// never has to understand orgs.
	if made.Org != "team-handbook" {
		t.Errorf("org = %q, want the bench's own slug as its org", made.Org)
	}
	if made.ID == "" || made.Name != "Team Handbook" {
		t.Errorf("created = %+v", made)
	}

	// Usable immediately: the wiki row is created up front, so the reader
	// resolves the bench rather than 404ing on one that visibly exists.
	var ws struct {
		Slug      string `json:"slug"`
		PageCount int    `json:"pageCount"`
	}
	if code := get(t, srv, "/api/v1/workspaces/team-handbook", &ws); code != http.StatusOK {
		t.Fatalf("GET the new bench = %d, want 200", code)
	}
	if ws.Slug != "team-handbook" || ws.PageCount != 0 {
		t.Errorf("new bench reads as %+v", ws)
	}
}

func TestWorkspaceCreateNamesTheBenchAfterItsSlugByDefault(t *testing.T) {
	srv, _, _ := testServer(t)

	var made struct {
		Name string `json:"name"`
	}
	code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "",
		map[string]any{"slug": "notes"}, &made)
	if code != http.StatusCreated {
		t.Fatalf("POST workspaces = %d, want 201", code)
	}
	if made.Name != "notes" {
		t.Errorf("name = %q, want the slug when none was given", made.Name)
	}
}

func TestWorkspaceCreateRejectsNamesItCannotGiveBack(t *testing.T) {
	// Invalid characters are refused rather than stripped: stripping produces
	// a bench under a name the user never chose and cannot guess later.
	srv, _, _ := testServer(t)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"empty slug", map[string]any{"slug": ""}},
		{"spaces", map[string]any{"slug": "team handbook"}},
		{"underscores", map[string]any{"slug": "team_handbook"}},
		{"leading dash", map[string]any{"slug": "-handbook"}},
		{"trailing dash", map[string]any{"slug": "handbook-"}},
		{"double dash", map[string]any{"slug": "team--handbook"}},
		{"slashes", map[string]any{"slug": "team/handbook"}},
		{"too long", map[string]any{"slug": longSlug(64)}},
		{"bad org", map[string]any{"slug": "handbook", "org": "Not An Org"}},
		{"name too long", map[string]any{"slug": "handbook", "name": longSlug(201)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "", tc.body, nil); code != http.StatusBadRequest {
				t.Errorf("POST %v = %d, want 400", tc.body, code)
			}
		})
	}

	// The boundary itself is allowed, so the rule rejects too-long and nothing
	// shorter.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "",
		map[string]any{"slug": longSlug(63)}, nil); code != http.StatusCreated {
		t.Errorf("a 63-character slug = %d, want 201", code)
	}
}

func longSlug(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestWorkspaceCreateRefusesADuplicateSlug(t *testing.T) {
	srv, _, _ := testServer(t)

	body := map[string]any{"slug": "handbook", "org": "handbook"}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "", body, nil); code != http.StatusCreated {
		t.Fatalf("first create = %d, want 201", code)
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "", body, nil); code != http.StatusConflict {
		t.Errorf("second create = %d, want 409", code)
	}
}

func TestWorkspaceCreateBindsTheCreatorAsAnOwner(t *testing.T) {
	// The membership is the whole point: without it the creator would land
	// back on the empty list they were trying to escape.
	srv, pool, wsID, src := authedServer(t)
	creator := addMember(t, pool, wsID, "cora", "")

	// The scopes a browser session carries, which is the path self-serve
	// creation actually arrives on.
	src.id = auth.Identity{UserID: creator, Scopes: []string{"read", "write", "admin"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "coras-bench"}, nil); code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", code)
	}

	// Visible to them now, and to them only -- the pre-existing demo bench
	// belongs to an org they are not in.
	var list []struct {
		Slug string `json:"slug"`
	}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces", "kiln_valid", nil, &list); code != http.StatusOK {
		t.Fatalf("GET workspaces = %d", code)
	}
	if len(list) != 1 || list[0].Slug != "coras-bench" {
		t.Fatalf("creator sees %+v, want only the bench they just made", list)
	}

	// And owning it means they can do the owner-only thing to it: the bench is
	// theirs to configure, not merely to look at.
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/coras-bench/connectors",
		"kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("creator reading their own bench's connectors = %d, want 200", code)
	}
}

func TestWorkspaceCreateCannotPlantABenchInSomeoneElsesOrg(t *testing.T) {
	// Authorization lives in the store transaction, not the handler: an org
	// that already exists may only be added to by an owner. Without it, any
	// signed-in user could plant a bench in another tenant's org and, being
	// its creator, manage it.
	srv, pool, wsID, src := authedServer(t)
	outsider := addMember(t, pool, wsID, "otto", "")

	src.id = auth.Identity{UserID: outsider, Scopes: []string{"read", "write"}}
	code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "trespass", "org": "test-org"}, nil)
	// 404, not 403: telling a caller that an org exists but is not theirs is
	// a tenant-enumeration oracle.
	if code != http.StatusNotFound {
		t.Errorf("outsider creating in another org = %d, want 404", code)
	}

	// An owner of that org may.
	owner := addMember(t, pool, wsID, "olga", "owner")
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "second-bench", "org": "test-org"}, nil); code != http.StatusCreated {
		t.Errorf("owner creating in their own org = %d, want 201", code)
	}

	// So may an instance admin, with no membership row at all.
	src.id = auth.Identity{UserID: outsider, Admin: true, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "third-bench", "org": "test-org"}, nil); code != http.StatusCreated {
		t.Errorf("admin creating in any org = %d, want 201", code)
	}
}

func TestWorkspaceCreateNeedsAnAuthenticatedWriter(t *testing.T) {
	// There is no anonymous ownership to grant, so the route demands a caller
	// even though the rest of the bench is readable without one.
	srv, pool, wsID, src := authedServer(t)

	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "",
		map[string]any{"slug": "anon"}, nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous create = %d, want 401", code)
	}

	reader := addMember(t, pool, wsID, "rita", "")
	src.id = auth.Identity{UserID: reader, Scopes: []string{"read"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces", "kiln_valid",
		map[string]any{"slug": "read-only"}, nil); code != http.StatusForbidden {
		t.Errorf("read-scoped create = %d, want 403", code)
	}
}
