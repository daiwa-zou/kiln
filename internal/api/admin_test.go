package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
)

func TestConnectorCRUDIsAdminOnly(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	member := addMember(t, pool, wsID, "mira", "member")

	body := map[string]any{
		"kind": "git", "name": "code",
		"config": map[string]any{"path": "/srv/repos/demo"},
	}

	// An editor role that can write steering must still not shape connectors:
	// a connector config decides what the worker reads.
	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid", body, nil); code != http.StatusForbidden {
		t.Fatalf("member connector create = %d, want 403", code)
	}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/connectors", "kiln_valid", nil, nil); code != http.StatusForbidden {
		t.Fatalf("member connector list = %d, want 403", code)
	}

	// Admin: full CRUD round trip.
	src.id = auth.Identity{UserID: member, Admin: true, Scopes: []string{"read", "write", "admin"}}
	var created struct {
		ID string `json:"id"`
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid", body, &created); code != http.StatusCreated {
		t.Fatalf("admin connector create = %d, want 201", code)
	}

	var list []map[string]any
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/connectors", "kiln_valid", nil, &list); code != http.StatusOK {
		t.Fatalf("admin connector list = %d", code)
	}
	if len(list) != 1 || list[0]["kind"] != "git" || list[0]["enabled"] != true {
		t.Errorf("connector list: %+v", list)
	}

	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/connectors/"+created.ID, "kiln_valid",
		map[string]any{"enabled": false}, nil); code != http.StatusOK {
		t.Fatalf("connector patch = %d", code)
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/connectors/"+created.ID, "kiln_valid", nil, nil); code != http.StatusOK {
		t.Fatalf("connector delete = %d", code)
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/connectors/"+created.ID, "kiln_valid", nil, nil); code != http.StatusNotFound {
		t.Fatalf("double delete = %d, want 404", code)
	}
}

func TestConnectorConfigPolicyAtWriteTime(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	admin := addMember(t, pool, wsID, "ada", "member")
	src.id = auth.Identity{UserID: admin, Admin: true, Scopes: []string{"read", "write", "admin"}}

	cases := map[string]map[string]any{
		"http url": {"kind": "git", "name": "c", "config": map[string]any{"url": "http://example.com/x.git"}},
		"url with creds": {"kind": "git", "name": "c",
			"config": map[string]any{"url": "https://user:pat@example.com/x.git"}},
		"loopback url": {"kind": "git", "name": "c",
			"config": map[string]any{"url": "https://127.0.0.1/x.git"}},
		"git without source":  {"kind": "git", "name": "c", "config": map[string]any{}},
		"upload without path": {"kind": "upload", "name": "d", "config": map[string]any{}},
		"unknown kind":        {"kind": "carrier-pigeon", "name": "p", "config": map[string]any{"path": "/x"}},
	}
	for name, body := range cases {
		if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid", body, nil); code != http.StatusBadRequest {
			t.Errorf("%s: create = %d, want 400", name, code)
		}
	}
}

func TestConnectorPatchValidation(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	admin := addMember(t, pool, wsID, "ada", "member")
	src.id = auth.Identity{UserID: admin, Admin: true, Scopes: []string{"read", "write", "admin"}}

	var created struct {
		ID string `json:"id"`
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid",
		map[string]any{"kind": "git", "name": "code", "config": map[string]any{"path": "/srv/repos/demo"}},
		&created); code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}

	// A patched config passes the same policy as a created one.
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/connectors/"+created.ID, "kiln_valid",
		map[string]any{"config": map[string]any{"url": "http://example.com/x.git"}}, nil); code != http.StatusBadRequest {
		t.Errorf("patch to http url = %d, want 400", code)
	}
	// A valid config patch, plus name, credential clear, and trigger change.
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/connectors/"+created.ID, "kiln_valid",
		map[string]any{
			"config": map[string]any{"path": "/srv/repos/other"},
			"name":   "renamed", "credentialId": "", "triggerMode": "poll",
		}, nil); code != http.StatusOK {
		t.Errorf("valid patch = %d, want 200", code)
	}
	var list []map[string]any
	send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/connectors", "kiln_valid", nil, &list)
	if len(list) != 1 || list[0]["name"] != "renamed" || list[0]["triggerMode"] != "poll" {
		t.Errorf("patched connector: %+v", list)
	}

	// Unknown trigger mode and unknown ids answer distinctly.
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/connectors/"+created.ID, "kiln_valid",
		map[string]any{"triggerMode": "carrier-pigeon"}, nil); code != http.StatusBadRequest {
		t.Errorf("bad trigger patch = %d, want 400", code)
	}
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/connectors/00000000-0000-0000-0000-000000000000", "kiln_valid",
		map[string]any{"config": map[string]any{"path": "/x"}}, nil); code != http.StatusNotFound {
		t.Errorf("patch missing connector = %d, want 404", code)
	}
}

