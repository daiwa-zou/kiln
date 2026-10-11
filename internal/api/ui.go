package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
)

// The reading UI, embedded and served from /ui/. Two generations share it
// while views are ported one at a time:
//
//   - the legacy files -- app.js, sources.js, style.css -- hand-written and
//     served as they are, until the last view that needs them is ported;
//   - ui/dist, the Vite build of web/ (TypeScript and Preact). It is
//     committed, so `go build` needs no Node, and CI rebuilds it and fails on
//     any diff. dist/index.html is the shell for both: it loads the bundle as
//     a module script ahead of the legacy scripts, which is how the legacy
//     router finds the views ported so far (window.kiln).
//
// Still deliberately not a server-rendered app: it consumes the same public
// REST API any other client would.
//
//go:embed ui
var uiFS embed.FS

// uiLegacy maps the legacy files to their served paths. Everything under
// ui/dist is enumerated rather than listed, since its names carry hashes.
var uiLegacy = map[string]string{
	"/ui/style.css":  "ui/style.css",
	"/ui/app.js":     "ui/app.js",
	"/ui/sources.js": "ui/sources.js",
}

// uiTypes is the allowlist of what the UI may serve, by extension. A build
// that emits anything else (an image Vite decided to copy, a sourcemap)
// panics at init instead of being served with a guessed type; extend this
// list deliberately. compress is false for formats that are compressed
// already.
var uiTypes = map[string]struct {
	contentType string
	compress    bool
}{
	".html":  {"text/html; charset=utf-8", true},
	".js":    {"text/javascript; charset=utf-8", true},
	".css":   {"text/css; charset=utf-8", true},
	".svg":   {"image/svg+xml", true},
	".woff2": {"font/woff2", false},
}

// uiName is every character a served path may contain. Vite's names are
// [name]-[hash] from source file names; anything outside this set would need
// escaping in index.html and is more likely a mistake than a file.
var uiName = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)

// uiAssets maps each served path to its file, content type, and ETag,
// computed once at init so cache validators can never drift from content.
var uiAssets = buildUIAssets(uiFS)

func buildUIAssets(fsys fs.FS) map[string]*uiAsset {
	out := map[string]*uiAsset{}
	add := func(served, file string) {
		if !uiName.MatchString(served) || strings.Contains(served, "..") {
			panic("ui asset has a name outside [A-Za-z0-9._/-]: " + served)
		}
		// A dist file shadowing a legacy one (a Vite input named style.css
		// with hashing off, say) would silently replace it.
		if _, dup := out[served]; dup {
			panic("two ui assets claim " + served)
		}
		t, ok := uiTypes[path.Ext(file)]
		if !ok {
			panic("ui asset with no allowlisted content type: " + file)
		}
		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			panic("ui asset missing from embed: " + file) // impossible after go build
		}
		cache := "no-cache"
		if strings.HasPrefix(served, "/ui/assets/") {
			cache = "public, max-age=31536000, immutable"
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(body))
		out[served] = &uiAsset{
			body:         body,
			contentType:  t.contentType,
			etag:         `"` + sum + `"`,
			gzipETag:     `"` + sum + `-gz"`,
			cacheControl: cache,
			compress:     t.compress,
		}
	}

	for served, file := range uiLegacy {
		add(served, file)
	}
	err := fs.WalkDir(fsys, "ui/dist", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "ui/dist/")
		if rel == "index.html" {
			add("/", p)
		} else {
			add("/ui/"+rel, p)
		}
		return nil
	})
	if err != nil {
		panic("ui/dist missing from embed: " + err.Error())
	}
	if out["/"] == nil {
		panic("ui/dist/index.html missing from embed; run `make ui`")
	}
	return out
}

type uiAsset struct {
	body         []byte
	contentType  string
	etag         string
	gzipETag     string
	cacheControl string
	compress     bool

	gzOnce sync.Once
	gz     []byte
}

// gzipped compresses on first use and keeps the result. Lazily, so a process
// that never serves the UI (a worker) pays nothing at init; deterministically
// (no name, zero mtime), so every replica returns the same bytes for the same
// ETag.
func (a *uiAsset) gzipped() []byte {
	a.gzOnce.Do(func() {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // only fails on a bad level
		_, _ = zw.Write(a.body)
		_ = zw.Close()
		a.gz = buf.Bytes()
	})
	return a.gz
}

