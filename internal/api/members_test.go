package api

import (
	"net/http"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
)

func TestMembershipIsOwnerGated(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	owner := addMember(t, pool, wsID, "olive", "owner")
	member := addMember(t, pool, wsID, "mira", "member")
	addMember(t, pool, wsID, "nate", "") // exists, no org

	// A member writes content but does not shape the org.
	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/members", "kiln_valid", nil, nil); code != http.StatusForbidden {
		t.Fatalf("member lists members = %d, want 403", code)
	}

	// An owner manages membership without any instance-admin bit.
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write", "admin"}}
	var members []map[string]any
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/members", "kiln_valid", nil, &members); code != http.StatusOK {
		t.Fatalf("owner lists members = %d", code)
	}
	if len(members) != 2 {
		t.Errorf("members = %d, want 2", len(members))
	}

	// Add the org-less user; unknown logins 404 with guidance.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/members", "kiln_valid",
		map[string]string{"login": "nate", "role": "viewer"}, nil); code != http.StatusCreated {
		t.Errorf("add member = %d", code)
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/members", "kiln_valid",
		map[string]string{"login": "ghost", "role": "viewer"}, nil); code != http.StatusNotFound {
		t.Errorf("add unknown login = %d, want 404", code)
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/members", "kiln_valid",
		map[string]string{"login": "nate", "role": "sovereign"}, nil); code != http.StatusBadRequest {
		t.Errorf("bogus role = %d, want 400", code)
	}

	// Promote, then remove.
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/members/"+member, "kiln_valid",
		map[string]string{"role": "owner"}, nil); code != http.StatusOK {
		t.Errorf("promote = %d", code)
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/members/"+member, "kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("remove = %d", code)
	}
}

func TestOwnersReachAdminSurfaces(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	owner := addMember(t, pool, wsID, "olive", "owner")
	member := addMember(t, pool, wsID, "mira", "member")

	body := map[string]any{
		"kind": "git", "name": "code",
		"config": map[string]any{"path": "/srv/repos/demo"},
	}
	// The M3 tenancy line: owners shape their own org's connectors...
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write", "admin"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid", body, nil); code != http.StatusCreated {
		t.Errorf("owner connector create = %d, want 201", code)
	}
	// ...and members still do not.
	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid", body, nil); code != http.StatusForbidden {
		t.Errorf("member connector create = %d, want 403", code)
	}
}

func TestLastOwnerCannotLockOut(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	owner := addMember(t, pool, wsID, "olive", "owner")
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write", "admin"}}

	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/members/"+owner, "kiln_valid",
		map[string]string{"role": "member"}, nil); code != http.StatusConflict {
		t.Errorf("demote last owner = %d, want 409", code)
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/members/"+owner, "kiln_valid", nil, nil); code != http.StatusConflict {
		t.Errorf("remove last owner = %d, want 409", code)
	}

	// An instance admin may break the glass.
	src.id = auth.Identity{UserID: owner, Admin: true, Scopes: []string{"read", "write", "admin"}}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/members/"+owner, "kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("admin removes last owner = %d, want 200", code)
	}
}
