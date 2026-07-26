package api

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
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

// uiETag makes reloads free: Cache-Control is no-cache (revalidate every
// time, so a redeploy is picked up immediately), and the hash answers that
// revalidation with a 304 instead of the whole document.
var uiETag = fmt.Sprintf(`"%x"`, sha256.Sum256(indexHTML))

// uiCSP locks the page to its own two inline blocks. The UI renders
// machine-generated markdown into innerHTML and holds a bearer token in
// localStorage, so the policy is the backstop for any future regression in
// its escaping: injected inline script or a foreign fetch target simply does
// not execute. Hashes are computed from the embedded file at init, so the
// policy can never drift from the markup it guards.
var uiCSP = fmt.Sprintf(
	"default-src 'none'; script-src '%s'; style-src '%s'; "+
		"img-src 'self' https: data:; connect-src 'self'; "+
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
	inlineHash("<script>", "</script>"),
	inlineHash("<style>", "</style>"),
)

// inlineHash returns the CSP sha256 source for the single block between the
// given tags. The UI is one file with exactly one script and one style block;
// a second block would silently fail CSP, which is the desired failure mode
// for markup this policy has never seen.
func inlineHash(open, close string) string {
	start := bytes.Index(indexHTML, []byte(open))
	end := bytes.Index(indexHTML, []byte(close))
	if start < 0 || end < 0 || end <= start {
		// Unreachable with the embedded file; an empty hash blocks all inline
		// code, failing closed rather than open.
		return "sha256-invalid"
	}
	block := indexHTML[start+len(open) : end]
	sum := sha256.Sum256(block)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func mountUI(r chi.Router) {
	serve := func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		// The UI is revalidated on every load so a redeploy is never served
		// stale; the ETag turns that revalidation into a 304.
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", uiETag)
		h.Set("Content-Security-Policy", uiCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		// Wiki content may embed external images; the reader's URL is not
		// the image host's business.
		h.Set("Referrer-Policy", "no-referrer")
		if req.Header.Get("If-None-Match") == uiETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(indexHTML)
	}

	r.Get("/", serve)
	// Any non-API path renders the app shell; hash routes survive reloads at
	// "/" on their own, and an unknown plain path lands on the overview.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		serve(w, req)
	})
}
