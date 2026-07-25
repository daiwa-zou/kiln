package api

import (
	_ "embed"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// indexHTML is the minimal reading UI.
//
// Embedded as a single file rather than scaffolded as a framework app: M0 needs
// a way to read what kiln produced, and the Next.js frontend the plan describes
// is its own deployable with its own build. This consumes the same public REST
// API that frontend will, so it is a stand-in rather than a detour.
//
//go:embed ui.html
var indexHTML []byte

func mountUI(r chi.Router) {
	serve := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The UI is fetched fresh so a redeploy is never served from cache
		// while the API behind it has moved on.
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(indexHTML)
	}

	r.Get("/", serve)
	// Any non-API path renders the app, so deep links to a page work on reload.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		serve(w, req)
	})
}
