package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

// WorkspaceCreator is the self-serve lifecycle surface: making a bench and
// getting rid of one. Separate from Store so a read-only deployment can leave
// it nil and simply not mount the routes.
type WorkspaceCreator interface {
	CreateWorkspace(ctx context.Context, orgSlug, wsSlug, name, userID string, instanceAdmin bool) (string, error)
	// DeleteWorkspace returns the blob keys the bench's uploads referenced, so
	// the caller can free the objects the database cascade cannot reach.
	DeleteWorkspace(ctx context.Context, workspaceID string) ([]string, error)
}

const maxWorkspaceBodyBytes = 4 << 10

// slugPattern is what a URL-safe, lowercase identifier looks like.
//
// Case is normalized -- "Team-Handbook" becomes "team-handbook", which is what
// anyone typing it expects -- but nothing else is. Invalid characters are
// rejected rather than stripped: dropping them silently produces a bench under
// a name the user never chose and cannot guess later.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

const maxSlugLen = 63

// handleWorkspaceCreate lets a signed-in user make their own bench.
//
// Without it, a user who signs in and belongs to no org sees an empty list
// with nothing to click: workspaces could only be created by `kiln build` or
// `kiln admin token create` on the operator's machine. Creating one here makes
// the creator an owner of its org, so they can immediately add sources,
// members, and credentials.
func (s *Server) handleWorkspaceCreate(w http.ResponseWriter, r *http.Request) {
	// Auth is required even in a deployment with auth disabled the caller is
	// an admin -- there is no anonymous ownership to grant.
	var (
		userID string
		admin  bool
	)
	if s.Auth != nil {
		id, ok := auth.FromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		if !id.HasScope("write") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "token lacks the write scope"})
			return
		}
		userID, admin = id.UserID, id.Admin
	} else {
		admin = true
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceBodyBytes)
	var body struct {
		Org  string `json:"org"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	slug := strings.ToLower(strings.TrimSpace(body.Slug))
	if !validSlug(slug) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "slug must be lowercase letters, numbers, and single dashes (e.g. \"team-handbook\")",
		})
		return
	}
	org := strings.ToLower(strings.TrimSpace(body.Org))
	if org == "" {
		// Default to the bench's own slug as its org, so a first-time user
		// gets a working bench without having to understand orgs at all.
		org = slug
	}
	if !validSlug(org) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "org must be lowercase letters, numbers, and single dashes",
		})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = slug
	}
	if len(name) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is too long"})
		return
	}

	id, err := s.Workspaces.CreateWorkspace(r.Context(), org, slug, name, userID, admin)
	switch {
	case errors.Is(err, store.ErrWorkspaceExists):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "a bench with that slug already exists in this org",
		})
		return
	case errors.Is(err, store.ErrOrgForbidden):
		// Deliberately indistinguishable from "no such org": telling a caller
		// that an org exists but is not theirs is a tenant-enumeration oracle.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "org not found, or you do not own it",
		})
		return
	case err != nil:
		s.fail(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "slug": slug, "org": org, "name": name,
	})
}

// handleWorkspaceDelete removes a bench and everything in it.
//
// This is the most destructive thing the API does. Deleting a source is also
// immediate now, but its pages are soft-deleted and recoverable for the
// retention window; nothing here is staged and nothing is recoverable. So it
// asks the caller to name what they are deleting: the slug in the request must
// match the slug in the path. A DELETE that fires by accident, from a
// mis-scripted loop or a retried request, cannot supply that.
//
// guardAdmin is the gate the rest of the tenancy-shaping routes use: an
// instance admin, or an owner of the bench's org, holding a token with the
// admin scope. An editor with write access can queue builds and resolve
// reviews; they cannot delete the bench those builds write to.
func (s *Server) handleWorkspaceDelete(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceBodyBytes)
	var body struct {
		Slug string `json:"slug"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Slug) != ws.Slug {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "to delete this bench, repeat its slug in the request body",
		})
		return
	}

	keys, err := s.Workspaces.DeleteWorkspace(r.Context(), ws.ID)
	if err != nil {
		s.failOrNotFound(w, err, "bench not found")
		return
	}
	// The bench is gone from the database whatever happens next; an object
	// store that refuses leaves bytes nothing points at, which is a cleanup
	// job rather than something to fail the request over.
	if s.Blobs != nil {
		for _, k := range keys {
			s.deleteBlobQuietly(r.Context(), k)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"slug": ws.Slug, "status": "deleted"})
}

func validSlug(s string) bool {
	return s != "" && len(s) <= maxSlugLen && slugPattern.MatchString(s)
}
