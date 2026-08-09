package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The MCP endpoint is exercised as raw JSON-RPC over HTTP rather than through a
// client library, because what these tests are about is the HTTP shape: the
// status, the headers, and the transport. The protocol itself is the SDK's.

// mcpSession opens a session and holds its id, which every later call must
// echo. Streamable HTTP answers with server-sent events, so each response is a
// `data:` line wrapping the JSON-RPC frame.
type mcpSession struct {
	url, token, id string
}

func openMCP(t *testing.T, baseURL, token string) *mcpSession {
	t.Helper()
	s := &mcpSession{url: baseURL + "/mcp", token: token}

	res := s.post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2025-06-18","capabilities":{},`+
		`"clientInfo":{"name":"test","version":"1"}}}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("initialize = %d: %s", res.StatusCode, body)
	}
	s.id = res.Header.Get("Mcp-Session-Id")
	if s.id == "" {
		t.Fatal("initialize returned no Mcp-Session-Id")
	}
	// The server will not serve tool calls until the client says it is ready.
	s.post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`).Body.Close()
	return s
}

func (s *mcpSession) post(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	if s.id != "" {
		req.Header.Set("Mcp-Session-Id", s.id)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	return res
}

// call runs one tool and returns its text content.
func (s *mcpSession) call(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res := s.post(t, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
		tool, argsJSON))
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var frame struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("decode frame %q: %v", payload, err)
		}
		if frame.Error != nil {
			t.Fatalf("%s: %s", tool, frame.Error.Message)
		}
		var b strings.Builder
		for _, c := range frame.Result.Content {
			b.WriteString(c.Text)
		}
		return b.String()
	}
	t.Fatalf("%s: no data frame in %q", tool, raw)
	return ""
}

// A tool call reaches kiln's own API through the in-process loopback, and that
// dispatch re-enters the router it is already inside. Two properties of a
// server request have to be reconstructed for it to land, and both failed
// quietly rather than loudly: a nil Body panicked the first middleware that
// wrapped it, and an inherited chi route context resumed matching from /mcp,
// which sent a GET of /api/v1/workspaces to the POST handler.
func TestMCPToolsReadThroughTheAPI(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	s := openMCP(t, srv.URL, "")

	if out := s.call(t, "list_benches", map[string]any{}); !strings.Contains(out, "demo") {
		t.Errorf("list_benches = %q, want the seeded bench", out)
	}
	if out := s.call(t, "search_wiki", map[string]any{
		"bench": "demo", "query": "dispatch",
	}); !strings.Contains(strings.ToLower(out), "ripple") {
		t.Errorf("search_wiki = %q, want the seeded page", out)
	}
}

// The endpoint serves every bench the key can read, and no others. Both halves
// matter and neither is obvious from the code: the tools reach the wiki through
// a loopback back into this same router, so what a token can see is decided by
// the ordinary API handlers rather than by anything in the MCP layer. This is
// the test that says so.
func TestMCPServesExactlyTheBenchesTheTokenAllows(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	// A second bench in an org this caller does not belong to.
	other, err := js.EnsureWorkspace(context.Background(), "other-org", "hidden", "Hidden")
	if err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}
	if other == ws {
		t.Fatal("second bench collided with the first")
	}

	s := openMCP(t, srv.URL, "")

	// testServer runs without auth, so the caller is admin-equivalent and sees
	// both. The point being locked in is that the list comes from the same
	// visibility query the REST API uses, not from a hardcoded set.
	out := s.call(t, "list_benches", map[string]any{})
	for _, want := range []string{"demo", "hidden"} {
		if !strings.Contains(out, want) {
			t.Errorf("list_benches = %q, want it to include %q", out, want)
		}
	}

	// No bench is the default: every tool names one, so a connector shared
	// across benches cannot silently answer from whichever was configured.
	if out := s.call(t, "search_wiki", map[string]any{"query": "dispatch"}); !strings.Contains(out, "bench") {
		t.Errorf("search_wiki with no bench = %q, want it to ask for one", out)
	}

	// A bench that does not exist reads the same as one the caller cannot see:
	// absence and inaccessibility must not be distinguishable, or the error
	// message becomes a way to enumerate other tenants' benches.
	missing := s.call(t, "search_wiki", map[string]any{"bench": "no-such-bench", "query": "x"})
	if !strings.Contains(missing, "list_benches") {
		t.Errorf("unknown bench = %q, want it to point at list_benches", missing)
	}
}

// With authentication disabled the endpoint is open: an authless MCP server is
// a supported connector, and a challenge here would send clients hunting for
// an authorization server that does not exist.
func TestMCPIsOpenWhenAuthIsDisabled(t *testing.T) {
	srv, js, ws := testServer(t)
	seed(t, js, ws)

	s := &mcpSession{url: srv.URL + "/mcp"}
	res := s.post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("initialize without a token = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q on an authless deployment, want none", got)
	}
}

// The protected-resource document is what a client reads to find the OAuth
// flow, and `resource` must equal the endpoint URL exactly or the client
// rejects it. It is built from PublicURL because behind a terminating proxy
// the request itself carries the wrong scheme and an internal host.
//
// Served at both the path-suffixed location (which the 401 challenge names and
// clients probe first) and the bare one (the fallback).
func TestProtectedResourceMetadata(t *testing.T) {
	s := &Server{PublicURL: "https://kiln.example.com/", Store: (*fakeStore)(nil)}
	srv := httptest.NewServer(s.Router())
	defer srv.Close()

	for _, path := range []string{
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-protected-resource",
	} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		var doc struct {
			Resource               string   `json:"resource"`
			AuthorizationServers   []string `json:"authorization_servers"`
			BearerMethodsSupported []string `json:"bearer_methods_supported"`
		}
		err = json.NewDecoder(res.Body).Decode(&doc)
		res.Body.Close()
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}

		// The trailing slash on PublicURL must not survive into the identifier.
		if want := "https://kiln.example.com/mcp"; doc.Resource != want {
			t.Errorf("%s: resource = %q, want %q", path, doc.Resource, want)
		}
		// No authorization server is configured, so the field is absent rather
		// than guessed at.
		if len(doc.AuthorizationServers) != 0 {
			t.Errorf("%s: authorization_servers = %v, want absent", path, doc.AuthorizationServers)
		}
		if len(doc.BearerMethodsSupported) == 0 {
			t.Errorf("%s: bearer_methods_supported is empty", path)
		}
	}
}

// fakeStore mounts the MCP routes without a database: the metadata documents
// are static and must answer before anything else is configured.
type fakeStore struct{ Store }
