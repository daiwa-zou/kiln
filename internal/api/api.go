// Package api serves the REST surface and the embedded UI.
//
// The UI consumes exactly these endpoints -- there is no server-side rendering
// and no privileged internal route -- so a separate frontend, a CLI, or an MCP
// server needs no server change.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/github"
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
	Search(ctx context.Context, workspaceID, query string, limit, offset int, prefix bool) ([]store.SearchHit, error)
	Gaps(ctx context.Context, workspaceID string, limit, offset int) ([]store.Gap, error)
	LoadArtifact(ctx context.Context, workspaceID, kind string) (string, error)
	Graph(ctx context.Context, workspaceID string, limit int) ([]store.GraphNode, []store.GraphEdge, int, error)
}

// WriteStore is the human-loop surface: steering, corrections, the review
// queue, backlinks, and the role lookup that gates mutation. Separate from
// Store so the read-only handlers keep their narrow dependency.
type WriteStore interface {
	UpsertSteeringDoc(ctx context.Context, workspaceID, kind, body, updatedBy string) error
	LoadSteeringDocs(ctx context.Context, workspaceID string) (map[string]string, error)
	CreateCorrection(ctx context.Context, workspaceID, pageRef, body, createdBy string) (string, error)
	ListCorrections(ctx context.Context, workspaceID, pageRef string) ([]store.CorrectionRow, error)
	SetCorrectionActive(ctx context.Context, workspaceID, correctionID string, active bool) error
	ListReviews(ctx context.Context, workspaceID, status string, limit, offset int) ([]store.ReviewRow, error)
	ResolveReview(ctx context.Context, workspaceID, reviewID, action, resolvedBy string) error
	RequestResearch(ctx context.Context, workspaceID, reviewID string) (runID string, err error)
	Backlinks(ctx context.Context, workspaceID, slug string) ([]store.PageInfo, error)
	WorkspaceRole(ctx context.Context, workspaceID, userID string) (string, error)
}

// Server holds the API dependencies.
type Server struct {
	Store Store
	// Writes backs the human-loop routes. Nil disables them (405/501 would
	// lie; the routes are simply not mounted), which keeps read-only embeds
	// of this server working unchanged.
	Writes WriteStore
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
	// PublicURL is the absolute origin clients reach this instance on. It is
	// the only thing that survives a reverse proxy, so the MCP discovery
	// documents are built from it rather than from the inbound request.
	PublicURL string
	// MCPAuthorizationServer is the issuer URL of an OAuth authorization
	// server that mints tokens for the MCP endpoint. Empty leaves
	// authorization_servers out of the protected-resource metadata, which is
	// the honest document for a deployment that authenticates with kiln API
	// tokens or not at all.
	MCPAuthorizationServer string
	// Runs backs the run queue routes. Nil leaves them unmounted, matching
	// how Writes disables the human loop.
	Runs RunStore
	// BudgetWindow is the rolling window workspace budgets apply to; zero
	// disables budget enforcement at enqueue.
	BudgetWindow time.Duration
	// SourcePollInterval mirrors the worker's poll cadence so the UI can say
	// when poll-mode sources are next checked. Informational only; the worker
	// process's own config remains the authority on when polls actually run.
	SourcePollInterval time.Duration
	// Admin backs the connector and credential CRUD. Nil leaves those routes
	// unmounted.
	Admin AdminStore
	// Files backs the uploaded-documents routes. Nil leaves them unmounted.
	Files FileStore
	// Workspaces backs self-serve bench creation. Nil leaves it unmounted,
	// which keeps a read-only embed unable to create anything.
	Workspaces WorkspaceCreator
	// Blobs stores uploaded file content. Nil (object storage unconfigured)
	// keeps listing and deletion working but answers uploads with 503.
	Blobs blob.Store
	// Members backs org membership management. Nil leaves it unmounted.
	Members MemberStore
	// Keyring seals credentials at write time. Nil (no master key configured)
	// keeps connector CRUD working but answers credential writes with 503.
	Keyring *Keyring

	// GitHub, Users, SessionPool, and SessionTTL enable browser sign-in. The
	// /auth routes mount only when the GitHub client has OAuth credentials.
	GitHub      *github.Client
	Users       UserStore
	SessionPool *pgxpool.Pool
	SessionTTL  time.Duration

	// Hooks and WebhookSecret enable /hooks/github; the route mounts only
	// when both are present. WebhookCooldown is the quiet period after a
	// finished run during which pushes do not start another.
	Hooks           HookStore
	WebhookSecret   []byte
	WebhookCooldown time.Duration

	// hookLimit buckets webhook deliveries per remote host; created by Router().
	hookLimit *limiter

	// writeLimit buckets mutating requests per caller; created by Router().
	writeLimit *limiter

	// uploadLimit buckets file uploads separately from other writes: dropping
	// a folder of documents is one user action that arrives as many requests,
	// so it gets a roomier bucket than the human-paced write surface.
	uploadLimit *limiter

	// built is the router Router() last returned, so the MCP endpoint's tools
	// can dispatch their reads back through it without leaving the process.
	// Set at the end of Router(), read long afterwards on a request, so it is
	// guarded: Router() may be called again by a test while requests are in
	// flight from an earlier call.
	built   http.Handler
	builtMu sync.RWMutex

	// Metrics, when set, instruments every route. Nil leaves the server
	// uninstrumented, which is what tests and embedded uses want.
	Metrics *observability.Metrics
}

