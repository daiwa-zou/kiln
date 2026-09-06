package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

func TestPublishTargetRoundTrip(t *testing.T) {
	srv, _, _ := testServer(t)

	// A bench that does not publish reports that plainly rather than 404ing:
	// most benches never publish, and the UI needs "not configured" to be a
	// normal answer.
	var before map[string]any
	if code := get(t, srv, "/api/v1/workspaces/demo/publish", &before); code != http.StatusOK {
		t.Fatalf("GET publish = %d", code)
	}
	if before["configured"] != false {
		t.Errorf("unconfigured bench reported %v", before)
	}

	var put map[string]any
	if code := send(t, srv, http.MethodPut, "/api/v1/workspaces/demo/publish", "", map[string]any{
		"repo": "https://github.com/example/wiki.git", "branch": "main", "pathPrefix": "wiki",
	}, &put); code != http.StatusOK {
		t.Fatalf("PUT publish = %d (%v)", code, put)
	}
	// The response says the surprising part out loud.
	if note, _ := put["note"].(string); note == "" {
		t.Error("configuring publishing did not warn that the repository is overwritten")
	}

	var after map[string]any
	get(t, srv, "/api/v1/workspaces/demo/publish", &after)
	if after["configured"] != true || after["repo"] != "https://github.com/example/wiki.git" {
		t.Fatalf("target did not round-trip: %v", after)
	}
	if after["branch"] != "main" || after["pathPrefix"] != "wiki" {
		t.Errorf("branch/prefix did not round-trip: %v", after)
	}
	if after["authenticated"] != false {
		t.Errorf("a target with no credential reported authenticated: %v", after)
	}

	// Patching leaves unset fields alone, so toggling enabled does not require
	// resending the repository URL.
	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/publish", "",
		map[string]any{"enabled": false}, nil); code != http.StatusOK {
		t.Fatalf("PATCH publish = %d", code)
	}
	get(t, srv, "/api/v1/workspaces/demo/publish", &after)
	if after["enabled"] != false || after["repo"] != "https://github.com/example/wiki.git" {
		t.Errorf("patch disturbed the rest of the target: %v", after)
	}

	if code := send(t, srv, http.MethodDelete, "/api/v1/workspaces/demo/publish", "", nil, nil); code != http.StatusOK {
		t.Fatalf("DELETE publish = %d", code)
	}
	get(t, srv, "/api/v1/workspaces/demo/publish", &after)
	if after["configured"] != false {
		t.Errorf("target survived deletion: %v", after)
	}
}

// TestPublishRefusesAnUnsafeRemote: the clone policy applies to publishing too,
// and it is checked while someone is looking at the form rather than silently
// on a build hours later.
func TestPublishRefusesAnUnsafeRemote(t *testing.T) {
	srv, _, _ := testServer(t)

	for _, repo := range []string{
		"http://github.com/example/wiki.git",          // not https
		"https://user:token@github.com/example/x.git", // credentials in the URL
		"https://127.0.0.1/example/wiki.git",          // private address space
		"ftp://example.com/x.git",
		"",
	} {
		code, body := putJSON(t, srv, "/api/v1/workspaces/demo/publish",
			map[string]any{"repo": repo})
		if code != http.StatusBadRequest {
			t.Errorf("PUT with repo %q = %d, want 400", repo, code)
		}
		// A refusal has to say why: someone typing a URL into a form needs the
		// reason, and the shared send helper decodes only 2xx bodies.
		if body["error"] == nil {
			t.Errorf("PUT with repo %q gave no reason", repo)
		}
	}
}

// putJSON is send for a request whose response body matters even when it
// fails.
func putJSON(t *testing.T, srv *httptest.Server, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// TestPublishConfigIsAdminOnly: choosing where the wiki is mirrored names an
// external repository and attaches a credential that can write to it, which is
// the same class of decision as configuring what a bench reads.
func TestPublishConfigIsAdminOnly(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)

	member := addMember(t, pool, wsID, "mira-publish", "member")
	src.id = auth.Identity{UserID: member, Scopes: []string{"read", "write"}}

	if code := send(t, srv, http.MethodPut, "/api/v1/workspaces/demo/publish", "kiln_valid",
		map[string]any{"repo": "https://github.com/example/wiki.git"}, nil); code != http.StatusForbidden {
		t.Errorf("member PUT publish = %d, want 403", code)
	}
	// Reading where a bench publishes is not privileged; it is part of knowing
	// what the bench is.
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/publish", "kiln_valid", nil, nil); code != http.StatusOK {
		t.Errorf("member GET publish = %d, want 200", code)
	}
}

// TestPublishReportsTheLastOutcome is what makes a failure findable: it lands
// next to the configuration that caused it, the way a connector's does.
func TestPublishReportsTheLastOutcome(t *testing.T) {
	srv, js, wsID := testServer(t)

	if code := send(t, srv, http.MethodPut, "/api/v1/workspaces/demo/publish", "", map[string]any{
		"repo": "https://github.com/example/wiki.git",
	}, nil); code != http.StatusOK {
		t.Fatal("configure failed")
	}

	if err := js.MarkPublished(t.Context(), wsID, "", "git push: Repository not found."); err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	get(t, srv, "/api/v1/workspaces/demo/publish", &after)
	if after["lastError"] != "git push: Repository not found." {
		t.Errorf("failure not reported: %v", after)
	}

	// A success clears the error and stamps the commit.
	if err := js.MarkPublished(t.Context(), wsID, "abc123def", ""); err != nil {
		t.Fatal(err)
	}
	get(t, srv, "/api/v1/workspaces/demo/publish", &after)
	if after["lastError"] != "" || after["lastCommit"] != "abc123def" {
		t.Errorf("success did not clear the failure: %v", after)
	}
	if after["lastPublished"] == nil {
		t.Error("a successful publish recorded no timestamp")
	}
}

// TestMarkPublishedKeepsTheLastGoodCommit: a later failure must not erase what
// is currently published, so the UI can say both "this broke" and "here is
// what is up there".
func TestMarkPublishedKeepsTheLastGoodCommit(t *testing.T) {
	_, js, wsID := testServer(t)

	if _, err := js.SetPublishTarget(t.Context(), store.PublishTarget{
		WorkspaceID: wsID, RepoURL: "https://github.com/example/wiki.git",
		Branch: "main", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.MarkPublished(t.Context(), wsID, "good123", ""); err != nil {
		t.Fatal(err)
	}
	if err := js.MarkPublished(t.Context(), wsID, "", "network unreachable"); err != nil {
		t.Fatal(err)
	}

	got, err := js.PublishTargetFor(t.Context(), wsID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastCommit != "good123" {
		t.Errorf("LastCommit = %q; a failure erased what is published", got.LastCommit)
	}
	if got.LastError != "network unreachable" {
		t.Errorf("LastError = %q", got.LastError)
	}
}
