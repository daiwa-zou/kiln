package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	"github.com/daiwa-zou/kiln/internal/store"
)

// Configuring where a bench publishes is an admin action, with the connector
// and credential routes rather than with page edits.
//
// It names an external repository and attaches a credential that can write to
// it, which is the same shape of decision as configuring what a bench reads --
// and a misconfigured target writes this bench's contents into somebody else's
// repository. An editor who can queue builds and delete pages should not be
// able to choose where the wiki is mirrored.

const maxPublishBodyBytes = 8 << 10

// PublishStore is the publish-target surface; *store.WikiStore implements it.
type PublishStore interface {
	PublishTargetFor(ctx context.Context, workspaceID string) (store.PublishTarget, error)
	SetPublishTarget(ctx context.Context, t store.PublishTarget) (string, error)
	PatchPublishTarget(ctx context.Context, workspaceID string, p store.PublishTargetPatch) error
	DeletePublishTarget(ctx context.Context, workspaceID string) error
}

// publishJSON shapes a target for responses. The credential is reported by id
// and presence only; the secret itself never leaves the keyring.
func publishJSON(t store.PublishTarget) map[string]any {
	out := map[string]any{
		"kind": t.Kind, "repo": t.RepoURL, "branch": t.Branch,
		"pathPrefix":    t.PathPrefix,
		"enabled":       t.Enabled,
		"credentialId":  t.CredentialID,
		"authenticated": t.CredentialID != "",
		"lastCommit":    t.LastCommit,
		"lastError":     t.LastError,
	}
	if t.LastPublishedAt != nil {
		out["lastPublished"] = t.LastPublishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Server) handlePublishGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if s.Publish == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	t, err := s.Publish.PublishTargetFor(r.Context(), ws.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Not an error: most benches do not publish, and a 404 here would make
		// the UI treat "not configured" as a failure to report.
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	out := publishJSON(t)
	out["configured"] = true
	writeJSON(w, http.StatusOK, out)
}

// handlePublishPut configures where the bench publishes.
func (s *Server) handlePublishPut(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	if s.Publish == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "publishing is not available on this deployment"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPublishBodyBytes)

	var body struct {
		Repo         string `json:"repo"`
		Branch       string `json:"branch"`
		PathPrefix   string `json:"pathPrefix"`
		CredentialID string `json:"credentialId"`
		Enabled      *bool  `json:"enabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	// Validated here, at configuration time, rather than only at push time.
	// The same policy the git connector applies to a clone: https, no embedded
	// credentials, nothing resolving into private address space. Someone
	// typing a bad URL should hear about it while they are looking at the
	// form, not silently on a build hours later.
	if _, err := gitconn.ValidateRemoteURL(body.Repo); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !s.credentialUsable(w, r, ws, body.CredentialID) {
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	branch := strings.TrimSpace(body.Branch)
	if branch == "" {
		branch = "main"
	}

	id, err := s.Publish.SetPublishTarget(r.Context(), store.PublishTarget{
		WorkspaceID:  ws.ID,
		Kind:         "github",
		RepoURL:      strings.TrimSpace(body.Repo),
		Branch:       branch,
		PathPrefix:   strings.TrimSpace(body.PathPrefix),
		CredentialID: body.CredentialID,
		Enabled:      enabled,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "status": "configured",
		// Said plainly, because it is the one surprising property: the
		// repository is a mirror, and hand edits there do not survive.
		"note": "the wiki is mirrored on every successful build; edits made in the repository are overwritten",
	})
}

func (s *Server) handlePublishPatch(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	if s.Publish == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no publish target"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPublishBodyBytes)

	var body struct {
		Repo         *string `json:"repo"`
		Branch       *string `json:"branch"`
		PathPrefix   *string `json:"pathPrefix"`
		CredentialID *string `json:"credentialId"`
		Enabled      *bool   `json:"enabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Repo != nil {
		if _, err := gitconn.ValidateRemoteURL(*body.Repo); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if body.CredentialID != nil && !s.credentialUsable(w, r, ws, *body.CredentialID) {
		return
	}

	err := s.Publish.PatchPublishTarget(r.Context(), ws.ID, store.PublishTargetPatch{
		RepoURL:      body.Repo,
		Branch:       body.Branch,
		PathPrefix:   body.PathPrefix,
		CredentialID: body.CredentialID,
		Enabled:      body.Enabled,
	})
	if err != nil {
		s.failOrNotFound(w, err, "no publish target")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handlePublishDelete stops the bench publishing. The repository keeps whatever
// was last pushed: kiln wrote those files, but it does not own the repository,
// and emptying someone's repo because they turned off a mirror would be
// indefensible.
func (s *Server) handlePublishDelete(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	if s.Publish == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no publish target"})
		return
	}
	if err := s.Publish.DeletePublishTarget(r.Context(), ws.ID); err != nil {
		s.failOrNotFound(w, err, "no publish target")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "removed",
		"note":   "already-published files were left in the repository",
	})
}
