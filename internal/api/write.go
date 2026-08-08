package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/store"
)

// This file is the write surface: steering documents, page corrections, and
// review resolution. It is deliberately small -- the wiki's content is never
// written here (pages come only from the pipeline); what humans write is
// guidance about the content.

// Request body ceilings. Corrections and steering text are injected verbatim
// into every future generation prompt, so their size is a cost and
// prompt-hygiene boundary, not just a transport nicety.
const (
	maxSteeringBytes   = 64 << 10
	maxCorrectionBytes = 16 << 10
	maxResolveBytes    = 4 << 10
)

// canWrite decides whether the caller may mutate this workspace. Admin tokens
// and auth-disabled deployments may; otherwise any org role except "viewer"
// may. "member" -- the historical default -- writes, because locking existing
// deployments out of the feature they upgraded for is worse than a default-
// permissive role no one has assigned restrictively yet.
func (s *Server) canWrite(r *http.Request, ws store.WorkspaceRow) (userID string, ok bool, err error) {
	if s.Auth == nil {
		return "", true, nil
	}
	id, found := auth.FromContext(r.Context())
	if !found {
		return "", false, nil
	}
	if id.Admin {
		return id.UserID, true, nil
	}
	role, err := s.Writes.WorkspaceRole(r.Context(), ws.ID, id.UserID)
	if err != nil {
		return "", false, err
	}
	return id.UserID, role != "" && role != "viewer", nil
}

// guardWrite wraps the shared preamble of every write handler: resolve the
// workspace, check the role, and bound the body. Returns ok=false after
// having written the response.
func (s *Server) guardWrite(w http.ResponseWriter, r *http.Request, maxBytes int64) (store.WorkspaceRow, string, bool) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return store.WorkspaceRow{}, "", false
	}
	uid, allowed, err := s.canWrite(r, ws)
	if err != nil {
		s.fail(w, err)
		return store.WorkspaceRow{}, "", false
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "workspace role does not permit writes"})
		return store.WorkspaceRow{}, "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return ws, uid, true
}

// decodeBody parses a JSON body into dst, translating oversize and malformed
// bodies into a 400 the caller can act on. Returns false after writing the
// response.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return false
	}
	return true
}

// handleSteeringPut upserts a steering document. Steering is the main lever
// for changing a wiki's character without touching code: it is injected into
// every prompt from the next run onward.
func (s *Server) handleSteeringPut(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := s.guardWrite(w, r, maxSteeringBytes)
	if !ok {
		return
	}
	kind := chi.URLParam(r, "kind")
	if !slices.Contains(store.SteeringKinds, kind) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown steering kind"})
		return
	}

	var body struct {
		Body string `json:"body"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	if err := s.Writes.UpsertSteeringDoc(r.Context(), ws.ID, kind, body.Body, uid); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"kind": kind, "status": "saved"})
}

// handleSteeringGet returns both steering documents, so the UI can render the
// editing form without a second round trip.
func (s *Server) handleSteeringGet(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	docs, err := s.Writes.LoadSteeringDocs(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

// handleCorrectionCreate pins a correction to a page. Pages are never
// hand-edited -- the pipeline would clobber the edit -- so a correction lives
// beside the page and is re-injected into every future prompt for it.
func (s *Server) handleCorrectionCreate(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := s.guardWrite(w, r, maxCorrectionBytes)
	if !ok {
		return
	}
	pageRef := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	var body struct {
		Body string `json:"body"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "correction body is empty"})
		return
	}

	id, err := s.Writes.CreateCorrection(r.Context(), ws.ID, pageRef, body.Body, uid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "page not found"})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "status": "pinned"})
}

