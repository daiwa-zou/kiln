// Package api serves the REST surface and the embedded UI.
//
// The UI consumes exactly these endpoints -- there is no server-side rendering
// and no privileged internal route -- so a separate frontend, a CLI, or an MCP
// server needs no server change.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// Store is the read surface the API depends on. An interface so handlers can
// be tested against a fake; *store.WikiStore is the production implementation.
type Store interface {
	ListWorkspaces(ctx context.Context, userID string, admin bool) ([]store.WorkspaceRow, error)
	ResolveWorkspace(ctx context.Context, ref, userID string, admin bool) (store.WorkspaceRow, error)
	LoadPageSummaries(ctx context.Context, workspaceID string, limit, offset int) ([]store.PageInfo, error)
	LoadPage(ctx context.Context, workspaceID, ref string) (wiki.Page, error)
	Search(ctx context.Context, workspaceID, query string, limit, offset int) ([]store.SearchHit, error)
	Gaps(ctx context.Context, workspaceID string, limit, offset int) ([]store.Gap, error)
	LoadArtifact(ctx context.Context, workspaceID, kind string) (string, error)
}

// Server holds the API dependencies.
type Server struct {
	Store Store
	// DB backs the readiness probe only; every content query goes through
	// Store so handlers stay testable without Postgres.
	DB  *store.DB
	Log *slog.Logger
	// Auth, when non-nil, guards every /api/v1 route except /version. Nil
	// means authentication was disabled by configuration; visibility then
	// behaves as admin, which is only defensible on a localhost deployment.
	Auth *auth.Middleware
	// CORSOrigins are origins allowed to call the API from a browser, for a
	// separately hosted frontend. Empty means same-origin only.
	CORSOrigins []string
}

// Pagination bounds. Defaults serve the UI; ceilings stop a caller from
// turning a listing into a full-table dump.
const (
	defaultPageLimit = 500
	maxPageLimit     = 1000
	defaultGapLimit  = 200
	defaultHitLimit  = 50
	maxHitLimit      = 200
)

// Router builds the HTTP handler.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	if len(s.CORSOrigins) > 0 {
		r.Use(corsMiddleware(s.CORSOrigins))
	}

	// Liveness answers even when the database is down: a failing readiness
	// check should not make an orchestrator kill a process that is merely
	// waiting on Postgres.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.handleReady)

	r.Route("/api/v1", func(r chi.Router) {
		// Version stays outside auth so the UI can render a sensible sign-in
		// state; it discloses nothing about content.
		r.Get("/version", s.handleVersion)

		r.Group(func(r chi.Router) {
			if s.Auth != nil {
				r.Use(s.Auth.Wrap)
			}
			r.Get("/workspaces", s.handleWorkspaces)

			r.Route("/workspaces/{workspace}", func(r chi.Router) {
				r.Get("/", s.handleWorkspace)
				r.Get("/pages", s.handlePages)
				r.Get("/pages/*", s.handlePage)
				r.Get("/index", s.handleArtifact("index"))
				r.Get("/overview", s.handleArtifact("overview"))
				r.Get("/log", s.handleArtifact("log"))
				r.Get("/search", s.handleSearch)
				r.Get("/gaps", s.handleGaps)
			})
		})
	})

	mountUI(r)
	return r
}

// corsMiddleware allows the configured origins to call the API from a
// browser. The allowlist is exact-match: reflecting arbitrary origins would
// undo the point of having one.
func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && slices.Contains(origins, origin) {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
				h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-None-Match")
				h.Set("Access-Control-Max-Age", "600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// scope returns the caller's visibility: an admin sees every org, a user sees
// the orgs they belong to, and a deployment with auth disabled behaves as
// admin. Fails closed when auth is enabled but no identity is present.
func (s *Server) scope(r *http.Request) (userID string, admin bool) {
	if s.Auth == nil {
		return "", true
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		return "", false
	}
	return id.UserID, id.Admin
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	// The UI compares this to its own build so a skewed pair is visible rather
	// than mysterious.
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       observability.Version,
		"schemaVersion": store.SchemaVersion,
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil || s.DB.Ping(r.Context()) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unready", "reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// WorkspaceSummary is a workspace as the list endpoint returns it.
type WorkspaceSummary struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	PageCount int    `json:"pageCount"`
	Revision  int64  `json:"revision"`
}

func summaryOf(ws store.WorkspaceRow) WorkspaceSummary {
	return WorkspaceSummary{
		ID: ws.ID, Slug: ws.Slug, Name: ws.Name,
		PageCount: ws.PageCount, Revision: ws.Revision,
	}
}

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	uid, admin := s.scope(r)
	rows, err := s.Store.ListWorkspaces(r.Context(), uid, admin)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]WorkspaceSummary, 0, len(rows))
	for _, ws := range rows {
		out = append(out, summaryOf(ws))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, summaryOf(ws))
}

