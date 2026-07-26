package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
)

// send issues a JSON request with an optional bearer token and decodes a JSON
// response when into is non-nil.
func send(t *testing.T, srv *httptest.Server, method, path, token string, body, into any) int {
	t.Helper()

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	if into != nil && res.StatusCode < 300 {
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return res.StatusCode
}

func TestSteeringRoundTripThroughTheAPI(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	code := send(t, srv, http.MethodPut, "/api/v1/workspaces/demo/steering/purpose", "",
		map[string]string{"body": "Document the dispatch path for on-call."}, nil)
	if code != http.StatusOK {
		t.Fatalf("PUT steering = %d, want 200", code)
	}
	if code := send(t, srv, http.MethodPut, "/api/v1/workspaces/demo/steering/bogus", "",
		map[string]string{"body": "x"}, nil); code != http.StatusNotFound {
		t.Errorf("PUT unknown steering kind = %d, want 404", code)
	}

	var docs map[string]string
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/steering", "", nil, &docs); code != http.StatusOK {
		t.Fatalf("GET steering = %d, want 200", code)
	}
	if docs["purpose"] != "Document the dispatch path for on-call." {
		t.Errorf("purpose = %q after round trip", docs["purpose"])
	}

	// What the API wrote must be exactly what the pipeline injects.
	steering, err := js.LoadSteering(context.Background(), ws)
	if err != nil {
		t.Fatalf("LoadSteering: %v", err)
	}
	if steering.Purpose == "" {
		t.Error("steering written via the API never reached the prompt loader")
	}
}

func TestCorrectionsThroughTheAPI(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	var created map[string]string
	code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/corrections/ripple", "",
		map[string]string{"body": "Ripple does not own retries; beacon does."}, &created)
	if code != http.StatusCreated {
		t.Fatalf("POST correction = %d, want 201", code)
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/corrections/no-such-page", "",
		map[string]string{"body": "x"}, nil); code != http.StatusNotFound {
		t.Errorf("correction on missing page = %d, want 404", code)
	}
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/corrections/ripple", "",
		map[string]string{"body": "   "}, nil); code != http.StatusBadRequest {
		t.Errorf("blank correction = %d, want 400", code)
	}

	var list []map[string]any
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/corrections/ripple", "", nil, &list); code != http.StatusOK {
		t.Fatalf("GET corrections = %d, want 200", code)
	}
	if len(list) != 1 {
		t.Fatalf("corrections listed = %d, want 1", len(list))
	}

	if code := send(t, srv, http.MethodPatch, "/api/v1/workspaces/demo/correction/"+created["id"], "",
		map[string]any{"active": false}, nil); code != http.StatusOK {
		t.Fatalf("PATCH correction = %d, want 200", code)
	}
	steering, err := js.LoadSteering(context.Background(), ws)
	if err != nil {
		t.Fatalf("LoadSteering: %v", err)
	}
	if len(steering.Corrections["ripple"]) != 0 {
		t.Error("deactivated correction still reaches the prompt")
	}
}

func TestReviewQueueThroughTheAPI(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	if err := js.RecordRun(context.Background(), jobs.RunSummary{
		WorkspaceID: ws, Trigger: "manual", Status: jobs.StatusSucceeded,
		Reviews: []jobs.ReviewNote{{
			Kind: "uncertain", Title: "Retry ownership unclear",
			Detail: "two candidates", Unit: diff.Key("module:ripple"),
		}},
	}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	var reviews []map[string]any
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/reviews", "", nil, &reviews); code != http.StatusOK {
		t.Fatalf("GET reviews = %d, want 200", code)
	}
	if len(reviews) != 1 {
		t.Fatalf("open reviews = %d, want 1", len(reviews))
	}
	id, _ := reviews[0]["id"].(string)

	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/reviews/"+id+"/resolve", "",
		map[string]string{"action": "dismiss"}, nil); code != http.StatusOK {
		t.Fatalf("resolve = %d, want 200", code)
	}
	// Second resolution: the item is no longer open.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/reviews/"+id+"/resolve", "",
		map[string]string{"action": "dismiss"}, nil); code != http.StatusNotFound {
		t.Errorf("second resolve = %d, want 404", code)
	}

	// The inbox is empty again; history remains reachable with status=.
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/reviews", "", nil, &reviews); code != http.StatusOK {
		t.Fatal("GET reviews after resolve failed")
	}
	if len(reviews) != 0 {
		t.Errorf("open reviews after resolve = %d, want 0", len(reviews))
	}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/reviews?status=", "", nil, &reviews); code != http.StatusOK || len(reviews) != 1 {
		t.Errorf("all-status reviews = %d items (code %d), want the resolved one", len(reviews), code)
	}
}

func TestBacklinksThroughTheAPI(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)
	_ = ws

	var back []PageSummary
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/backlinks/ripple", "", nil, &back); code != http.StatusOK {
		t.Fatalf("GET backlinks = %d, want 200", code)
	}
	// seed() writes architecture -> [[ripple]].
	if len(back) != 1 || back[0].Slug != "architecture" {
		t.Errorf("backlinks = %+v, want [architecture]", back)
	}
}

func TestWriteRouteBodiesAreBounded(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)
	_ = ws

	// Steering and corrections are prompt-injected verbatim; an unbounded
	// body would be an unbounded prompt.
	big := strings.Repeat("x", maxSteeringBytes+1024)
	code := send(t, srv, http.MethodPut, "/api/v1/workspaces/demo/steering/purpose", "",
		map[string]string{"body": big}, nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize steering = %d, want 413", code)
	}
}

func TestWriteScopeIsRequiredForMutation(t *testing.T) {
	// Hermetic: a read-scope token is rejected by the middleware's scope
	// check, before any handler or database is touched -- exactly the
	// pre-wired write branch the routes now exercise.
	srv := httptest.NewServer((&Server{
		Writes: (*fakeWrites)(nil),
		Auth:   &auth.Middleware{Source: &stubSource{id: auth.Identity{UserID: "u1", Scopes: []string{"read"}}}},
	}).Router())
	defer srv.Close()

	code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/reviews/x/resolve", "kiln_valid",
		map[string]string{"action": "dismiss"}, nil)
	if code != http.StatusForbidden {
		t.Errorf("write with read scope = %d, want 403 from the scope check", code)
	}
}
