package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/auth"
	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	webconn "github.com/daiwa-zou/kiln/internal/connector/web"
	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/store"
)

// The admin surface: connector and credential CRUD. Admin-only on purpose --
// a connector config decides what the worker reads and clones, and a
// credential is a secret. Neither is a thing an editor role should shape.

// AdminStore is what the admin routes need from persistence.
type AdminStore interface {
	ListConnectors(ctx context.Context, workspaceID string) ([]store.ConnectorRow, error)
	CreateConnector(ctx context.Context, c store.ConnectorRow) (string, error)
	UpdateConnector(ctx context.Context, workspaceID, id string, p store.ConnectorPatch) error
	DeleteConnector(ctx context.Context, workspaceID, id string) (diff.Cascade, error)

	CreateCredential(ctx context.Context, orgID, kind string, ciphertext, nonce []byte) (string, error)
	ListCredentialMeta(ctx context.Context, orgID string) ([]store.CredentialMeta, error)
	DeleteCredential(ctx context.Context, orgID, id string) error
	CredentialInOrg(ctx context.Context, orgID, id string) (bool, error)
	OrgOfWorkspace(ctx context.Context, workspaceID string) (string, error)
}

const maxAdminBodyBytes = 16 << 10

var (
	connectorKinds    = []string{"git", "upload", "web"}
	credentialKinds   = []string{"git_pat"}
	triggerModes      = []string{"manual", "webhook", "poll"}
	errNotAdmin       = "requires an instance admin or an owner of this bench's org"
	errNoKeyring      = "credentials are unavailable: no master key is configured (set KILN_MASTER_KEY and restart)"
	errUnknownKind    = "unknown kind"
	errUnknownTrigger = "unknown trigger mode"
)

// guardAdmin resolves the workspace and requires an instance admin or an
// owner of the workspace's org. Owners shape their own org — connectors,
// credentials, membership — which is the tenancy line M3 draws: editors and
// viewers get 403, and the workspace resolution before it keeps 404-vs-403
// behavior consistent with the rest of the API.
func (s *Server) guardAdmin(w http.ResponseWriter, r *http.Request) (store.WorkspaceRow, bool) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return store.WorkspaceRow{}, false
	}
	if s.Auth != nil {
		id, found := auth.FromContext(r.Context())
		if !found {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": errNotAdmin})
			return store.WorkspaceRow{}, false
		}
		// The admin scope is required in addition to the role: a token minted
		// for the human-loop write API (steering, corrections) must not be
		// able to seal credentials or reshape connectors. Browser sessions
		// carry every scope; automation tokens opt in at mint time.
		if !id.HasScope("admin") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "token lacks the admin scope"})
			return store.WorkspaceRow{}, false
		}
		if !id.Admin {
			role, err := s.Writes.WorkspaceRole(r.Context(), ws.ID, id.UserID)
			if err != nil {
				s.fail(w, err)
				return store.WorkspaceRow{}, false
			}
			if role != "owner" {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": errNotAdmin})
				return store.WorkspaceRow{}, false
			}
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	return ws, true
}

// connectorJSON shapes a connector for responses. The config is echoed back:
// it holds paths and URLs, not secrets -- secrets live in credentials.
func connectorJSON(c store.ConnectorRow) map[string]any {
	out := map[string]any{
		"id": c.ID, "kind": c.Kind, "name": c.Name, "config": c.Config,
		"triggerMode": c.TriggerMode, "enabled": c.Enabled,
		"created": c.CreatedAt.UTC().Format(time.RFC3339),
	}
	if c.CredentialID != "" {
		out["credentialId"] = c.CredentialID
	}
	if c.LastSyncedAt != nil {
		out["lastSynced"] = c.LastSyncedAt.UTC().Format(time.RFC3339)
	}
	if c.LastError != "" {
		out["lastError"] = c.LastError
	}
	return out
}