// PageSummary is a page without its body, for listings.
type PageSummary struct {
	Path    string   `json:"path"`
	Slug    string   `json:"slug"`
	Type    string   `json:"type"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags,omitempty"`
	Updated string   `json:"updated,omitempty"`
}

func (s *Server) handlePages(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok || notModified(w, r, ws) {
		return
	}
	limit, offset := pagination(r, defaultPageLimit, maxPageLimit)

	pages, err := s.Store.LoadPageSummaries(r.Context(), ws.ID, limit, offset)
	if err != nil {
		s.fail(w, err)
		return
	}

	out := make([]PageSummary, 0, len(pages))
	for _, p := range pages {
		out = append(out, PageSummary{
			Path: p.Path, Slug: p.Slug, Type: p.Type,
			Title: p.Title, Tags: p.Tags, Updated: p.Updated,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok || notModified(w, r, ws) {
		return
	}
	wanted := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	p, err := s.Store.LoadPage(r.Context(), ws.ID, wanted)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "page not found"})
		return
	case err != nil:
		s.fail(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path": p.Path, "slug": p.Slug, "type": string(p.Meta.Type),
		"title": p.Meta.Title, "tags": p.Meta.Tags,
		"related": p.Meta.Related, "sources": p.Meta.Sources,
		"updated": p.Meta.Updated, "builtAtRef": p.Meta.BuiltAtRef,
		"body": p.Body,
	})
}

func (s *Server) handleArtifact(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, ok := s.resolve(w, r)
		if !ok || notModified(w, r, ws) {
			return
		}
		body, err := s.Store.LoadArtifact(r.Context(), ws.ID, kind)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"kind": kind, "body": body})
	}
}

// SearchHit is one full-text match.
type SearchHit struct {
	Path  string  `json:"path"`
	Slug  string  `json:"slug"`
	Type  string  `json:"type"`
	Title string  `json:"title"`
	Rank  float64 `json:"rank"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, []SearchHit{})
		return
	}
	limit, offset := pagination(r, defaultHitLimit, maxHitLimit)

	hits, err := s.Store.Search(r.Context(), ws.ID, query, limit, offset)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]SearchHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, SearchHit{Path: h.Path, Slug: h.Slug, Type: h.Type, Title: h.Title, Rank: h.Rank})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGaps lists pages the wiki has declared it wants and does not have.
func (s *Server) handleGaps(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok || notModified(w, r, ws) {
		return
	}
	limit, offset := pagination(r, defaultGapLimit, maxPageLimit)

	gaps, err := s.Store.Gaps(r.Context(), ws.ID, limit, offset)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, map[string]any{"slug": g.Slug, "wantedBy": g.WantedBy})
	}
	writeJSON(w, http.StatusOK, out)
}

// resolve turns the {workspace} path segment into a workspace, bounded to
// what the caller can see. Absence and no-access answer identically, so a
// slug cannot be probed for existence across org boundaries.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (store.WorkspaceRow, bool) {
	ref := chi.URLParam(r, "workspace")
	uid, admin := s.scope(r)

	ws, err := s.Store.ResolveWorkspace(r.Context(), ref, uid, admin)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
		return store.WorkspaceRow{}, false
	case err != nil:
		// A dead database is an outage, not a missing workspace.
		s.fail(w, err)
		return store.WorkspaceRow{}, false
	}
	return ws, true
}

// notModified serves conditional requests off the wiki revision, which exists
// precisely to bust caches: content only changes when an import bumps it.
// Returns true when a 304 was written and the handler should stop.
func notModified(w http.ResponseWriter, r *http.Request, ws store.WorkspaceRow) bool {
	tag := fmt.Sprintf(`W/"%s-%d"`, ws.ID, ws.Revision)
	w.Header().Set("ETag", tag)
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// pagination reads limit/offset with a default and a ceiling.
func pagination(r *http.Request, def, ceil int) (limit, offset int) {
	limit = def
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, ceil)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if s.Log != nil {
		s.Log.Error("api request failed", "err", err)
	}
	// The detail goes to the log, not the response: a database error can carry
	// schema and connection details a client has no business seeing.
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Serve runs the HTTP server until the context is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		// Full-cycle timeouts so a slow-loris client or a stalled write cannot
		// pin a connection forever. The handler-level chi Timeout (30s) fires
		// first for well-behaved requests; these are the transport backstop.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
