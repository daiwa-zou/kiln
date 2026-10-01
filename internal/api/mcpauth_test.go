package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

// The MCP endpoint's authenticated path. Every other MCP test runs on an
// authless deployment, which is a supported shape but leaves the half that
// matters for a hosted bench -- an agent key working the moment it is minted
// and failing the moment it is revoked -- entirely unexercised.

func TestMCPAcceptsAKilnTokenLikeAnyOtherRoute(t *testing.T) {
	srv, pool, wsID, src := authedServer(t)
	seed(t, store.NewWikiStore(pool), wsID)

	agent := addMember(t, pool, wsID, "aiden", "member")
	src.id = auth.Identity{UserID: agent, Scopes: []string{"read"}}

	// The token resolves through the same table every other route uses, so a
	// key minted for the API is an agent key without further ceremony.
	s := openMCP(t, srv.URL, "kiln_valid")
	if out := s.call(t, "list_benches", map[string]any{}); !strings.Contains(out, "demo") {
		t.Errorf("list_benches = %q, want the bench this key can read", out)
	}
	if out := s.call(t, "search_wiki", map[string]any{
		"bench": "demo", "query": "dispatch",
	}); !strings.Contains(strings.ToLower(out), "ripple") {
		t.Errorf("search_wiki = %q, want the seeded page", out)
	}
}

func TestMCPChallengesACallerWithNoUsableToken(t *testing.T) {
	srv, _, _, _ := authedServer(t)

	for _, tc := range []struct{ name, token string }{
		{"no token at all", ""},
		{"a token nothing issued", "kiln_bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &mcpSession{url: srv.URL + "/mcp", token: tc.token}
			res := s.post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
				`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
			defer res.Body.Close()

			// 401, not 500: an unrecognized token is an invalid token, and the
			// difference decides whether a client retries with credentials or
			// reports the server as broken.
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("initialize = %d, want 401", res.StatusCode)
			}
			// The challenge names the metadata document, which is how a client
			// finds out where to authenticate.
			if challenge := res.Header.Get("WWW-Authenticate"); !strings.Contains(challenge, resourceMetadataPath) {
				t.Errorf("WWW-Authenticate = %q, want it to point at %s", challenge, resourceMetadataPath)
			}
		})
	}
}

func TestMCPStopsServingARevokedToken(t *testing.T) {
	// The reason the endpoint resolves against the tokens table rather than
	// holding an MCP credential of its own: revocation takes effect on the
	// next request, with no second place to remember.
	srv, pool, wsID, src := authedServer(t)
	agent := addMember(t, pool, wsID, "aiden", "member")
	src.id = auth.Identity{UserID: agent, Scopes: []string{"read"}}

	s := openMCP(t, srv.URL, "kiln_valid")
	if out := s.call(t, "list_benches", map[string]any{}); out == "" {
		t.Fatal("the token did not work before it was revoked")
	}

	src.revoked = true
	res := s.post(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("call with a revoked token = %d, want 401", res.StatusCode)
	}
}