// Pagination bounds. Defaults serve the UI; ceilings stop a caller from
// turning a listing into a full-table dump.
const (
	defaultPageLimit = 500
	maxPageLimit     = 1000
	defaultGapLimit  = 200
	defaultHitLimit  = 50
	maxHitLimit      = 200
	// Graph node caps: 300 nodes is where the client layout stays smooth,
	// and hubs-first truncation keeps a capped graph informative.
	defaultGraphNodes = 300
	maxGraphNodes     = 600
)

// Router builds the HTTP handler.
func (s *Server) Router() http.Handler {
	if s.writeLimit == nil {
		s.writeLimit = newLimiter()
	}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	if s.Metrics != nil {
		// Outermost of the observability middleware so it sees the status
		// Recoverer produces for a panic, rather than reporting the request
		// that panicked as a success.
		r.Use(metricsMiddleware(s.Metrics))
	}
	if len(s.CORSOrigins) > 0 {
		r.Use(corsMiddleware(s.CORSOrigins))
	}

	// File uploads sit in their own group because the 30-second request
	// timeout every other route lives under would kill a large document on a
	// slow uplink mid-stream. Same auth wrap, own limiter, longer leash.
	if s.Files != nil {
		if s.uploadLimit == nil {
			s.uploadLimit = newLimiterSized(uploadBurst, uploadRefillEach)
		}
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(uploadTimeout))
			if s.Auth != nil {
				r.Use(s.Auth.Wrap)
			}
			r.With(writeLimiter(s.uploadLimit)).
				Post("/api/v1/workspaces/{workspace}/files", s.handleFileUpload)
		})
	}

	// The MCP endpoint sits outside the 30-second handler timeout: Streamable
	// HTTP holds a response open to stream results back, which that timeout
	// would sever mid-session. Claude allows five minutes for a tool call, and
	// the transport-level WriteTimeout is the real backstop.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(mcpTimeout))
		s.mountMCP(r)
	})

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))
		s.mountRoutes(r)
	})

	mountUI(r)

	s.builtMu.Lock()
	s.built = r
	s.builtMu.Unlock()
	return r
}

// handler returns the router, for the MCP endpoint's in-process reads.
func (s *Server) handler() http.Handler {
	s.builtMu.RLock()
	defer s.builtMu.RUnlock()
	return s.built
}

