package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/auth"
)

// API keys for agents: the MCP server reads this instance over HTTP, so an
// agent needs a bearer token, and the only way to get one used to be a shell
// on the host running `kiln admin token create`.
//
// Everything minted here carries the read scope and nothing else. That is what
// MCP needs -- every tool it exposes is a read -- and it means the endpoint
// cannot be turned into a way to acquire more authority than the session
// already has: a writer or an admin asking for an agent key gets a key that
// can only read, which is a reduction rather than an escalation. The token is
// attached to the caller's own user, so it sees exactly the benches they do.
//
// Minting is a write, so the middleware's scope rule already requires the
// write scope to reach it. That is the property worth keeping rather than
// working around: a key issued here cannot issue another, so a leaked one
// cannot mint itself a fresh long-lived replacement and outlive the
// revocation of the original. A person signed in at the browser has the write
// scope; a read-only automation token does not, and is refused.
const (
	agentTokenScope = "read"
	// Long enough to be useful in a config file, short enough that a key
	// pasted somewhere and forgotten stops working.
	agentTokenTTL = 365 * 24 * time.Hour
)

// callerUser resolves the caller to the user a key would belong to.
//
// Auth disabled is not an error and not a user: the deployment has decided
// every caller is trusted, so there is no identity to attach a key to and no
// key for an agent to carry. Saying that plainly is more useful than minting
// an orphan token that guards nothing.
func (s *Server) callerUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, found := auth.FromContext(r.Context())
	if !found || id.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this instance runs with authentication disabled, so agents connect without a key",
		})
		return "", false
	}
	return id.UserID, true
}

func (s *Server) handleTokensList(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.callerUser(w, r)
	if !ok {
		return
	}
	rows, err := auth.ListForUser(r.Context(), s.SessionPool, userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, tokenJSON(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.callerUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	// The name is a label for the reader's own benefit, so it is the one thing
	// the caller does choose -- bounded, since it is displayed back.
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "agent"
	}
	if len(name) > 60 {
		name = name[:60]
	}

	// Scope and lifetime are the server's decision, not the caller's.
	plain, info, err := auth.MintForUser(
		r.Context(), s.SessionPool, userID, name, []string{agentTokenScope}, agentTokenTTL)
	if err != nil {
		s.fail(w, err)
		return
	}

	// The only response that will ever carry the plaintext. It is not logged,
	// and no later read can recover it: the column holds a hash.
	payload := tokenJSON(info)
	payload["token"] = plain
	writeJSON(w, http.StatusCreated, payload)
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.callerUser(w, r)
	if !ok {
		return
	}
	err := auth.RevokeID(r.Context(), s.SessionPool, userID, chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, auth.ErrNoSuchToken):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such key"})
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	}
}

func tokenJSON(t auth.TokenInfo) map[string]any {
	out := map[string]any{
		"id": t.ID, "name": t.Name, "scopes": t.Scopes,
		"created": t.CreatedAt.UTC().Format(time.RFC3339),
	}
	if t.LastUsed != nil {
		out["lastUsed"] = t.LastUsed.UTC().Format(time.RFC3339)
	}
	if t.ExpiresAt != nil {
		out["expires"] = t.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return out
}
