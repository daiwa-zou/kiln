package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/store"
)

// Deleting a page is a member-level write, alongside corrections and uploads
// rather than alongside deleting a bench.
//
// It is destructive, but bounded and answerable: one page, soft-deleted and so
// recoverable for the retention window, reversible at any time by restoring it,
// and listed until someone does. That is a different kind of power from
// reconfiguring what a bench reads or removing the bench itself, both of which
// stay admin-gated. Someone trusted to add a correction that rewrites a page on
// every future build is already trusted to decide the page should not exist.

// maxDeletePageBytes bounds the delete request body, which carries only a
// short reason.
const maxDeletePageBytes = 4 << 10

// PageDeleter is the page-removal surface; *store.WikiStore implements it.
type PageDeleter interface {
	DeletePage(ctx context.Context, workspaceID, pageRef, reason, deletedBy string) (slug string, err error)
	RestorePage(ctx context.Context, workspaceID, slug string) (restored bool, err error)
	ListDeletedPages(ctx context.Context, workspaceID string) ([]store.DeletedPage, error)
}

// handlePageDelete removes a page and records that it should stay removed.
//
// The response says so explicitly. "Deleted" on its own would be ambiguous here
// in a way it is not elsewhere in the product: everything else a build touches
// comes back on the next run, and someone who has watched that happen has every
// reason to expect this to as well.
func (s *Server) handlePageDelete(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := s.guardWrite(w, r, maxDeletePageBytes)
	if !ok {
		return
	}
	if s.Pages == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "page not found"})
		return
	}
	pageRef := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	// The reason is optional. Making someone justify a deletion to their own
	// wiki is friction with no payoff -- but when they do give one it reaches
	// the agent, which is what turns "do not write this" into "put it
	// somewhere better".
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 && !decodeBody(w, r, &body) {
		return
	}

	slug, err := s.Pages.DeletePage(r.Context(), ws.ID, pageRef, strings.TrimSpace(body.Reason), uid)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "page not found"})
		return
	case err != nil:
		s.fail(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"slug": slug, "status": "deleted",
		// Named so a client can say the thing that matters without knowing how
		// the pipeline works.
		"staysDeleted": true,
	})
}

// handlePageRestore lifts a deletion.
//
// restored distinguishes the two outcomes: the page came back now, or the
// suppression is gone but the soft-deleted row was already swept and only a
// build can produce it again.
func (s *Server) handlePageRestore(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.guardWrite(w, r, maxResolveBytes)
	if !ok {
		return
	}
	if s.Pages == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "page not found"})
		return
	}

	slug := chi.URLParam(r, "slug")
	restored, err := s.Pages.RestorePage(r.Context(), ws.ID, slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no deleted page by that name"})
		return
	case err != nil:
		s.fail(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"slug": slug, "status": "restored", "pageBack": restored,
	})
}

// handleDeletedPagesList shows what a bench has removed.
//
// Worth a surface of its own rather than leaving deletions invisible: a
// deletion that cannot be seen cannot be reconsidered, and the whole point of
// making it durable is that nothing else will surface it later.
func (s *Server) handleDeletedPagesList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if s.Pages == nil {
		writeJSON(w, http.StatusOK, []map[string]any{})
		return
	}
	rows, err := s.Pages.ListDeletedPages(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, map[string]any{
			"slug": d.Slug, "path": d.Path, "title": d.Title,
			"reason":  d.Reason,
			"deleted": d.DeletedAt.UTC().Format(time.RFC3339),
			// recoverable reports whether restoring brings the page straight
			// back, or only allows the next build to write it again.
			"recoverable": d.Path != "",
			"live":        d.Live,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
