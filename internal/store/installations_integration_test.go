package store

import (
	"context"
	"testing"

	"github.com/daiwa-zou/kiln/internal/github"
)

func installation(id int64, login string) github.Installation {
	var i github.Installation
	i.ID = id
	i.Account.Login = login
	i.Account.Type = "Organization"
	return i
}

func TestInstallationOwnershipQueries(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	wsA, err := s.EnsureWorkspace(ctx, "org-a", "alpha", "Alpha")
	if err != nil {
		t.Fatal(err)
	}
	wsB, err := s.EnsureWorkspace(ctx, "org-b", "beta", "Beta")
	if err != nil {
		t.Fatal(err)
	}

	user := func(login string, admin bool) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (login, is_admin) VALUES ($1, $2) RETURNING id`, login, admin).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	join := func(ws, uid, role string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO org_members (org_id, user_id, role)
			SELECT org_id, $2, $3 FROM workspaces WHERE id = $1`, ws, uid, role); err != nil {
			t.Fatal(err)
		}
	}

	alice := user("alice", false) // member of org-a, holds installation 111
	bob := user("bob", false)     // member of org-b, holds installation 222
	root := user("root", true)    // instance admin, member of nothing, holds 333
	join(wsA, alice, "owner")
	join(wsB, bob, "owner")

	for uid, inst := range map[string]int64{alice: 111, bob: 222, root: 333} {
		if err := s.SyncUserInstallations(ctx, uid, []github.Installation{installation(inst, "acct")}); err != nil {
			t.Fatal(err)
		}
	}
	// An installation GitHub told us about (webhook) that no user is linked to.
	if err := s.UpsertGitHubInstallationEvent(ctx, 999, "victim", "Organization", false); err != nil {
		t.Fatal(err)
	}

	hasCases := []struct {
		name string
		user string
		inst int64
		want bool
	}{
		{"own installation", alice, 111, true},
		{"another user's installation", alice, 222, false},
		{"known installation, nobody linked", alice, 999, false},
		{"unknown installation", alice, 5, false},
	}
	for _, c := range hasCases {
		got, err := s.UserHasInstallation(ctx, c.user, c.inst)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("UserHasInstallation(%s) = %v, want %v", c.name, got, c.want)
		}
	}

	servesCases := []struct {
		name string
		ws   string
		inst int64
		want bool
	}{
		{"member's installation serves its org", wsA, 111, true},
		{"other org's member does not serve this org", wsA, 222, false},
		{"instance admin's installation serves any org", wsA, 333, true},
		{"installation no user holds", wsA, 999, false},
		{"unknown installation", wsA, 5, false},
		{"second org sees its own member", wsB, 222, true},
		{"second org does not see first org's", wsB, 111, false},
	}
	for _, c := range servesCases {
		got, err := s.InstallationServesWorkspace(ctx, c.ws, c.inst)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("InstallationServesWorkspace(%s) = %v, want %v", c.name, got, c.want)
		}
	}

	// Leaving the org, or losing the installation on GitHub and signing in
	// again, ends the installation's standing in the workspace.
	if err := s.RemoveOrgMember(ctx, wsA, alice); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.InstallationServesWorkspace(ctx, wsA, 111); got {
		t.Error("removed member's installation still serves the org")
	}
	if err := s.SyncUserInstallations(ctx, bob, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.InstallationServesWorkspace(ctx, wsB, 222); got {
		t.Error("revoked installation still serves the org")
	}
}
