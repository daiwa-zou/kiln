package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/store"
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

// Asking for a gap is the one place a human adds to the review queue directly.
// It must file exactly one question however many times it is asked, and must
// refuse a slug the wiki never asked for -- the queue is what people read when
// deciding what to build.
func TestAskingForAGapFilesOneReview(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	// The seeded wiki links to a page nobody has written.
	const ask = "/api/v1/workspaces/demo/gaps/never-written/request"
	if code := send(t, srv, http.MethodPost, ask, "", map[string]any{}, nil); code != http.StatusAccepted {
		t.Fatalf("ask = %d, want 202", code)
	}

	var reviews []map[string]any
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/reviews", "", nil, &reviews); code != http.StatusOK {
		t.Fatalf("GET reviews = %d, want 200", code)
	}
	if len(reviews) != 1 {
		t.Fatalf("open reviews = %d, want 1", len(reviews))
	}
	if kind, _ := reviews[0]["kind"].(string); kind != "gap" {
		t.Errorf("kind = %q, want gap", kind)
	}
	if title, _ := reviews[0]["title"].(string); !strings.Contains(title, "never-written") {
		t.Errorf("title = %q, want it to name the missing page", title)
	}
	// The detail counts the pages that wanted it, so the question carries its
	// own evidence rather than sending the reader back to the Gaps view.
	if detail, _ := reviews[0]["detail"].(string); !strings.Contains(detail, "1 page link") {
		t.Errorf("detail = %q, want the inbound count", detail)
	}

	// Asking twice asks once: the queue holds a question, not a click count.
	if code := send(t, srv, http.MethodPost, ask, "", map[string]any{}, nil); code != http.StatusAccepted {
		t.Fatalf("second ask = %d, want 202", code)
	}
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/reviews", "", nil, &reviews); code != http.StatusOK {
		t.Fatal("GET reviews after asking twice failed")
	}
	if len(reviews) != 1 {
		t.Errorf("open reviews after asking twice = %d, want 1", len(reviews))
	}

	// A slug nothing links to is not a gap, so there is nothing to ask for.
	if code := send(t, srv, http.MethodPost,
		"/api/v1/workspaces/demo/gaps/not-a-gap/request", "", map[string]any{}, nil); code != http.StatusNotFound {
		t.Errorf("ask for a non-gap = %d, want 404", code)
	}
}

// fileReview puts one open review item of a given kind on the bench and
// returns its id, so a test can act on it through the API.
func fileReview(t *testing.T, js *store.WikiStore, ws, kind, title string) string {
	t.Helper()
	if err := js.FileReview(context.Background(), ws, kind, title, "detail"); err != nil {
		t.Fatalf("FileReview: %v", err)
	}
	rows, err := js.ListReviews(context.Background(), ws, "open", 50, 0)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	for _, r := range rows {
		if r.Title == title {
			return r.ID
		}
	}
	t.Fatalf("review %q not found after filing", title)
	return ""
}

func TestResearchQueuesARunAndLeavesTheQuestionOpen(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)
	id := fileReview(t, js, ws, "gap", "retry-policy is referred to but not written")

	var out map[string]any
	if code := send(t, srv, http.MethodPost,
		"/api/v1/workspaces/demo/reviews/"+id+"/research", "", map[string]any{}, &out); code != http.StatusAccepted {
		t.Fatalf("research = %d, want 202", code)
	}
	if out["runId"] == "" || out["runId"] == nil {
		t.Errorf("no run id returned: %v", out)
	}

	// Research is evidence, not a decision: the question is still in the
	// inbox, now reporting that a worker is reading for it.
	var reviews []map[string]any
	if code := send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/reviews", "", nil, &reviews); code != http.StatusOK {
		t.Fatal("GET reviews failed")
	}
	if len(reviews) != 1 {
		t.Fatalf("open reviews = %d, want the question still waiting", len(reviews))
	}
	if reviews[0]["researching"] != true {
		t.Errorf("researching = %v, want true while the run is queued", reviews[0]["researching"])
	}
	// The button must not be offered twice for the same read.
	if reviews[0]["researchable"] != true {
		t.Errorf("researchable = %v; a gap is answerable by reading", reviews[0]["researchable"])
	}

	// The bench runs one job at a time, and this one's slot is taken.
	if code := send(t, srv, http.MethodPost,
		"/api/v1/workspaces/demo/reviews/"+id+"/research", "", map[string]any{}, nil); code != http.StatusConflict {
		t.Errorf("second research = %d, want 409", code)
	}
}

