package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/daiwa-zou/kiln/internal/auth"
	kilnmcp "github.com/daiwa-zou/kiln/internal/mcp"
	"github.com/daiwa-zou/kiln/internal/observability"
)

// kiln as a remote MCP server.
//
// The stdio server (`kiln mcp`) still exists and is still the right thing for
// a local agent: it spawns, reads one instance, and dies with the host. What it
// cannot be is a *connector* -- a URL someone pastes into Claude -- because
// there is no subprocess to spawn on the other end. That is what this is: the
// same tools, the same client, over Streamable HTTP, mounted on the API server
// that already holds the wiki.
//
// The transport is Streamable HTTP rather than the older HTTP+SSE pairing.
// Claude supports both and is deprecating SSE, and the SDK's handler speaks
// the current one.

const (
	// mcpPath is where the endpoint lives. It is also the resource identifier
	// in the protected-resource metadata, and RFC 8707 requires those to be
	// the same string, so it is spelled once.
	mcpPath = "/mcp"
	// resourceMetadataPath is the RFC 9728 document for mcpPath. Clients probe
	// the path-suffixed form first, so that is the one the challenge names.
	resourceMetadataPath = "/.well-known/oauth-protected-resource" + mcpPath
	// mcpTimeout bounds one MCP request. Longer than the 30s the rest of the
	// API lives under because a Streamable HTTP response stays open while
	// results stream back; shorter than the five minutes Claude waits, so the
	// server gives up first and says so.
	mcpTimeout = 4 * time.Minute
	// mcpScope is the only scope the tools need: every one of them is a read.
	mcpScope = "read"
)

// mountMCP registers the MCP endpoint and its discovery documents.
//
// Requires Store, which every deployment has; the tools are all reads.
func (s *Server) mountMCP(r chi.Router) {
	if s.Store == nil {
		return
	}

	handler := mcp.NewStreamableHTTPHandler(s.mcpServerFor, nil)

	// The well-known documents sit outside the auth wrap by nature: their whole
	// job is to tell an unauthenticated client how to authenticate.
	prm := mcpauth.ProtectedResourceMetadataHandler(s.resourceMetadata())
	r.Method(http.MethodGet, resourceMetadataPath, prm)
	r.Method(http.MethodOptions, resourceMetadataPath, prm)
	// The bare path is the fallback probe, and the spec's own default location.
	r.Method(http.MethodGet, "/.well-known/oauth-protected-resource", prm)
	r.Method(http.MethodOptions, "/.well-known/oauth-protected-resource", prm)

	r.Handle(mcpPath, s.mcpAuth(handler))
}

// mcpServerFor builds the MCP server for one HTTP request.
//
// Per request, not once, because the tools read kiln with the caller's own
// token: two agents pointed at the same endpoint must see exactly what their
// own tokens see, and that is only true if the credential travels with the
// request rather than being captured at mount time.
//
// No default bench. The stdio server takes --workspace because it is started
// by someone who knows which bench they mean; a connector is a URL shared
// across benches, so every tool takes a bench argument and list_benches
// enumerates what the token can reach.
func (s *Server) mcpServerFor(r *http.Request) *mcp.Server {
	client := kilnmcp.NewClient(s.publicBase(r), bearerToken(r))
	client.HTTP = &http.Client{
		Transport: loopback{handler: func() http.Handler { return s.handler() }},
		Timeout:   30 * time.Second,
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "kiln",
		Title:   "kiln wiki",
		Version: observability.Version,
	}, nil)
	kilnmcp.NewServer(client, "", observability.Version).Register(srv)
	return srv
}

// mcpAuth guards the endpoint.
//
// Bearer only, never the session cookie: a cookie would let any page the user
// happens to be visiting POST tool calls to this endpoint with their ambient
// authority. An agent carries a token; a browser tab does not get to.
//
// The 401 is the load-bearing part. A client that cannot authenticate learns
// where to go from the WWW-Authenticate challenge, and both the MCP spec and
// Claude require the pointer to arrive on a 401 -- not as a tool error, and not
// on a 200. RequireBearerToken writes exactly that shape.
func (s *Server) mcpAuth(next http.Handler) http.Handler {
	// Authentication disabled: the deployment has decided every caller is
	// trusted, and an authless MCP server is a thing connectors support. A
	// challenge here would send clients hunting for an authorization server
	// that does not exist.
	if s.Auth == nil {
		return next
	}
	opts := &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: strings.TrimRight(s.PublicURL, "/") + resourceMetadataPath,
		// Every tool is a read, and read is the scope kiln gives a token by
		// default. Requiring it here turns a token that cannot read into one
		// clean 403 at the door, rather than seven tools that each fail
		// somewhere inside with a message about the wiki.
		Scopes: []string{mcpScope},
		// kiln tokens are opaque and long-lived; the tokens table holds the
		// expiry and IdentityForToken already refuses a lapsed one. There is no
		// `exp` claim to read out-of-band, so the middleware must not insist on
		// one -- it would reject every valid kiln token.
		AllowMissingExpiration: true,
	}
	return mcpauth.RequireBearerToken(s.verifyMCPToken, opts)(next)
}