func TestConnectorRejectsForeignCredential(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	admin := addMember(t, pool, wsID, "ada", "member")
	src.id = auth.Identity{UserID: admin, Admin: true, Scopes: []string{"read", "write", "admin"}}

	// A credential in a different org: referencing it from this workspace's
	// connectors must read as absent, never attach.
	ctx := context.Background()
	var otherOrg, foreignCred string
	if err := pool.QueryRow(ctx,
		`INSERT INTO orgs (name, slug) VALUES ('other','other') RETURNING id`).Scan(&otherOrg); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO credentials (org_id, kind, ciphertext, nonce)
		VALUES ($1, 'git_pat', '\x00'::bytea, '\x00'::bytea) RETURNING id`, otherOrg).Scan(&foreignCred); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{
		"kind": "git", "name": "code", "credentialId": foreignCred,
		"config": map[string]any{"path": "/srv/repos/demo"},
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid", body, nil); code != http.StatusNotFound {
		t.Fatalf("create with foreign credential = %d, want 404", code)
	}

	// Same boundary on patch: create clean, then try to attach the secret.
	var created struct {
		ID string `json:"id"`
	}
	delete(body, "credentialId")
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/connectors", "kiln_valid", body, &created); code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/connectors/"+created.ID, "kiln_valid",
		map[string]any{"credentialId": foreignCred}, nil); code != http.StatusNotFound {
		t.Errorf("patch with foreign credential = %d, want 404", code)
	}

	// A same-org credential attaches fine.
	var cred struct {
		ID string `json:"id"`
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/credentials", "kiln_valid",
		map[string]string{"kind": "git_pat", "secret": "tok"}, &cred); code != http.StatusCreated {
		t.Fatalf("credential create = %d", code)
	}
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/connectors/"+created.ID, "kiln_valid",
		map[string]any{"credentialId": cred.ID}, nil); code != http.StatusOK {
		t.Errorf("patch with own credential = %d, want 200", code)
	}
}

func TestCredentialCreateValidation(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	admin := addMember(t, pool, wsID, "ada", "member")
	src.id = auth.Identity{UserID: admin, Admin: true, Scopes: []string{"read", "write", "admin"}}

	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/credentials", "kiln_valid",
		map[string]string{"kind": "git_pat", "secret": "   "}, nil); code != http.StatusBadRequest {
		t.Errorf("blank secret = %d, want 400", code)
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/credentials/00000000-0000-0000-0000-000000000000",
		"kiln_valid", nil, nil); code != http.StatusNotFound {
		t.Errorf("delete missing credential = %d, want 404", code)
	}
}

func TestCredentialsAreWriteOnly(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	admin := addMember(t, pool, wsID, "ada", "member")
	src.id = auth.Identity{UserID: admin, Admin: true, Scopes: []string{"read", "write", "admin"}}

	secret := "ghp_super_secret_token_value"
	var created struct {
		ID string `json:"id"`
	}
	code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/credentials", "kiln_valid",
		map[string]string{"kind": "git_pat", "secret": secret}, &created)
	if code != http.StatusCreated || created.ID == "" {
		t.Fatalf("credential create = %d (%+v)", code, created)
	}

	// The listing carries metadata only; the secret is never readable back.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/workspaces/demo/credentials", nil)
	req.Header.Set("Authorization", "Bearer kiln_valid")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 1<<12)
	n, _ := res.Body.Read(buf)
	if strings.Contains(string(buf[:n]), secret) {
		t.Fatal("credential listing leaked the secret")
	}

	// The database holds only ciphertext.
	var stored []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT ciphertext FROM credentials WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), secret) {
		t.Fatal("credential stored in plaintext")
	}

	// Unknown kinds are rejected; deletion works once.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/credentials", "kiln_valid",
		map[string]string{"kind": "aws_key", "secret": "x"}, nil); code != http.StatusBadRequest {
		t.Errorf("unknown credential kind = %d, want 400", code)
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/credentials/"+created.ID, "kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("credential delete = %d", code)
	}
	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/credentials/"+created.ID, "kiln_valid", nil, nil); code != http.StatusNotFound {
		t.Errorf("credential double delete = %d, want 404", code)
	}
}

func TestCredentialCreateWithoutKeyringIs503(t *testing.T) {
	srv, _, _ := testServer(t) // testServer wires no keyring and no auth (admin-by-default)
	code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/credentials", "",
		map[string]string{"kind": "git_pat", "secret": "x"}, nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("credential create without master key = %d, want 503", code)
	}
}
