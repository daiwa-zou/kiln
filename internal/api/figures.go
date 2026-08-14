package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// The figure surface: the pictures and graphs recovered from source documents.
//
// Read-only and unauthenticated beyond the workspace resolution every other
// read goes through, because a figure is page content -- it is embedded in an
// <img> the browser fetches on its own, without the Authorization header the
// JSON endpoints carry. Anything a reader may read the page for, they may see
// the pictures in.
//
// Bodies keep the reference form `figure:ID` in storage and are rewritten to
// these URLs on the way out. That is what keeps a stored page portable across
// a deployment whose mount point changed.

// FigureStore is the figure surface; *store.WikiStore implements it.
type FigureStore interface {
	Figure(ctx context.Context, workspaceID, id string) (store.FigureRow, error)
	ListFigures(ctx context.Context, workspaceID string) ([]store.FigureRow, error)
}

// figureCacheSeconds is how long a browser may keep a figure.
//
// Long, and safely so: a figure's id is minted per (source, content digest),
// so different bytes are a different id and a stale cache entry cannot show
// the wrong picture. Immutable tells the browser not to revalidate at all,
// which matters for a page carrying a dozen charts.
const figureCacheSeconds = 31536000

// figureURL is where a figure is served from, and what a page's `figure:ID`
// reference resolves to at read time.
func figureURL(workspace, id string) string {
	return "/api/v1/workspaces/" + workspace + "/figures/" + id
}

// resolveFigureBody rewrites a stored body's figure references into URLs a
// browser can fetch. Unknown ids are left in their reference form rather than
// pointed at a URL that would 404: an unrendered reference is visible and
// diagnosable, a broken image is neither.
func resolveFigureBody(body, workspace string, known map[string]bool) string {
	return wiki.ResolveFigures(body, func(id string) string {
		if !known[id] {
			return ""
		}
		return figureURL(workspace, id)
	})
}

// knownFigureIDs is the set of figure ids a workspace can serve.
//
// One list per page read rather than a lookup per reference: a bench has tens
// of figures, not thousands, and this keeps the page handler to a single extra
// query however many pictures the page carries. A failure degrades to "no
// figures resolve", which leaves the page readable.
func (s *Server) knownFigureIDs(ctx context.Context, workspaceID string) map[string]bool {
	if s.Figures == nil {
		return nil
	}
	rows, err := s.Figures.ListFigures(ctx, workspaceID)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("figures could not be listed; page images will not resolve", "err", err)
		}
		return nil
	}
	out := make(map[string]bool, len(rows))
	for _, f := range rows {
		out[f.ID] = true
	}
	return out
}

// handleFigure streams one figure's bytes.
func (s *Server) handleFigure(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if s.Figures == nil || s.Blobs == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "figure not found"})
		return
	}

	f, err := s.Figures.Figure(r.Context(), ws.ID, chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "figure not found"})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}

	body, err := s.Blobs.Get(r.Context(), f.BlobKey)
	if err != nil {
		// The row survived its bytes. A 404 is the honest answer to the
		// browser -- there is nothing to show -- and the log is where an
		// operator finds out storage lost something.
		if s.Log != nil {
			s.Log.Error("figure bytes are missing from storage",
				"figure", f.ID, "key", f.BlobKey, "err", err)
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "figure not found"})
		return
	}
	defer body.Close()

	// Content type comes from the extractor, which decoded the image to
	// establish it; it is never echoed from user input. nosniff stops a
	// browser from second-guessing that and treating a document's image as
	// something executable.
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(figureCacheSeconds)+", immutable")
	if f.SizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(f.SizeBytes, 10))
	}
	if _, err := io.Copy(w, body); err != nil && s.Log != nil {
		// The status line is already sent; nothing to do but record it.
		s.Log.Warn("figure stream ended early", "figure", f.ID, "err", err)
	}
}

// handleFiguresList reports a workspace's figures, for the ingest view and for
// anyone auditing what a document contributed.
func (s *Server) handleFiguresList(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if s.Figures == nil {
		writeJSON(w, http.StatusOK, []map[string]any{})
		return
	}
	rows, err := s.Figures.ListFigures(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, f := range rows {
		out = append(out, map[string]any{
			"id": f.ID, "source": f.SourceKey, "ref": f.Ref,
			"width": f.Width, "height": f.Height, "page": f.Page,
			"caption": f.Caption, "size": f.SizeBytes,
			"contentType": f.ContentType,
			"url":         figureURL(chi.URLParam(r, "workspace"), f.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
