package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const pinnedBench = "aaaaaaaa-0000-4000-8000-000000000001"

// filingKiln serves the two filing routes and records what reached them, raw.
type filingKiln struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
	status int // answer every filing with this, when set
}

func newFilingKiln(t *testing.T) *filingKiln {
	t.Helper()
	k := &filingKiln{}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		k.paths = append(k.paths, r.Method+" "+r.URL.EscapedPath())
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		k.bodies = append(k.bodies, body)
		status := k.status
		k.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case status != 0:
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"token lacks the required scope"}`))
		case strings.HasSuffix(r.URL.Path, "/request"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"filed"}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"r-1","status":"filed"}`))
		}
	}))
	t.Cleanup(k.Close)
	return k
}

func listTools(t *testing.T, s *Server) map[string]string {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "kiln", Version: "test"}, nil)
	s.Register(srv)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	out := map[string]string{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool.Description
	}
	return out
}

var filingTools = []string{"request_gap", "file_question", "propose_correction"}

func TestWriteToolsAbsentWithoutWrites(t *testing.T) {
	tools := listTools(t, NewServer(NewClient("http://unused", ""), "", "test"))
	if len(tools) != 7 {
		t.Errorf("a read-only server lists %d tools, want the seven reads", len(tools))
	}
	for _, name := range filingTools {
		if _, ok := tools[name]; ok {
			t.Errorf("%s is listed without writes", name)
		}
	}

	tools = listTools(t, NewServer(NewClient("http://unused", ""), "", "test").WithWrites(true, pinnedBench))
	if len(tools) != 10 {
		t.Errorf("a filing server lists %d tools, want ten", len(tools))
	}
	for _, name := range filingTools {
		desc, ok := tools[name]
		if !ok {
			t.Fatalf("%s missing with writes on", name)
		}
		// What the agent most needs to believe, and where it files.
		if !strings.Contains(desc, "Changes nothing in the wiki") {
			t.Errorf("%s does not say it changes no page: %q", name, desc)
		}
		if !strings.Contains(desc, pinnedBench) {
			t.Errorf("%s does not name the bench it files into: %q", name, desc)
		}
	}
}

func TestWriteToolsPathEscapeTheSlug(t *testing.T) {
	k := newFilingKiln(t)
	s := NewServer(NewClient(k.URL, ""), "", "test").WithWrites(true)

	if out, isErr := call(t, s, "request_gap", map[string]any{
		"bench": "acme/kiln", "slug": "a/../../reviews?x=1#y",
	}); isErr {
		t.Fatalf("request_gap: %s", out)
	}
	want := "POST /api/v1/workspaces/acme~kiln/gaps/a%2F..%2F..%2Freviews%3Fx=1%23y/request"
	if len(k.paths) != 1 || k.paths[0] != want {
		t.Errorf("request reached %v, want exactly %q", k.paths, want)
	}
}

func TestForbiddenWriteIsAToolError(t *testing.T) {
	k := newFilingKiln(t)
	k.status = http.StatusForbidden
	s := NewServer(NewClient(k.URL, ""), "", "test").WithWrites(true)

	out, isErr := call(t, s, "propose_correction", map[string]any{
		"bench": "kiln", "page": "jobs", "text": "The retry limit is 5.",
	})
	if !isErr {
		t.Fatalf("a 403 came back as success: %s", out)
	}
	if !strings.Contains(out, `"Can file questions and corrections"`) || !strings.Contains(out, "Nothing was filed") {
		t.Errorf("refusal does not say what key to ask for: %s", out)
	}
}

func TestFilingToolsSendTheRightShape(t *testing.T) {
	k := newFilingKiln(t)
	s := NewServer(NewClient(k.URL, ""), "", "test").WithWrites(true, pinnedBench)

	// A pinned key files into its bench by default.
	if out, isErr := call(t, s, "file_question", map[string]any{
		"page": "jobs", "kind": "uncertain", "title": "Is it 3?", "detail": "the page says 3",
	}); isErr || !strings.Contains(out, "nothing in the wiki changed") {
		t.Fatalf("file_question = %q (error %v)", out, isErr)
	}
	if out, isErr := call(t, s, "propose_correction", map[string]any{
		"page": "jobs", "text": "It is 5.", "rationale": "config.go says 5",
	}); isErr {
		t.Fatalf("propose_correction: %s", out)
	}
	wantPath := "POST /api/v1/workspaces/" + pinnedBench + "/reviews"
	if !slices.Equal(k.paths, []string{wantPath, wantPath}) {
		t.Errorf("paths = %v", k.paths)
	}
	if k.bodies[0]["kind"] != "uncertain" || k.bodies[0]["title"] != "Is it 3?" {
		t.Errorf("question body = %v", k.bodies[0])
	}
	if k.bodies[1]["kind"] != "correction" || k.bodies[1]["text"] != "It is 5." || k.bodies[1]["detail"] != "config.go says 5" {
		t.Errorf("correction body = %v", k.bodies[1])
	}

	// Refused before any request: another bench, and a kind the tool does not take.
	before := len(k.paths)
	if out, isErr := call(t, s, "file_question", map[string]any{
		"bench": "other", "page": "jobs", "kind": "uncertain", "title": "x",
	}); !isErr || !strings.Contains(out, pinnedBench) {
		t.Errorf("another bench = %q (error %v), want a refusal naming the pinned bench", out, isErr)
	}
	for _, kind := range []string{"deletion", "correction", "gap", ""} {
		if _, isErr := call(t, s, "file_question", map[string]any{
			"page": "jobs", "kind": kind, "title": "x",
		}); !isErr {
			t.Errorf("kind %q was accepted", kind)
		}
	}
	if len(k.paths) != before {
		t.Errorf("a refused call still reached kiln: %v", k.paths[before:])
	}
}

func TestStatusErrorReadsTheAPIMessage(t *testing.T) {
	k := newFilingKiln(t)
	k.status = http.StatusTooManyRequests
	err := NewClient(k.URL, "").RequestGap(context.Background(), "kiln", "x")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTooManyRequests || se.Msg != "token lacks the required scope" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(se.Error(), "429") {
		t.Errorf("Error() = %q", se.Error())
	}
}
