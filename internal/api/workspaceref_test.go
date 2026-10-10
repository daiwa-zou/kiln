package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

type ambiguityBody struct {
	Error      string `json:"error"`
	Candidates []struct {
		Ref string `json:"ref"`
		ID  string `json:"id"`
	} `json:"candidates"`
}

// ambiguousStore answers every workspace lookup as the real store does when a
// bare slug matches two visible benches, so the handler's mapping to a 409 is
// covered without a database.
type ambiguousStore struct{ errStore }

func (ambiguousStore) ResolveWorkspace(_ context.Context, ref, _ string, _ bool) (store.WorkspaceRow, error) {
	if ref == "docs" {
		return store.WorkspaceRow{}, &store.AmbiguousWorkspaceError{
			Ref: "docs",
			Candidates: []store.WorkspaceCandidate{
				{ID: "11111111-1111-1111-1111-111111111111", Org: "evil-co", Slug: "docs"},
				{ID: "22222222-2222-2222-2222-222222222222", Org: "good-co", Slug: "docs"},
			},
		}
	}
	return store.WorkspaceRow{ID: ref, Org: "o", Slug: "s"}, nil
}

func TestAmbiguousBareSlugIs409WithCandidates(t *testing.T) {
	srv := httptest.NewServer((&Server{Store: ambiguousStore{}}).Router())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/v1/workspaces/docs/pages")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("ambiguous bare slug = %d, want 409", res.StatusCode)
	}
	var body ambiguityBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error == "" || len(body.Candidates) != 2 {
		t.Fatalf("body = %+v, want an error and both candidates", body)
	}
	if body.Candidates[0].Ref != "evil-co/docs" || body.Candidates[0].ID != "11111111-1111-1111-1111-111111111111" ||
		body.Candidates[1].Ref != "good-co/docs" || body.Candidates[1].ID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("candidates = %+v", body.Candidates)
	}

	// An id is never ambiguous.
	if code := get(t, srv, "/api/v1/workspaces/22222222-2222-2222-2222-222222222222", nil); code != http.StatusOK {
		t.Fatalf("by id = %d, want 200", code)
	}
}

// The wire format of an org/slug reference: "org~slug" in a path segment, with
// a percent-encoded slash accepted as well.
func TestWorkspaceRefParsing(t *testing.T) {
	for in, want := range map[string]string{
		"docs":                                 "docs",
		"good-co~docs":                         "good-co/docs",
		"good-co%2Fdocs":                       "good-co/docs",
		"good-co%2fdocs":                       "good-co/docs",
		"11111111-1111-1111-1111-111111111111": "11111111-1111-1111-1111-111111111111",
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/x", nil)
		rc := chiCtx(r, in)
		if got := workspaceRef(rc); got != want {
			t.Errorf("workspaceRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// End to end against a real database: the attack scenario from the review.
// The attacker's org has the older "demo" bench and adds the victim.
func TestWorkspaceReferenceFormsEndToEnd(t *testing.T) {
	srv, pool, goodID, src := authedServer(t)
	ctx := context.Background()
	js := store.NewWikiStore(pool)

	evilID, err := js.EnsureWorkspace(ctx, "evil-co", "demo", "Evil Demo")
	if err != nil {
		t.Fatal(err)
	}
	victim := addMember(t, pool, goodID, "victim", "member")
	if _, err := pool.Exec(ctx, `
		INSERT INTO org_members (org_id, user_id, role)
		SELECT org_id, $2, 'member' FROM workspaces WHERE id = $1`, evilID, victim); err != nil {
		t.Fatal(err)
	}
	loner := addMember(t, pool, goodID, "loner", "member")

	src.id = auth.Identity{UserID: victim, Scopes: []string{"read"}}
	var amb ambiguityBody
	res := rawGet(t, srv, "/api/v1/workspaces/demo", "kiln_valid")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("bare slug for a user in both orgs = %d, want 409", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&amb); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	got := map[string]string{}
	for _, c := range amb.Candidates {
		got[c.Ref] = c.ID
	}
	if len(got) != 2 || got["test-org/demo"] != goodID || got["evil-co/demo"] != evilID {
		t.Fatalf("candidates = %v", got)
	}

	for name, tc := range map[string]struct{ path, wantID string }{
		"id (good)":     {"/api/v1/workspaces/" + goodID, goodID},
		"id (evil)":     {"/api/v1/workspaces/" + evilID, evilID},
		"org~slug":      {"/api/v1/workspaces/test-org~demo", goodID},
		"org~slug evil": {"/api/v1/workspaces/evil-co~demo", evilID},
		"org%2Fslug":    {"/api/v1/workspaces/evil-co%2Fdemo", evilID},
	} {
		var ws WorkspaceSummary
		res := rawGet(t, srv, tc.path, "kiln_valid")
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: %s = %d, want 200", name, tc.path, res.StatusCode)
			res.Body.Close()
			continue
		}
		if err := json.NewDecoder(res.Body).Decode(&ws); err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if ws.ID != tc.wantID || ws.Org == "" {
			t.Errorf("%s: got %+v, want id %s with an org", name, ws, tc.wantID)
		}
	}

	// A user in only one org keeps the bare slug, and cannot reach the other
	// org's bench by any form: not found, never forbidden.
	src.id = auth.Identity{UserID: loner, Scopes: []string{"read"}}
	if res := rawGet(t, srv, "/api/v1/workspaces/demo", "kiln_valid"); res.StatusCode != http.StatusOK {
		t.Errorf("single-org bare slug = %d, want 200", res.StatusCode)
	} else {
		res.Body.Close()
	}
	for _, p := range []string{"/api/v1/workspaces/" + evilID, "/api/v1/workspaces/evil-co~demo"} {
		res := rawGet(t, srv, p, "kiln_valid")
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s from outside the org = %d, want 404", p, res.StatusCode)
		}
	}
}

func rawGet(t *testing.T, srv *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// chiCtx attaches a {workspace} route parameter the way the router would.
func chiCtx(r *http.Request, workspace string) *http.Request {
	rc := chi.NewRouteContext()
	rc.URLParams.Add("workspace", workspace)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
}
