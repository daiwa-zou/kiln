package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/github"
	"github.com/daiwa-zou/kiln/internal/store"
)

// linkInstallation records, the way a GitHub sign-in does, that the user can
// reach an App installation.
func linkInstallation(t *testing.T, pool *pgxpool.Pool, userID string, installationID int64) {
	t.Helper()
	var inst github.Installation
	inst.ID = installationID
	inst.Account.Login = "acct"
	inst.Account.Type = "Organization"
	if err := store.NewWikiStore(pool).SyncUserInstallations(
		context.Background(), userID, []github.Installation{inst}); err != nil {
		t.Fatal(err)
	}
}

func installationBody(installationID any) map[string]any {
	return map[string]any{
		"kind": "git", "name": "code",
		"config": map[string]any{
			"url": "https://github.com/victim/private.git", "installation_id": installationID,
		},
	}
}

// An installation id is a small guessable integer and naming one makes the
// worker clone with a token minted for it. A bench owner who is not an
// instance admin must therefore hold the installation themselves.
func TestConnectorInstallationMustBelongToCaller(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	owner := addMember(t, pool, wsID, "olga", "owner")
	linkInstallation(t, pool, owner, 111)

	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write", "admin"}}
	const path = "/api/v1/workspaces/demo/connectors"

	// Someone else's installation: refused, and the answer is the same
	// whether or not that installation exists.
	for _, id := range []float64{222, 999999} {
		if code := send(t, srv, http.MethodPost, path, "kiln_valid", installationBody(id), nil); code != http.StatusForbidden {
			t.Fatalf("create with installation %v the caller lacks = %d, want 403", id, code)
		}
	}
	var list []map[string]any
	send(t, srv, http.MethodGet, path, "kiln_valid", nil, &list)
	if len(list) != 0 {
		t.Fatalf("a refused create left a connector behind: %+v", list)
	}

	// Their own installation is accepted.
	var created struct {
		ID string `json:"id"`
	}
	if code := send(t, srv, http.MethodPost, path, "kiln_valid", installationBody(111), &created); code != http.StatusCreated {
		t.Fatalf("create with own installation = %d, want 201", code)
	}

	// Patching the config is the same door: swapping in a foreign id fails,
	// keeping the stored one passes, and a config with no installation passes.
	patch := func(cfg map[string]any) int {
		return send(t, srv, http.MethodPatch, path+"/"+created.ID, "kiln_valid", map[string]any{"config": cfg}, nil)
	}
	url := "https://github.com/victim/private.git"
	if code := patch(map[string]any{"url": url, "installation_id": 222}); code != http.StatusForbidden {
		t.Errorf("patch to a foreign installation = %d, want 403", code)
	}
	if code := patch(map[string]any{"url": url, "installation_id": 111}); code != http.StatusOK {
		t.Errorf("patch keeping own installation = %d, want 200", code)
	}
	if code := patch(map[string]any{"url": url}); code != http.StatusOK {
		t.Errorf("patch dropping the installation = %d, want 200", code)
	}

	// Malformed ids are rejected outright rather than truncated by the worker
	// into a different installation than the one that was checked.
	for name, bad := range map[string]any{"fraction": 111.5, "zero": 0, "negative": -1, "string": "111"} {
		if code := send(t, srv, http.MethodPost, path, "kiln_valid", installationBody(bad), nil); code != http.StatusBadRequest {
			t.Errorf("installation_id %s = %d, want 400", name, code)
		}
	}
}

// An owner may keep editing a connector an admin set up with an installation
// the owner does not hold, as long as the installation id is left as it was.
func TestConnectorPatchKeepsAdminChosenInstallation(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	admin := addMember(t, pool, wsID, "ada", "member")
	owner := addMember(t, pool, wsID, "olga", "owner")
	const path = "/api/v1/workspaces/demo/connectors"

	src.id = auth.Identity{UserID: admin, Admin: true, Scopes: []string{"read", "write", "admin"}}
	var created struct {
		ID string `json:"id"`
	}
	if code := send(t, srv, http.MethodPost, path, "kiln_valid", installationBody(555), &created); code != http.StatusCreated {
		t.Fatalf("admin create = %d, want 201", code)
	}

	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write", "admin"}}
	same := map[string]any{"url": "https://github.com/victim/other.git", "installation_id": 555}
	if code := send(t, srv, http.MethodPatch, path+"/"+created.ID, "kiln_valid", map[string]any{"config": same}, nil); code != http.StatusOK {
		t.Errorf("owner patch keeping admin's installation = %d, want 200", code)
	}
	swapped := map[string]any{"url": "https://github.com/victim/other.git", "installation_id": 556}
	if code := send(t, srv, http.MethodPatch, path+"/"+created.ID, "kiln_valid", map[string]any{"config": swapped}, nil); code != http.StatusForbidden {
		t.Errorf("owner patch swapping installation = %d, want 403", code)
	}
}

func TestConnectorInstallationAdminBypass(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	admin := addMember(t, pool, wsID, "ada", "member")
	src.id = auth.Identity{UserID: admin, Admin: true, Scopes: []string{"read", "write", "admin"}}

	// Instance admins configure connectors for any bench, including with an
	// installation they have not linked themselves.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid",
		installationBody(4242), nil); code != http.StatusCreated {
		t.Fatalf("admin create with installation = %d, want 201", code)
	}
}

func TestConnectorInstallationAuthDisabled(t *testing.T) {
	// No Auth middleware: the loopback-only dev mode in which every caller is
	// an admin and nobody has signed in, so no installation is ever linked.
	srv, _, _ := testServer(t)
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "",
		installationBody(4242), nil); code != http.StatusCreated {
		t.Fatalf("auth-disabled create with installation = %d, want 201", code)
	}
	// Validation still applies in that mode.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "",
		installationBody("4242"), nil); code != http.StatusBadRequest {
		t.Fatalf("auth-disabled malformed installation = %d, want 400", code)
	}
}

func TestConnectorInstallationRefusalDoesNotLeak(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	owner := addMember(t, pool, wsID, "olga", "owner")
	other := addMember(t, pool, wsID, "otto", "member")
	linkInstallation(t, pool, other, 222)
	src.id = auth.Identity{UserID: owner, Scopes: []string{"read", "write", "admin"}}

	// 222 exists and belongs to someone else; 999999 does not exist. The
	// refusals must be indistinguishable.
	msgs := map[string]bool{}
	for _, id := range []float64{222, 999999} {
		raw, err := json.Marshal(installationBody(id))
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/workspaces/demo/connectors", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer kiln_valid")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]string
		_ = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("installation %v: status %d, want 403", id, res.StatusCode)
		}
		msgs[body["error"]] = true
	}
	if len(msgs) != 1 {
		t.Errorf("existing and absent installations answered differently: %v", msgs)
	}
}