func (s *Server) handleConnectorsList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	rows, err := s.Admin.ListConnectors(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, connectorJSON(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// validateConnectorConfig applies the policy a config must pass before it is
// allowed to drive a worker: a git URL must clear the remote-clone policy
// here at write time as well as at sync time, so a bad config is rejected at
// the API instead of failing runs later.
func validateConnectorConfig(kind string, cfg map[string]any) string {
	rawURL, _ := cfg["url"].(string)
	path, _ := cfg["path"].(string)

	switch kind {
	case "git":
		if rawURL == "" && path == "" {
			return "git connector config needs url (https remote) or path (local, under worker.permitted_source_roots)"
		}
		if rawURL != "" {
			if _, err := gitconn.ValidateRemoteURL(rawURL); err != nil {
				return err.Error()
			}
		}
	case "upload":
		// No path means files mode: the connector consumes the workspace's
		// uploaded files from the blob store, which is the browser-upload
		// flow. An explicit path stays valid for worker-local directories and
		// is still gated by worker.permitted_source_roots at run time.
	case "web":
		urls := webconn.URLsFrom(cfg)
		if len(urls) == 0 {
			return "web connector config needs urls (an array of https addresses)"
		}
		// Reject bad URLs at write time exactly as the fetch would at sync
		// time; the private-address check happens at connect (pinned dial).
		for _, u := range urls {
			if _, err := webconn.ValidateURL(u); err != nil {
				return err.Error()
			}
		}
	}
	return ""
}

func (s *Server) handleConnectorCreate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind         string         `json:"kind"`
		Name         string         `json:"name"`
		Config       map[string]any `json:"config"`
		CredentialID string         `json:"credentialId"`
		TriggerMode  string         `json:"triggerMode"`
		Enabled      *bool          `json:"enabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !slices.Contains(connectorKinds, body.Kind) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errUnknownKind})
		return
	}
	if body.TriggerMode != "" && !slices.Contains(triggerModes, body.TriggerMode) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errUnknownTrigger})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if msg := validateConnectorConfig(body.Kind, body.Config); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if !s.credentialUsable(w, r, ws, body.CredentialID) {
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	id, err := s.Admin.CreateConnector(r.Context(), store.ConnectorRow{
		WorkspaceID: ws.ID, Kind: body.Kind, Name: body.Name,
		Config: body.Config, CredentialID: body.CredentialID,
		TriggerMode: body.TriggerMode, Enabled: enabled,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleConnectorPatch(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Name         *string         `json:"name"`
		Config       *map[string]any `json:"config"`
		CredentialID *string         `json:"credentialId"`
		TriggerMode  *string         `json:"triggerMode"`
		Enabled      *bool           `json:"enabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.TriggerMode != nil && !slices.Contains(triggerModes, *body.TriggerMode) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errUnknownTrigger})
		return
	}
	if body.Config != nil {
		// The patched config must clear the same policy as a created one; the
		// connector's stored kind decides which rules apply.
		c, err := s.connectorInWorkspace(r.Context(), ws.ID, chi.URLParam(r, "id"))
		if err != nil {
			s.failOrNotFound(w, err, "connector not found")
			return
		}
		if msg := validateConnectorConfig(c.Kind, *body.Config); msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
			return
		}
	}

	if body.CredentialID != nil && !s.credentialUsable(w, r, ws, *body.CredentialID) {
		return
	}

	err := s.Admin.UpdateConnector(r.Context(), ws.ID, chi.URLParam(r, "id"), store.ConnectorPatch{
		Name: body.Name, Config: body.Config, CredentialID: body.CredentialID,
		TriggerMode: body.TriggerMode, Enabled: body.Enabled,
	})
	if err != nil {
		s.failOrNotFound(w, err, "connector not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": chi.URLParam(r, "id"), "status": "updated"})
}

// handleConnectorDelete removes a connector, the sources it synced, and the
// wiki content only those sources produced. Removing the repository a bench
// reads and keeping the pages describing it is not a safer outcome, only a
// quieter one: the wiki would go on asserting things with nothing left to
// check them against.
func (s *Server) handleConnectorDelete(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	cascade, err := s.Admin.DeleteConnector(r.Context(), ws.ID, chi.URLParam(r, "id"))
	if err != nil {
		s.failOrNotFound(w, err, "connector not found")
		return
	}
	if s.Blobs != nil {
		for _, key := range cascade.DeleteBlobs {
			s.deleteBlobQuietly(r.Context(), key)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": chi.URLParam(r, "id"), "status": "deleted",
		"pagesRemoved":      len(cascade.DeletePages),
		"pagesRegenerating": len(cascade.RegeneratePages),
	})
}

// credentialUsable verifies a referenced credential belongs to the
// workspace's own org. The schema does not enforce this; without the check, a
// connector row could name another tenant's secret and the worker would
// decrypt it at sync time. Empty ids (no credential, or clearing one) pass.
// Returns false after writing the response.
func (s *Server) credentialUsable(w http.ResponseWriter, r *http.Request, ws store.WorkspaceRow, credentialID string) bool {
	if credentialID == "" {
		return true
	}
	orgID, err := s.Admin.OrgOfWorkspace(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	ok, err := s.Admin.CredentialInOrg(r.Context(), orgID, credentialID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	if !ok {
		// Absent and foreign answer identically, so credential ids cannot be
		// probed across org boundaries.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "credential not found"})
		return false
	}
	return true
}

// connectorInWorkspace loads one connector and confirms it belongs to the
// workspace, so a cross-tenant id reads as absent.
func (s *Server) connectorInWorkspace(ctx context.Context, workspaceID, id string) (*store.ConnectorRow, error) {
	rows, err := s.Admin.ListConnectors(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, store.ErrNotFound
}

// handleCredentialCreate seals a secret and stores it. Write-only: the
// response carries the id, never the secret, and no read route returns it.
func (s *Server) handleCredentialCreate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	if s.Keyring == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": errNoKeyring})
		return
	}
	var body struct {
		Kind   string `json:"kind"`
		Secret string `json:"secret"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !slices.Contains(credentialKinds, body.Kind) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errUnknownKind})
		return
	}
	if strings.TrimSpace(body.Secret) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "secret is empty"})
		return
	}

	orgID, err := s.Admin.OrgOfWorkspace(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	ciphertext, nonce, err := s.Keyring.Seal([]byte(body.Secret))
	if err != nil {
		s.fail(w, err)
		return
	}
	id, err := s.Admin.CreateCredential(r.Context(), orgID, body.Kind, ciphertext, nonce)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "kind": body.Kind})
}

func (s *Server) handleCredentialsList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	orgID, err := s.Admin.OrgOfWorkspace(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	rows, err := s.Admin.ListCredentialMeta(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"id": c.ID, "kind": c.Kind,
			"created": c.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCredentialDelete(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.guardAdmin(w, r)
	if !ok {
		return
	}
	orgID, err := s.Admin.OrgOfWorkspace(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Admin.DeleteCredential(r.Context(), orgID, chi.URLParam(r, "id")); err != nil {
		s.failOrNotFound(w, err, "credential not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": chi.URLParam(r, "id"), "status": "deleted"})
}

func (s *Server) failOrNotFound(w http.ResponseWriter, err error, msg string) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": msg})
		return
	}
	s.fail(w, err)
}

// Keyring is re-exported narrowly so serve wiring reads naturally.
type Keyring = crypto.Keyring
