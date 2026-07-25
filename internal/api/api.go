// Package api serves the REST surface and the embedded UI.
//
// The UI consumes exactly these endpoints -- there is no server-side rendering
// and no privileged internal route -- so a separate frontend, a CLI, or an MCP
// server needs no server change.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

// Server holds the API dependencies.
type Server struct {
	Store *store.JobStore
	DB    *store.DB
	Log   *slog.Logger
}

// Router builds the HTTP handler.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// Liveness answers even when the database is down: a failing readiness
	// check should not make an orchestrator kill a process that is merely
	// waiting on Postgres.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.handleReady)

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/version", s.handleVersion)
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

	mountUI(r)
	return r
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
	if err := s.DB.Ping(r.Context()); err != nil {
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

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Pool.Query(r.Context(), `
		SELECT ws.id, ws.slug, ws.name,
		       coalesce(wk.page_count, 0), coalesce(wk.revision, 0)
		FROM workspaces ws
		LEFT JOIN wikis wk ON wk.workspace_id = ws.id
		ORDER BY ws.slug`)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()

	out := []WorkspaceSummary{}
	for rows.Next() {
		var ws WorkspaceSummary
		if err := rows.Scan(&ws.ID, &ws.Slug, &ws.Name, &ws.PageCount, &ws.Revision); err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, ws)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolve(w, r)
	if !ok {
		return
	}

	var ws WorkspaceSummary
	err := s.DB.Pool.QueryRow(r.Context(), `
		SELECT ws.id, ws.slug, ws.name,
		       coalesce(wk.page_count, 0), coalesce(wk.revision, 0)
		FROM workspaces ws
		LEFT JOIN wikis wk ON wk.workspace_id = ws.id
		WHERE ws.id = $1`, id).Scan(&ws.ID, &ws.Slug, &ws.Name, &ws.PageCount, &ws.Revision)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
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
	id, ok := s.resolve(w, r)
	if !ok {
		return
	}

	pages, err := s.Store.LoadPages(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	out := make([]PageSummary, 0, len(pages))
	for _, p := range pages {
		out = append(out, PageSummary{
			Path: p.Path, Slug: p.Slug, Type: string(p.Meta.Type),
			Title: p.Meta.Title, Tags: p.Meta.Tags, Updated: p.Meta.Updated,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolve(w, r)
	if !ok {
		return
	}
	wanted := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	pages, err := s.Store.LoadPages(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	for _, p := range pages {
		// Addressable by path or by slug: a wikilink carries a slug, the tree
		// carries a path, and both should resolve.
		if p.Path == wanted || p.Slug == wanted {
			writeJSON(w, http.StatusOK, map[string]any{
				"path": p.Path, "slug": p.Slug, "type": string(p.Meta.Type),
				"title": p.Meta.Title, "tags": p.Meta.Tags,
				"related": p.Meta.Related, "sources": p.Meta.Sources,
				"updated": p.Meta.Updated, "builtAtRef": p.Meta.BuiltAtRef,
				"body": p.Body,
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "page not found"})
}

func (s *Server) handleArtifact(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.resolve(w, r)
		if !ok {
			return
		}
		body, err := s.Store.LoadArtifact(r.Context(), id, kind)
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
	id, ok := s.resolve(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, []SearchHit{})
		return
	}

	rows, err := s.DB.Pool.Query(r.Context(), `
		SELECT p.path, p.slug, p.type, p.title,
		       ts_rank(p.search, plainto_tsquery('english', $2)) AS rank
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1
		  AND p.deleted_at IS NULL
		  AND p.search @@ plainto_tsquery('english', $2)
		ORDER BY rank DESC
		LIMIT 50`, id, query)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()

	out := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.Path, &h.Slug, &h.Type, &h.Title, &h.Rank); err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, h)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGaps lists pages the wiki has declared it wants and does not have.
//
// Unresolved wikilinks are the cheapest and most precise gap signal available,
// and they are already materialized, so this is one query rather than a feature.
func (s *Server) handleGaps(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolve(w, r)
	if !ok {
		return
	}

	rows, err := s.DB.Pool.Query(r.Context(), `
		SELECT l.to_slug, count(*) AS wanted_by
		FROM page_links l
		JOIN pages p ON p.id = l.from_page_id
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND NOT l.resolved AND p.deleted_at IS NULL
		GROUP BY l.to_slug
		ORDER BY wanted_by DESC, l.to_slug`, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var slug string
		var count int
		if err := rows.Scan(&slug, &count); err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, map[string]any{"slug": slug, "wantedBy": count})
	}
	writeJSON(w, http.StatusOK, out)
}

// resolve turns the {workspace} path segment into an ID, accepting either a
// UUID or a slug so URLs stay readable.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (string, bool) {
	ref := chi.URLParam(r, "workspace")

	var id string
	err := s.DB.Pool.QueryRow(r.Context(), `
		SELECT id FROM workspaces
		WHERE slug = $1 OR id::text = $1
		LIMIT 1`, ref).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
		return "", false
	}
	return id, true
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
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
