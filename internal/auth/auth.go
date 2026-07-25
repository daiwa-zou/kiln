package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// Identity is what a valid token authenticates: a user, their admin bit, and
// the scopes the token grants. Org membership is resolved per query rather
// than snapshotted here, so a revoked membership takes effect immediately.
type Identity struct {
	UserID string
	Admin  bool
	Scopes []string
}

// HasScope reports whether the token grants a scope.
func (id Identity) HasScope(scope string) bool {
	return slices.Contains(id.Scopes, scope)
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

// Middleware authenticates every request with a bearer token.
type Middleware struct {
	Source Source
	Log    *slog.Logger
}

// Wrap enforces authentication and the read scope on the wrapped handler.
// Write scopes are checked here too, so future mutating routes are covered the
// day they are added rather than the day someone remembers.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain, ok := bearerToken(r)
		if !ok || !looksLikeToken(plain) {
			deny(w, http.StatusUnauthorized, "missing or malformed bearer token")
			return
		}

		id, err := m.Source.IdentityForToken(r.Context(), HashToken(plain))
		switch {
		case errors.Is(err, ErrUnauthenticated):
			deny(w, http.StatusUnauthorized, "invalid or expired token")
			return
		case err != nil:
			if m.Log != nil {
				m.Log.Error("auth lookup failed", "err", err)
			}
			deny(w, http.StatusInternalServerError, "internal error")
			return
		}

		if !id.HasScope(requiredScope(r.Method)) {
			deny(w, http.StatusForbidden, "token lacks the required scope")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

func requiredScope(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return "read"
	default:
		return "write"
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