// handleCorrectionsList returns a page's corrections, so the UI can show what
// guidance is already pinned before someone adds more.
func (s *Server) handleCorrectionsList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	pageRef := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	rows, err := s.Writes.ListCorrections(r.Context(), ws.ID, pageRef)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"id": c.ID, "body": c.Body, "active": c.Active, "created": c.Created,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCorrectionPatch toggles a correction's active flag. Deactivation, not
// deletion: the record of what a human once said about a page is worth keeping.
func (s *Server) handleCorrectionPatch(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.guardWrite(w, r, maxResolveBytes)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	var body struct {
		Active *bool `json:"active"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Active == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "active is required"})
		return
	}

	err := s.Writes.SetCorrectionActive(r.Context(), ws.ID, id, *body.Active)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "correction not found"})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "active": *body.Active})
}

// handleReviews lists the review queue: the wiki's questions for its humans.
// Status defaults to open -- the inbox -- and "" lists everything.
func (s *Server) handleReviews(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	status := "open"
	if q := r.URL.Query(); q.Has("status") {
		status = q.Get("status")
	}
	limit, offset := pagination(r, defaultGapLimit, maxPageLimit)

	rows, err := s.Writes.ListReviews(r.Context(), ws.ID, status, limit, offset)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, rv := range rows {
		out = append(out, map[string]any{
			"id": rv.ID, "kind": rv.Kind, "title": rv.Title, "detail": rv.Detail,
			"actions": rv.Actions, "status": rv.Status, "pageSlug": rv.PageSlug,
			"created": rv.CreatedAt, "resolved": rv.ResolvedAt,
			// Whether the item can be handed to a worker is decided here rather
			// than in the client: the kinds reading can settle are a property of
			// the queue, and a button the server would refuse is worse than no
			// button at all.
			"researchable": s.Runs != nil && rv.Status == "open" &&
				slices.Contains(store.ResearchableKinds, rv.Kind),
			"researching": rv.Researching,
			"research":    rv.Research,
			"researched":  rv.ResearchAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleReviewResolve closes an open review with the human's chosen action.
// "approve" on a deletion review is what authorizes the next run's cascade;
// any other action simply resolves the question.
func (s *Server) handleReviewResolve(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := s.guardWrite(w, r, maxResolveBytes)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	var body struct {
		Action string `json:"action"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Action) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "action is required"})
		return
	}

	err := s.Writes.ResolveReview(r.Context(), ws.ID, id, body.Action, uid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review not found or already resolved"})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "action": body.Action})
}

// handleReviewResearch hands a review item back to the worker: instead of a
// human answering the question from scratch, the agent that raised it re-reads
// the bench's sources with that one question in hand and attaches what it
// finds. The item stays in the queue either way -- research is evidence, not a
// decision -- so this resolves nothing.
//
// 202 with the run id: the answer arrives on the card when the worker gets to
// it, the same way a rebuild's pages do.
func (s *Server) handleReviewResearch(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.guardWrite(w, r, maxResolveBytes)
	if !ok {
		return
	}

	// Enforced here for the same reason a rebuild enforces it: this is the
	// last moment refusing costs nothing.
	if refused, err := s.refuseOverBudget(r.Context(), ws); err != nil {
		s.fail(w, err)
		return
	} else if refused != "" {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": refused})
		return
	}

	id := chi.URLParam(r, "id")
	runID, err := s.Writes.RequestResearch(r.Context(), ws.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "review not found or no longer open"})
		return
	case errors.Is(err, store.ErrNotResearchable):
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this review asks for a decision, not for reading; only " +
				strings.Join(store.ResearchableKinds, ", ") + " items can be researched"})
		return
	case errors.Is(err, store.ErrRunActive):
		// Not a failure: the bench runs one job at a time and the slot is
		// taken -- by a build, or by research already queued for this very
		// item. Saying so beats filing a request that would silently collapse
		// onto a run which is not going to answer anything.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this bench already has a queued or running job; try again when it finishes"})
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "runId": runID})
}

// handleBacklinks lists pages linking to a slug -- the inbound half of the
// graph the store has materialized since the first import.
func (s *Server) handleBacklinks(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok || notModified(w, r, ws) {
		return
	}
	slug := chi.URLParam(r, "slug")

	rows, err := s.Writes.Backlinks(r.Context(), ws.ID, slug)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]PageSummary, 0, len(rows))
	for _, p := range rows {
		out = append(out, PageSummary{Path: p.Path, Slug: p.Slug, Type: p.Type, Title: p.Title})
	}
	writeJSON(w, http.StatusOK, out)
}
