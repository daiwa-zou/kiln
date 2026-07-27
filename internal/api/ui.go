package api

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// The reading UI: one HTML shell plus one stylesheet and one script, embedded
// and served from /ui/. Split from a single inline file when the graph view
// pushed it past two thousand lines (the roadmap's revisit trigger); still
// deliberately not a framework app — it consumes the same public REST API a
// future frontend would, so it remains a stand-in rather than a detour.
//
//go:embed ui
var uiFS embed.FS

// uiAssets maps each served path to its file, content type, and ETag,
// computed once at init so cache validators can never drift from content.
var uiAssets = func() map[string]*uiAsset {
	out := map[string]*uiAsset{}
	for path, meta := range map[string]struct{ file, contentType string }{
		"/":             {"ui/index.html", "text/html; charset=utf-8"},
		"/ui/style.css": {"ui/style.css", "text/css; charset=utf-8"},
		"/ui/app.js":    {"ui/app.js", "text/javascript; charset=utf-8"},
	} {
		body, err := uiFS.ReadFile(meta.file)
		if err != nil {
			panic("ui asset missing from embed: " + meta.file) // impossible after go build
		}
		out[path] = &uiAsset{
			body:        body,
			contentType: meta.contentType,
			etag:        fmt.Sprintf(`"%x"`, sha256.Sum256(body)),
		}
	}
	return out
}()

type uiAsset struct {
	body        []byte
	contentType string
	etag        string
}

// uiCSP locks the page to its own served assets. The UI renders
// machine-generated markdown into innerHTML and holds a bearer token in
// localStorage, so the policy is the backstop for any future regression in
// its escaping: injected inline script or a foreign fetch target simply does
// not execute. With no inline blocks left, 'self' covers exactly the two
// files this binary serves.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' https: data:; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func mountUI(r chi.Router) {
	serve := func(asset *uiAsset) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			h := w.Header()
			h.Set("Content-Type", asset.contentType)
			// Revalidated on every load so a redeploy is never served stale;
			// the ETag turns that revalidation into a 304.
			h.Set("Cache-Control", "no-cache")
			h.Set("ETag", asset.etag)
			h.Set("Content-Security-Policy", uiCSP)
			h.Set("X-Content-Type-Options", "nosniff")
			// Wiki content may embed external images; the reader's URL is not
			// the image host's business.
			h.Set("Referrer-Policy", "no-referrer")
			if req.Header.Get("If-None-Match") == asset.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write(asset.body)
		}
	}

	for path, asset := range uiAssets {
		r.Get(path, serve(asset))
	}
	index := serve(uiAssets["/"])
	// Any non-API path renders the app shell; hash routes survive reloads at
	// "/" on their own, and an unknown plain path lands on the overview.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		index(w, req)
	})
}
