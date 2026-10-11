package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Identity is what a valid token authenticates: a user, their admin bit, and
// the scopes the token grants. Org membership is resolved per query rather
// than snapshotted here, so a revoked membership takes effect immediately.
type Identity struct {
	UserID string
	Admin  bool
	Scopes []string
	// TokenID and TokenName identify the API token that authenticated the
	// request, empty for a browser session. The id keys the per-key filing
	// bucket and caps; the name is what the review queue shows as the filer.
	TokenID   string
	TokenName string
}

// Scopes a token can carry. read and write split on the HTTP method (see
// requiredScope); admin gates the operator surfaces.
//
// propose is narrower than write and is never granted bare: it is stored as
// "propose:<workspace id>" and lets a token file questions and proposed
// corrections into that one bench -- the two filing routes the API opts in
// with Middleware.WithPropose -- and nothing else. That is what makes
// "nothing an agent sends changes a page" true of the token rather than only
// of the MCP tools: a write token can pin corrections, resolve deletion
// reviews and delete pages over plain REST.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeAdmin = "admin"
	// ScopeProposePrefix is followed by a workspace id; see ProposeScope.
	ScopeProposePrefix = "propose:"
)

// ProposeScope is the scope that lets a token file into one workspace.
func ProposeScope(workspaceID string) string { return ScopeProposePrefix + workspaceID }

// ProposeWorkspace parses a propose scope, returning the workspace id it is
// pinned to. Anything that is not exactly "propose:" plus a canonical UUID is
// rejected, so a malformed or bare "propose" grants nothing.
func ProposeWorkspace(scope string) (string, bool) {
	rest, ok := strings.CutPrefix(scope, ScopeProposePrefix)
	if !ok {
		return "", false
	}
	u, err := uuid.Parse(rest)
	if err != nil || u.String() != rest {
		return "", false
	}
	return rest, true
}

// HasScope reports whether the token grants a scope.
func (id Identity) HasScope(scope string) bool {
	return slices.Contains(id.Scopes, scope)
}

// ProposeWorkspaces lists the workspaces the token may file into through a
// propose scope. A write token files anywhere it can write and needs none.
func (id Identity) ProposeWorkspaces() []string {
	var out []string
	for _, s := range id.Scopes {
		if ws, ok := ProposeWorkspace(s); ok {
			out = append(out, ws)
		}
	}
	return out
}

// CanProposeIn reports whether a token without write may file into a
// workspace: it must read, and hold the propose scope pinned to exactly that
// workspace.
func (id Identity) CanProposeIn(workspaceID string) bool {
	return workspaceID != "" && id.HasScope(ScopeRead) &&
		slices.Contains(id.ProposeWorkspaces(), workspaceID)
}

// ErrUnauthenticated is returned by a Source when no live token matches.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// Source resolves a token hash to an identity. Declared as an interface so the
// middleware is testable without Postgres.
type Source interface {
	IdentityForToken(ctx context.Context, tokenHash string) (Identity, error)
}

type ctxKey struct{}

// FromContext returns the identity the middleware attached, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// Middleware authenticates every request with a bearer token or, when
// Sessions is set, a browser session cookie.
type Middleware struct {
	Source Source
	// Sessions enables cookie authentication. Nil keeps the middleware
	// token-only, which is the correct posture for deployments that never
	// turned sign-in on.
	Sessions SessionSource
	Log      *slog.Logger

	// propose decides whether a mutating request from a token that lacks
	// write, but holds a propose scope, is one of the filing routes for a
	// workspace that scope names. Nil (the zero value) admits no such
	// request, so the narrow scope is useless until a router opts in.
	propose func(*http.Request, Identity) bool
}

// WithPropose returns a copy of the middleware that admits propose-scoped
// tokens wherever f answers true. The router supplies f because only it knows
// which handler a request will reach and which workspace it names; the scope
// check stays here, in the one place every route passes through.
func (m Middleware) WithPropose(f func(*http.Request, Identity) bool) *Middleware {
	m.propose = f
	return &m
}

// Wrap enforces authentication and the read scope on the wrapped handler.
// Write scopes are checked here too, so future mutating routes are covered the
// day they are added rather than the day someone remembers.
//
// An Authorization header always wins over a cookie: API clients keep exact
// token semantics even from a browser that also carries a session. Cookie
// authentication additionally enforces the CSRF double-submit on mutations —
// cookies are ambient authority, sent by the browser on an attacker's behalf,
// which is precisely what bearer tokens are immune to and cookies are not.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if plain, ok := bearerToken(r); ok {
			if !looksLikeToken(plain) {
				deny(w, http.StatusUnauthorized, "missing or malformed bearer token")
				return
			}
			id, err := m.Source.IdentityForToken(r.Context(), HashToken(plain))
			if !m.handled(w, err, id, r) {
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
			return
		}

		if m.Sessions != nil {
			if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
				id, csrf, err := m.Sessions.IdentityForSession(r.Context(), HashToken(c.Value))
				if !m.handled(w, err, id, r) {
					return
				}
				// A session without a stored CSRF token never mutates: the
				// empty string must not compare equal to an absent header.
				if mutating(r.Method) && (csrf == "" || r.Header.Get(CSRFHeader) != csrf) {
					deny(w, http.StatusForbidden, "missing or mismatched CSRF token")
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
				return
			}
		}

		deny(w, http.StatusUnauthorized, "missing or malformed bearer token")
	})
}

// handled writes the failure response for a lookup, returning true when the
// request may proceed.
func (m *Middleware) handled(w http.ResponseWriter, err error, id Identity, r *http.Request) bool {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		deny(w, http.StatusUnauthorized, "invalid or expired credentials")
		return false
	case err != nil:
		if m.Log != nil {
			m.Log.Error("auth lookup failed", "err", err)
		}
		deny(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if !id.HasScope(requiredScope(r.Method)) && !m.proposeAdmits(r, id) {
		deny(w, http.StatusForbidden, "token lacks the required scope")
		return false
	}
	return true
}

// proposeAdmits is the one exception to "mutations need write": a token that
// reads and holds a propose scope, on a route the router opted in. Every
// condition is checked here rather than trusted to the router's func, so a
// careless matcher can at worst admit a propose token, never a read-only one.
func (m *Middleware) proposeAdmits(r *http.Request, id Identity) bool {
	return m.propose != nil && mutating(r.Method) &&
		id.HasScope(ScopeRead) && len(id.ProposeWorkspaces()) > 0 &&
		m.propose(r, id)
}

func mutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

func requiredScope(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ScopeRead
	default:
		return ScopeWrite
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, rest, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(rest)
	return tok, tok != ""
}

func deny(w http.ResponseWriter, status int, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kiln"`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
