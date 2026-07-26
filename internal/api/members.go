package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

// Member management: owners (and instance admins) shape who belongs to the
// org behind a workspace and with what role. Guarded by guardAdmin, the same
// gate the connector and credential surfaces use.

// MemberStore is what membership management needs from persistence.
type MemberStore interface {
	ListOrgMembers(ctx context.Context, workspaceID string) ([]store.MemberRow, error)
	AddOrgMember(ctx context.Context, workspaceID, login, role string) (*store.MemberRow, error)
	SetMemberRole(ctx context.Context, workspaceID, userID, role string) error
	RemoveOrgMember(ctx context.Context, workspaceID, userID string) error
}

func memberJSON(m store.MemberRow) map[string]any {
	return map[string]any{
		"userId": m.UserID, "login": m.Login, "name": m.Name,
		"avatarUrl": m.AvatarURL, "role": m.Role,
		"since": m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleMembersList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	rows, err := s.Members.ListOrgMembers(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, memberJSON(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMemberAdd(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Login string `json:"login"`
		Role  string `json:"role"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Login) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "login is required"})
		return
	}
	if !slices.Contains(store.MemberRoles, body.Role) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be viewer, member, or owner"})
		return
	}

	m, err := s.Members.AddOrgMember(r.Context(), ws.ID, body.Login, body.Role)
	if err != nil {
		s.failOrNotFound(w, err, "no such user; they must sign in or be minted a token first")
		return
	}
	writeJSON(w, http.StatusCreated, memberJSON(*m))
}

func (s *Server) handleMemberPatch(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !slices.Contains(store.MemberRoles, body.Role) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be viewer, member, or owner"})
		return
	}
	target := chi.URLParam(r, "userID")

	// An owner demoting themselves is allowed — but not into a lockout: the
	// last owner keeps the keys unless an instance admin intervenes.
	if body.Role != "owner" && !s.callerIsAdmin(r) {
		if locked, err := s.wouldRemoveLastOwner(r.Context(), ws.ID, target); err != nil {
			s.fail(w, err)
			return
		} else if locked {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot demote the last owner"})
			return
		}
	}

	if err := s.Members.SetMemberRole(r.Context(), ws.ID, target, body.Role); err != nil {
		s.failOrNotFound(w, err, "member not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"userId": target, "role": body.Role})
}

func (s *Server) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	target := chi.URLParam(r, "userID")

	if !s.callerIsAdmin(r) {
		if locked, err := s.wouldRemoveLastOwner(r.Context(), ws.ID, target); err != nil {
			s.fail(w, err)
			return
		} else if locked {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot remove the last owner"})
			return
		}
	}

	if err := s.Members.RemoveOrgMember(r.Context(), ws.ID, target); err != nil {
		s.failOrNotFound(w, err, "member not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"userId": target, "status": "removed"})
}

func (s *Server) callerIsAdmin(r *http.Request) bool {
	if s.Auth == nil {
		return true
	}
	id, ok := auth.FromContext(r.Context())
	return ok && id.Admin
}

// wouldRemoveLastOwner reports whether taking owner away from target leaves
// the org ownerless.
func (s *Server) wouldRemoveLastOwner(ctx context.Context, workspaceID, targetUserID string) (bool, error) {
	members, err := s.Members.ListOrgMembers(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	owners := 0
	targetIsOwner := false
	for _, m := range members {
		if m.Role == "owner" {
			owners++
			if m.UserID == targetUserID {
				targetIsOwner = true
			}
		}
	}
	return targetIsOwner && owners == 1, nil
}