// mountRoutes registers everything except the upload route, under the
// standard request timeout.
func (s *Server) mountRoutes(r chi.Router) {
	// Liveness answers even when the database is down: a failing readiness
	// check should not make an orchestrator kill a process that is merely
	// waiting on Postgres.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.handleReady)

	// Sign-in lives outside the API auth wrap by nature: its whole job is to
	// create the credentials the wrap checks.
	if s.GitHub.SignInConfigured() && s.Users != nil && s.SessionPool != nil {
		r.Get("/auth/github/login", s.handleGitHubLogin)
		r.Get("/auth/github/callback", s.handleGitHubCallback)
		r.Post("/auth/logout", s.handleLogout)
	}

	// Webhook ingress: GitHub cannot carry a kiln token, so this sits outside
	// auth.Wrap behind its own HMAC check, size cap, and rate limit.
	if s.Hooks != nil && len(s.WebhookSecret) > 0 {
		if s.hookLimit == nil {
			s.hookLimit = newLimiter()
		}
		r.With(writeLimiter(s.hookLimit)).Post("/hooks/github", s.handleGitHubWebhook)
	}

	r.Route("/api/v1", func(r chi.Router) {
		// Version stays outside auth so the UI can render a sensible sign-in
		// state; it discloses nothing about content.
		r.Get("/version", s.handleVersion)

		r.Group(func(r chi.Router) {
			if s.Auth != nil {
				r.Use(s.Auth.Wrap)
			}
			if s.Users != nil {
				r.Get("/me", s.handleMe)
			}
			r.Get("/workspaces", s.handleWorkspaces)
			if s.Workspaces != nil {
				r.With(writeLimiter(s.writeLimit)).
					Post("/workspaces", s.handleWorkspaceCreate)
			}

			r.Route("/workspaces/{workspace}", func(r chi.Router) {
				r.Get("/", s.handleWorkspace)
				r.Get("/pages", s.handlePages)
				r.Get("/pages/*", s.handlePage)
				r.Get("/index", s.handleArtifact("index"))
				r.Get("/overview", s.handleArtifact("overview"))
				r.Get("/log", s.handleArtifact("log"))
				r.Get("/search", s.handleSearch)
				r.Get("/gaps", s.handleGaps)
				r.Get("/graph", s.handleGraph)

				if s.Runs != nil && s.Writes != nil {
					// The run queue: listing is a read; enqueueing shares the
					// write limiter and role gate, because a rebuild spends
					// real money.
					r.Get("/runs", s.handleRunsList)
					r.Get("/runs/{id}/items", s.handleRunItems)
					r.Group(func(r chi.Router) {
						r.Use(writeLimiter(s.writeLimit))
						r.Post("/runs", s.handleRunCreate)
					})
				}

				if s.Admin != nil {
					// Connector and credential CRUD: admin-only inside the
					// handlers, rate-limited with the other mutations.
					r.Group(func(r chi.Router) {
						r.Use(writeLimiter(s.writeLimit))
						r.Get("/connectors", s.handleConnectorsList)
						r.Post("/connectors", s.handleConnectorCreate)
						r.Patch("/connectors/{id}", s.handleConnectorPatch)
						r.Delete("/connectors/{id}", s.handleConnectorDelete)
						r.Get("/credentials", s.handleCredentialsList)
						r.Post("/credentials", s.handleCredentialCreate)
						r.Delete("/credentials/{id}", s.handleCredentialDelete)
					})
				}

				if s.Files != nil {
					// Uploaded documents: listing is a read; deletion shares
					// the write limiter. The upload POST itself lives outside
					// this subtree, under the longer timeout.
					r.Get("/files", s.handleFilesList)
					r.Group(func(r chi.Router) {
						r.Use(writeLimiter(s.writeLimit))
						r.Patch("/files/{id}", s.handleFilePatch)
						r.Delete("/files/{id}", s.handleFileDelete)
					})
				}

				if s.Members != nil {
					// Membership: the same owner/admin gate as connectors.
					r.Group(func(r chi.Router) {
						r.Use(writeLimiter(s.writeLimit))
						r.Get("/members", s.handleMembersList)
						r.Post("/members", s.handleMemberAdd)
						r.Patch("/members/{userID}", s.handleMemberPatch)
						r.Delete("/members/{userID}", s.handleMemberRemove)
					})
				}

				if s.Writes == nil {
					return
				}
				// The human loop: reads sit with the other reads; mutations
				// share a rate limiter so a script cannot flood the queue.
				r.Get("/backlinks/{slug}", s.handleBacklinks)
				r.Get("/steering", s.handleSteeringGet)
				r.Get("/reviews", s.handleReviews)
				r.Get("/corrections/*", s.handleCorrectionsList)

				r.Group(func(r chi.Router) {
					r.Use(writeLimiter(s.writeLimit))
					r.Put("/steering/{kind}", s.handleSteeringPut)
					r.Post("/corrections/*", s.handleCorrectionCreate)
					// Singular on purpose: /corrections/* is the per-page
					// wildcard (a page ref may contain slashes), and sharing
					// that subtree with an {id} param would leave method
					// dispatch to router tie-breaking rules.
					r.Patch("/correction/{id}", s.handleCorrectionPatch)
					r.Post("/reviews/{id}/resolve", s.handleReviewResolve)
					// Queueing research spends real money, so it sits behind
					// the same write limiter and role gate as a rebuild. The
					// route mounts only when there is a queue to put it on:
					// without one the button would file a run nothing claims.
					if s.Runs != nil {
						r.Post("/reviews/{id}/research", s.handleReviewResearch)
					}
				})
			})
		})
	})
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
				h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS, POST, PUT, PATCH")
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
	// than mysterious. githubSignIn tells the sign-in form whether to offer
	// the OAuth button; it discloses only that the deployment configured it.
	payload := map[string]any{
		"version":       observability.Version,
		"schemaVersion": store.SchemaVersion,
		"githubSignIn":  s.GitHub.SignInConfigured(),
	}
	if s.SourcePollInterval > 0 {
		payload["sourcePollIntervalSeconds"] = int(s.SourcePollInterval.Seconds())
	}
	writeJSON(w, http.StatusOK, payload)
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
	Path    string  `json:"path"`
	Slug    string  `json:"slug"`
	Type    string  `json:"type"`
	Title   string  `json:"title"`
	Rank    float64 `json:"rank"`
	Snippet string  `json:"snippet,omitempty"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	// Results are a pure function of (workspace revision, query), so they are
	// exactly as cacheable as the page list -- and the ETag already mixes the
	// query string in. This matters more than it used to: search is the most
	// expensive read and the one agents hit most, and a polling agent that
	// re-asks the same question should get a 304 rather than a fresh scan.
	ws, ok := s.resolve(w, r)
	if !ok || notModified(w, r, ws) {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, []SearchHit{})
		return
	}
	limit, offset := pagination(r, defaultHitLimit, maxHitLimit)
	// prefix=1 says the last word may still be half-typed, so match it as a
	// prefix. Opt-in rather than always-on: it is what the UI's search box
	// needs on every keystroke and the opposite of what an agent submitting a
	// finished query wants, and the default has to keep meaning exact terms.
	prefix := r.URL.Query().Get("prefix") == "1"

	hits, err := s.Store.Search(r.Context(), ws.ID, query, limit, offset, prefix)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Only the first page is counted: paging past the end of a result set is
	// not a search that found nothing, and counting it would make the empty
	// rate a function of how far callers scroll.
	if s.Metrics != nil && offset == 0 {
		s.Metrics.SearchObserved(len(hits))
	}
	out := make([]SearchHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, SearchHit{
			Path: h.Path, Slug: h.Slug, Type: h.Type,
			Title: h.Title, Rank: h.Rank, Snippet: h.Snippet,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGraph returns the page graph: live pages and the resolved links
// between them. ETag'd on the wiki revision like every content read — links
// only change on import.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok || notModified(w, r, ws) {
		return
	}
	limit, _ := pagination(r, defaultGraphNodes, maxGraphNodes)
	nodes, edges, total, err := s.Store.Graph(r.Context(), ws.ID, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	outNodes := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		outNodes = append(outNodes, map[string]any{
			"slug": n.Slug, "title": n.Title, "type": n.Type, "links": n.Links,
		})
	}
	outEdges := make([]map[string]string, 0, len(edges))
	for _, e := range edges {
		outEdges = append(outEdges, map[string]string{"from": e.From, "to": e.To})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": outNodes, "edges": outEdges, "totalPages": total,
	})
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
//
// The query string is folded into the tag: `?offset=0` and `?offset=1000` are
// different representations of the same revision, and a shared cache that
// reused one tag for the other would serve the wrong page of results.
func notModified(w http.ResponseWriter, r *http.Request, ws store.WorkspaceRow) bool {
	tag := fmt.Sprintf(`W/"%s-%d"`, ws.ID, ws.Revision)
	if q := r.URL.RawQuery; q != "" {
		sum := sha256.Sum256([]byte(q))
		tag = fmt.Sprintf(`W/"%s-%d-%x"`, ws.ID, ws.Revision, sum[:6])
	}
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

// Serve runs the HTTP server until the context is cancelled. certFile and
// keyFile, when both set, serve HTTPS directly rather than expecting a
// terminating proxy in front.
func (s *Server) Serve(ctx context.Context, addr, certFile, keyFile string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		// Full-cycle timeouts so a slow-loris client or a stalled write cannot
		// pin a connection forever. The handler-level chi Timeout fires first
		// for well-behaved requests; these are the transport backstop.
		ReadTimeout: 30 * time.Second,
		// Longer than the rest of the API needs, because the MCP endpoint
		// streams: a Streamable HTTP response stays open while a tool call
		// runs, and a 60-second write deadline would cut it mid-session.
		WriteTimeout: mcpTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		if certFile != "" && keyFile != "" {
			errc <- srv.ListenAndServeTLS(certFile, keyFile)
			return
		}
		errc <- srv.ListenAndServe()
	}()

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