func TestResearchRefusesWhatReadingCannotSettle(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	// A budget question is about the deployment, not the material: no amount
	// of reading decides whether someone wants to spend more money.
	id := fileReview(t, js, ws, "budget", "budget window exceeded")
	if code := send(t, srv, http.MethodPost,
		"/api/v1/workspaces/demo/reviews/"+id+"/research", "", map[string]any{}, nil); code != http.StatusBadRequest {
		t.Errorf("research on a budget item = %d, want 400", code)
	}

	var reviews []map[string]any
	send(t, srv, http.MethodGet, "/api/v1/workspaces/demo/reviews", "", nil, &reviews)
	if len(reviews) != 1 || reviews[0]["researchable"] != false {
		t.Errorf("a budget item reported researchable: %v", reviews)
	}

	// An item that is not there, and one that is no longer open, both answer
	// 404 rather than filing a run against nothing.
	if code := send(t, srv, http.MethodPost,
		"/api/v1/workspaces/demo/reviews/"+uuid.NewString()+"/research", "", map[string]any{}, nil); code != http.StatusNotFound {
		t.Errorf("research on an unknown id = %d, want 404", code)
	}

	gap := fileReview(t, js, ws, "gap", "something is missing")
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/reviews/"+gap+"/resolve", "",
		map[string]string{"action": "dismiss"}, nil); code != http.StatusOK {
		t.Fatal("resolve failed")
	}
	if code := send(t, srv, http.MethodPost,
		"/api/v1/workspaces/demo/reviews/"+gap+"/research", "", map[string]any{}, nil); code != http.StatusNotFound {
		t.Errorf("research on a resolved item = %d, want 404", code)
	}
}

func TestResolvingAQuestionDropsItsQueuedResearch(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)
	id := fileReview(t, js, ws, "uncertain", "which retry count is right")

	if code := send(t, srv, http.MethodPost,
		"/api/v1/workspaces/demo/reviews/"+id+"/research", "", map[string]any{}, nil); code != http.StatusAccepted {
		t.Fatal("research request failed")
	}
	// A human who has decided does not wait for a reader, and the money the
	// queued run would spend buys an answer to a settled question.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/reviews/"+id+"/resolve", "",
		map[string]string{"action": "dismiss"}, nil); code != http.StatusOK {
		t.Fatalf("resolve = %d, want 200", code)
	}

	runs, err := js.ListRuns(context.Background(), ws, 50, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	for _, r := range runs {
		if r.Trigger == "research" {
			t.Errorf("a research run survived its question: %+v", r)
		}
	}

	// With the slot released, an ordinary build can be queued again.
	if code := send(t, srv, http.MethodPost, "/api/v1/workspaces/demo/runs", "",
		map[string]any{}, nil); code != http.StatusAccepted {
		t.Errorf("build after cancelled research = %d, want 202", code)
	}
}

func TestResearchFindingsLandOnTheCard(t *testing.T) {
	_, js, ws := testServer(t)
	seed(t, js, ws)
	id := fileReview(t, js, ws, "contradiction", "two retry counts")
	ctx := context.Background()

	if err := js.RecordResearch(ctx, id, "Both say three; the older page is stale.", false); err != nil {
		t.Fatalf("RecordResearch: %v", err)
	}
	rows, err := js.ListReviews(ctx, ws, "open", 50, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListReviews = %d rows, %v", len(rows), err)
	}
	if !strings.Contains(rows[0].Research, "the older page is stale") {
		t.Errorf("findings = %q", rows[0].Research)
	}
	if rows[0].ResearchAt == "" {
		t.Error("findings landed with no date; the card cannot say when it was read")
	}

	// Research that reports the question settled closes it; research that
	// lands on an already-answered item changes nothing.
	other := fileReview(t, js, ws, "gap", "a settled question")
	if err := js.RecordResearch(ctx, other, "Answered outright.", true); err != nil {
		t.Fatalf("RecordResearch: %v", err)
	}
	if err := js.ResolveReview(ctx, ws, other, "dismiss", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("resolving a research-closed item = %v, want ErrNotFound", err)
	}
	if err := js.RecordResearch(ctx, other, "A later, ignored answer.", false); err != nil {
		t.Fatalf("RecordResearch: %v", err)
	}
	rows, err = js.ListReviews(ctx, ws, "", 50, 0)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	for _, r := range rows {
		if r.ID == other && strings.Contains(r.Research, "ignored") {
			t.Error("research overwrote findings on an item that was already closed")
		}
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