// verifyMCPToken resolves a bearer token against the same table every other
// API route uses, so an agent key works here the moment it is minted and stops
// working the moment it is revoked.
func (s *Server) verifyMCPToken(ctx context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
	id, err := s.Auth.Source.IdentityForToken(ctx, auth.HashToken(token))
	if err != nil {
		// Unwrapping to ErrInvalidToken is what turns this into a 401 with the
		// challenge attached rather than a 500.
		return nil, mcpauth.ErrInvalidToken
	}
	return &mcpauth.TokenInfo{Scopes: id.Scopes, UserID: id.UserID}, nil
}

// resourceMetadata is the RFC 9728 document.
//
// `resource` must match the URL the user typed into their client exactly,
// including the path, or the client rejects the document -- hence PublicURL
// plus the same mcpPath constant the route is mounted on.
//
// authorization_servers is populated only when an operator has configured one.
// The field is how a client finds the OAuth flow, and naming an issuer that
// cannot mint tokens for this resource would send every client through a
// handshake that ends in a refusal. Absent, the document still says what it is
// and what it accepts, which is what a reader needs in the deployments kiln
// actually supports today: a token in the header, or no auth at all.
func (s *Server) resourceMetadata() *oauthex.ProtectedResourceMetadata {
	m := &oauthex.ProtectedResourceMetadata{
		Resource:               strings.TrimRight(s.PublicURL, "/") + mcpPath,
		ResourceName:           "kiln wiki",
		ScopesSupported:        []string{mcpScope},
		BearerMethodsSupported: []string{"header"},
	}
	if s.MCPAuthorizationServer != "" {
		m.AuthorizationServers = []string{s.MCPAuthorizationServer}
	}
	return m
}

// publicBase is the absolute origin of this instance, for the metadata
// documents and for the loopback client's request URLs.
//
// PublicURL wins because it is the only thing that survives a reverse proxy:
// behind TLS termination the request arrives as plain HTTP on some internal
// host, and a URL derived from it would be wrong in exactly the deployments
// that matter. The request is the fallback for a instance nobody configured,
// where it is right by construction.
func (s *Server) publicBase(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimRight(s.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// bearerToken pulls the credential the caller presented, so it can be replayed
// on the reads the tools make. Empty on an authless deployment.
func bearerToken(r *http.Request) string {
	f := strings.Fields(r.Header.Get("Authorization"))
	if len(f) == 2 && strings.EqualFold(f[0], "bearer") {
		return f[1]
	}
	return ""
}

// loopback dispatches the MCP client's reads back into this process.
//
// The tools deliberately read kiln through its HTTP API rather than the store:
// that is what makes an agent see exactly what its token sees, because every
// visibility rule lives in the handlers. When the MCP endpoint is mounted on
// that same API there is no reason to leave the process to get it -- no socket,
// no port to discover, no second pass through TLS, and no way for the internal
// hop to be reachable by anyone else.
//
// The handler is fetched through a func because the router does not exist yet
// when this is constructed: Router() builds the routes that contain this.
type loopback struct {
	handler func() http.Handler
}

func (l loopback) RoundTrip(req *http.Request) (*http.Response, error) {
	h := l.handler()
	if h == nil {
		return nil, http.ErrServerClosed
	}

	// A request built for a client is not shaped like one a server receives,
	// and handlers are written against the server's guarantees. Three of them
	// matter here, and the first is not optional: http.Server always sets a
	// non-nil Body, so a handler that wraps r.Body -- MaxBytesReader on every
	// write route -- dereferences nil and panics on a GET that arrived with
	// none.
	// A fresh routing context, because this dispatch starts from inside one.
	// chi keeps the path it has left to match in the request context, and
	// re-entering the same Mux with the outer request's context makes it
	// resume from /mcp rather than start at /. Routes then match by accident:
	// a GET of /api/v1/workspaces arrived at the POST handler.
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, chi.NewRouteContext())

	in := req.Clone(ctx)
	if in.Body == nil {
		in.Body = http.NoBody
	}
	in.RequestURI = in.URL.RequestURI()
	if in.RemoteAddr == "" {
		// The rate limiters bucket per caller. Everything on this path is
		// already inside the process and past the endpoint's own auth, so one
		// bucket for all of it is right -- but it must be a real address, not
		// the empty string every other caller would also hash to.
		in.RemoteAddr = "127.0.0.1:0"
	}

	rec := &recorder{header: make(http.Header), code: http.StatusOK}
	h.ServeHTTP(rec, in)
	return &http.Response{
		StatusCode: rec.code,
		Status:     http.StatusText(rec.code),
		Header:     rec.header,
		Body:       io.NopCloser(bytes.NewReader(rec.body.Bytes())),
		Request:    req,
	}, nil
}

// recorder is the smallest ResponseWriter that can be turned back into a
// Response. net/http/httptest has one, but it belongs to the test toolchain
// and this path runs in production.
type recorder struct {
	header  http.Header
	body    bytes.Buffer
	code    int
	written bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.written {
		r.code = code
		r.written = true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.written = true
	return r.body.Write(b)
}