// uiCSP locks the page to its own served assets. The UI renders
// machine-generated markdown into innerHTML and holds a bearer token in
// localStorage, so the policy is the backstop for any future regression in
// its escaping: injected inline script or a foreign fetch target simply does
// not execute. With no inline blocks left, 'self' covers exactly the files
// this binary serves.
//
// img-src is 'self' and data: only, deliberately NOT https:. Page markdown is
// written by a model from arbitrary sources, so an https: image there is a
// request every reader's browser makes on render, to a host and with a query
// string the author chose -- a zero-click way to carry data out of the wiki.
// Nothing in the UI needs a foreign image: figures recovered from documents
// are served from /api/v1/ (same origin), the avatar is initials or an inline
// SVG glyph, and the favicon is a data: URI. The renderer turns off-origin markdown
// images into links; this is the backstop if it ever stops doing so.
//
// font-src 'self' is there for the Vite build: it never inlines an asset
// (assetsInlineLimit 0, so no data: URI needs allowing), which means a font
// a component imports is a hashed file under /ui/assets/, and default-src
// 'none' would otherwise refuse it. Same origin only, like everything else.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// acceptsGzip reads Accept-Encoding the way RFC 9110 12.5.3 says to: a coding
// with q=0 is refused, and "*" stands for any coding not named. Substring
// matching would serve gzip to "gzip;q=0", which is a client saying no.
func acceptsGzip(header string) bool {
	star := false
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err != nil {
					f = 0 // an unreadable weight is not a yes
				}
				q = f
			}
		}
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "gzip", "x-gzip":
			return q > 0 // named explicitly: that decides it
		case "*":
			star = q > 0
		}
	}
	return star
}

// etagMatches reports whether If-None-Match names etag. The header is a list,
// compared weakly (RFC 9110 13.1.2): W/"x" matches "x", and "*" matches any
// current representation.
func etagMatches(header, etag string) bool {
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag == "*" || tag == etag {
			return true
		}
	}
	return false
}

func mountUI(r chi.Router) { mountUIAssets(r, uiAssets) }

func mountUIAssets(r chi.Router, assets map[string]*uiAsset) {
	serve := func(asset *uiAsset) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			h := w.Header()
			h.Set("Content-Type", asset.contentType)
			// Legacy files and the shell are revalidated on every load so a
			// redeploy is never served stale; the ETag turns that into a 304.
			// Hashed build output never changes under its name.
			h.Set("Cache-Control", asset.cacheControl)
			h.Set("Content-Security-Policy", uiCSP)
			h.Set("X-Content-Type-Options", "nosniff")
			// Links out of the wiki (off-origin images render as links, not
			// loads) should not tell the destination which page they came from.
			h.Set("Referrer-Policy", "no-referrer")

			body, etag := asset.body, asset.etag
			if asset.compress {
				// On every compressible response, 304s included: a cache
				// keyed on the URL alone would hand gzip to a client that
				// never asked for it.
				h.Set("Vary", "Accept-Encoding")
				if acceptsGzip(req.Header.Get("Accept-Encoding")) {
					body, etag = asset.gzipped(), asset.gzipETag
					h.Set("Content-Encoding", "gzip")
				}
			}
			h.Set("ETag", etag)
			if etagMatches(req.Header.Get("If-None-Match"), etag) {
				h.Del("Content-Encoding")
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write(body)
		}
	}

	index := serve(assets["/"])
	handlers := make(map[string]http.HandlerFunc, len(assets))
	for p, asset := range assets {
		handlers[p] = serve(asset)
	}
	r.Get("/", index)
	// One handler for every asset, looked up by exact path: no file system
	// is ever touched with a request path. Nothing but assets lives under
	// /ui/, so a miss is a 404 rather than the app shell -- a stale hashed
	// name answered with HTML would be a module script that fails to parse,
	// and no-store keeps that answer out of every cache.
	r.Get("/ui/*", func(w http.ResponseWriter, req *http.Request) {
		// Only the canonical spelling of a name is served. A percent-encoded
		// one (/ui/%61pp.js, /ui/assets%2F...) decodes to the same key but is
		// a different URL to every cache in between, which could then hold
		// a year-long immutable copy under a name nothing links to.
		if req.URL.RawPath == "" {
			if h, ok := handlers[req.URL.Path]; ok {
				h(w, req)
				return
			}
			// The shell used to answer anything under /ui/; the two spellings
			// of "the UI's index" someone may have bookmarked go home.
			if req.URL.Path == "/ui/" || req.URL.Path == "/ui/index.html" {
				w.Header().Set("X-Content-Type-Options", "nosniff")
				http.Redirect(w, req, "/", http.StatusPermanentRedirect)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.Error(w, "not found", http.StatusNotFound)
	})
	// Any other non-API path renders the app shell; hash routes survive
	// reloads at "/" on their own, and an unknown plain path lands on the
	// overview.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		index(w, req)
	})
}
