package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
)

// These run on the embedded files alone: no Node, no database. Passing them
// is what "go build without Node gives a working UI" means -- the committed
// build is complete, every file it names is served, and it stays inside the
// CSP.

func uiRouter(assets map[string]*uiAsset) http.Handler {
	r := chi.NewRouter()
	mountUIAssets(r, assets)
	return r
}

func uiGet(t *testing.T, h http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func distIndex(t *testing.T) string {
	t.Helper()
	b, err := uiFS.ReadFile("ui/dist/index.html")
	if err != nil {
		t.Fatalf("ui/dist/index.html is not embedded (run `make ui`): %v", err)
	}
	return string(b)
}

func TestUIDistReferencesResolve(t *testing.T) {
	h := uiRouter(uiAssets)
	index := distIndex(t)

	refs := regexp.MustCompile(`(?:src|href)="(/ui/[^"]*)"`).FindAllStringSubmatch(index, -1)
	if len(refs) < 4 {
		t.Fatalf("found %d /ui/ references in dist/index.html; expected the bundle and the three legacy files", len(refs))
	}
	for _, m := range refs {
		rec := uiGet(t, h, m[1], nil)
		if rec.Code != http.StatusOK {
			t.Errorf("dist/index.html references %s, which answers %d", m[1], rec.Code)
		}
	}

	// The bundle is a module script (deferred, and ahead of the legacy defer
	// scripts in document order, so window.kiln exists before app.js boots).
	module := regexp.MustCompile(`<script type="module"[^>]* src="(/ui/assets/[^"]+\.js)"`).FindStringSubmatch(index)
	if module == nil {
		t.Fatal("dist/index.html has no module script from /ui/assets/")
	}
	legacy := strings.Index(index, `src="/ui/app.js"`)
	if legacy < 0 || strings.Index(index, module[1]) > legacy {
		t.Error("the module script must come before the legacy app.js")
	}
}

func TestUIDistHasNoInlineCode(t *testing.T) {
	index := distIndex(t)
	for _, m := range regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(index, -1) {
		if strings.TrimSpace(m[2]) != "" {
			t.Errorf("inline script body in dist/index.html: %.60q", m[2])
		}
		if !strings.Contains(m[1], "src=") {
			t.Errorf("script tag without src in dist/index.html: %q", m[0])
		}
	}
	if loc := regexp.MustCompile(`\sstyle\s*=`).FindStringIndex(index); loc != nil {
		t.Errorf("inline style attribute in dist/index.html: %q", index[loc[0]:min(loc[1]+30, len(index))])
	}
	if loc := regexp.MustCompile(`(?i)\son[a-z]+\s*=`).FindStringIndex(index); loc != nil {
		t.Errorf("inline event handler in dist/index.html: %q", index[loc[0]:min(loc[1]+30, len(index))])
	}
	if strings.Contains(index, "<style") {
		t.Error("inline <style> block in dist/index.html")
	}
}

// Go's embed silently skips files whose names start with "." or "_". A
// bundler chunk named that way would be missing from the binary with nothing
// failing until a browser asked for it.
func TestUIDistIsFullyEmbedded(t *testing.T) {
	var onDisk int
	err := filepath.WalkDir(filepath.Join("ui", "dist"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		onDisk++
		if _, err := uiFS.ReadFile(filepath.ToSlash(p)); err != nil {
			t.Errorf("%s is on disk but not embedded; rename it (embed skips . and _ prefixes)", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if onDisk == 0 {
		t.Fatal("ui/dist is empty; run `make ui`")
	}
}

// Hashed names are what make "immutable" true. An unhashed file under
// assets/ would be cached for a year and never replaced.
func TestUIAssetsCarryAHash(t *testing.T) {
	hashed := regexp.MustCompile(`^/ui/assets/[A-Za-z0-9._-]+-[A-Za-z0-9_-]{8,}\.[a-z0-9]+$`)
	var n int
	for p := range uiAssets {
		if !strings.HasPrefix(p, "/ui/assets/") {
			continue
		}
		n++
		if !hashed.MatchString(p) {
			t.Errorf("%s is under /ui/assets/ (served immutable) without a content hash in its name", p)
		}
	}
	if n == 0 {
		t.Error("no hashed assets embedded")
	}
}

func TestUIHeaders(t *testing.T) {
	h := uiRouter(uiAssets)
	for p, asset := range uiAssets {
		rec := uiGet(t, h, p, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d", p, rec.Code)
			continue
		}
		hdr := rec.Header()
		if got := hdr.Get("Content-Security-Policy"); got != uiCSP {
			t.Errorf("%s: CSP = %q", p, got)
		}
		if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", p, got)
		}
		if got := hdr.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy = %q", p, got)
		}
		if got := hdr.Get("Content-Type"); got != asset.contentType {
			t.Errorf("%s: Content-Type = %q", p, got)
		}
		wantCache := "no-cache"
		if strings.HasPrefix(p, "/ui/assets/") {
			wantCache = "public, max-age=31536000, immutable"
		}
		if got := hdr.Get("Cache-Control"); got != wantCache {
			t.Errorf("%s: Cache-Control = %q, want %q", p, got, wantCache)
		}
		if asset.compress && hdr.Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: compressible but Vary = %q", p, hdr.Get("Vary"))
		}
		if hdr.Get("Content-Encoding") != "" {
			t.Errorf("%s: Content-Encoding %q without Accept-Encoding", p, hdr.Get("Content-Encoding"))
		}
		if !bytes.Equal(rec.Body.Bytes(), asset.body) {
			t.Errorf("%s: body differs from the embedded file", p)
		}
	}
}

// The whole policy, spelled out: a change to it is a security review.
// font-src is the one directive the Vite build added (T5 in the M11 plan).
func TestUICSPIsExact(t *testing.T) {
	const want = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if uiCSP != want {
		t.Errorf("uiCSP = %q\nwant   %q", uiCSP, want)
	}
}

func TestUIGzipRoundTrips(t *testing.T) {
	h := uiRouter(uiAssets)
	for p, asset := range uiAssets {
		if !asset.compress {
			continue
		}
		rec := uiGet(t, h, p, map[string]string{"Accept-Encoding": "br, gzip"})
		if rec.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("%s: Content-Encoding = %q, want gzip", p, rec.Header().Get("Content-Encoding"))
			continue
		}
		if got := rec.Header().Get("ETag"); got != asset.gzipETag || !strings.HasSuffix(got, `-gz"`) {
			t.Errorf("%s: gzip ETag = %q", p, got)
		}
		if rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: gzip response without Vary", p)
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !bytes.Equal(plain, asset.body) {
			t.Errorf("%s: gunzipped body differs from the file", p)
		}
	}
	// Deterministic: the same bytes for the same ETag on every replica.
	a := &uiAsset{body: []byte(strings.Repeat("kiln ", 100))}
	b := &uiAsset{body: []byte(strings.Repeat("kiln ", 100))}
	if !bytes.Equal(a.gzipped(), b.gzipped()) {
		t.Error("gzip output is not deterministic")
	}
}

func TestUIContentNegotiation(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"GZIP", true},
		{"x-gzip", true},
		{"deflate, gzip", true},
		{"gzip;q=0.5", true},
		{"gzip; q=0.001", true},
		{"*", true},
		{"br, *;q=0.1", true},
		{"", false},
		{"identity", false},
		{"br, deflate", false},
		{"gzip;q=0", false},
		{"gzip; q=0.0", false},
		{"gzip;q=0, *", false},
		{"*, gzip;q=0", false},
		{"*;q=0", false},
		{"gzip;q=bogus", false},
		{"gzipped", false},
	} {
		if got := acceptsGzip(tc.header); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}

	// End to end: refusing gzip gets identity, still with Vary.
	h := uiRouter(uiAssets)
	rec := uiGet(t, h, "/ui/app.js", map[string]string{"Accept-Encoding": "gzip;q=0"})
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("ETag") != uiAssets["/ui/app.js"].etag {
		t.Errorf("gzip;q=0 got Content-Encoding %q, ETag %q", rec.Header().Get("Content-Encoding"), rec.Header().Get("ETag"))
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Error("identity response for a compressible asset lacks Vary")
	}
}

// Formats that are compressed already never get Content-Encoding, and an
// extension outside the allowlist cannot be embedded at all.
func TestUIIncompressibleTypesAreNotGzipped(t *testing.T) {
	assets := buildUIAssets(fstest.MapFS{
		"ui/style.css":                        {Data: []byte("a{}")},
		"ui/app.js":                           {Data: []byte("boot();")},
		"ui/sources.js":                       {Data: []byte("")},
		"ui/dist/index.html":                  {Data: []byte("<!doctype html>")},
		"ui/dist/assets/serif-AbCdEf12.woff2": {Data: []byte("wOF2 not really")},
	})
	rec := uiGet(t, uiRouter(assets), "/ui/assets/serif-AbCdEf12.woff2", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "font/woff2" {
		t.Fatalf("woff2 = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Vary") != "" {
		t.Errorf("woff2 got Content-Encoding %q, Vary %q", rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	}
}

func TestUIConditionalRequests(t *testing.T) {
	h := uiRouter(uiAssets)
	asset := uiAssets["/ui/app.js"]
	gz := map[string]string{"Accept-Encoding": "gzip"}
	for _, tc := range []struct {
		name   string
		header map[string]string
		inm    string
		want   int
	}{
		{"identity etag", nil, asset.etag, http.StatusNotModified},
		{"gzip etag", gz, asset.gzipETag, http.StatusNotModified},
		{"weak gzip etag", gz, "W/" + asset.gzipETag, http.StatusNotModified},
		{"etag in a list", nil, `"nope", ` + asset.etag + `, "other"`, http.StatusNotModified},
		{"star", nil, "*", http.StatusNotModified},
		// A validator for the other representation is not this one.
		{"identity etag, gzip accepted", gz, asset.etag, http.StatusOK},
		{"gzip etag, identity", nil, asset.gzipETag, http.StatusOK},
		{"stale etag", nil, `"0000"`, http.StatusOK},
	} {
		hdr := map[string]string{"If-None-Match": tc.inm}
		for k, v := range tc.header {
			hdr[k] = v
		}
		rec := uiGet(t, h, "/ui/app.js", hdr)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
		if rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: Vary missing on a %d", tc.name, rec.Code)
		}
		if rec.Code == http.StatusNotModified && rec.Body.Len() != 0 {
			t.Errorf("%s: 304 with a body", tc.name)
		}
	}
}

func TestUIUnknownAssetsAre404(t *testing.T) {
	h := uiRouter(uiAssets)
	js := ""
	for p := range uiAssets {
		if strings.HasPrefix(p, "/ui/assets/") {
			js = strings.TrimPrefix(p, "/ui/assets/")
		}
	}
	for _, p := range []string{
		"/ui/assets/index-00000000.js", "/ui/assets/", "/ui/nope.js", "/ui/dist/index.html", "/ui/../ui/app.js",
		// Percent-encoded aliases of real entries.
		"/ui/%61pp.js", "/ui/assets%2F" + js, "/ui/assets/%69" + js[1:],
	} {
		rec := uiGet(t, h, p, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", p, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: nosniff missing", p)
		}
		if strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("%s: a miss under /ui/ answered with the app shell", p)
		}
	}
	// The UI's own index spellings, which used to render the shell, go home.
	for _, p := range []string{"/ui/", "/ui/index.html"} {
		rec := uiGet(t, h, p, nil)
		if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "/" {
			t.Errorf("%s = %d to %q, want 308 to /", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Outside /ui/, a plain path is still a deep link into the app.
	if rec := uiGet(t, h, "/some/deep/link", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("deep link = %d", rec.Code)
	}
}

func TestBuildUIAssetsRefusesBadTrees(t *testing.T) {
	base := func() fstest.MapFS {
		return fstest.MapFS{
			"ui/style.css":       {Data: []byte("a{}")},
			"ui/app.js":          {Data: []byte("boot();")},
			"ui/sources.js":      {Data: []byte("")},
			"ui/dist/index.html": {Data: []byte("<!doctype html>")},
		}
	}
	for _, tc := range []struct {
		name  string
		file  string
		panic string
	}{
		{"dist shadows a legacy file", "ui/dist/style.css", "two ui assets claim /ui/style.css"},
		{"name outside the allowed set", "ui/dist/assets/a b-12345678.js", "outside [A-Za-z0-9._/-]"},
		{"non-ascii name", "ui/dist/assets/é-12345678.js", "outside [A-Za-z0-9._/-]"},
		{"extension not allowlisted", "ui/dist/assets/logo-12345678.png", "no allowlisted content type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := base()
			fsys[tc.file] = &fstest.MapFile{Data: []byte("x")}
			defer func() {
				r := recover()
				if s, _ := r.(string); !strings.Contains(s, tc.panic) {
					t.Errorf("panic = %v, want one containing %q", r, tc.panic)
				}
			}()
			buildUIAssets(fsys)
		})
	}

	t.Run("no index.html", func(t *testing.T) {
		fsys := base()
		delete(fsys, "ui/dist/index.html")
		fsys["ui/dist/assets/x-12345678.js"] = &fstest.MapFile{Data: []byte("x")}
		defer func() {
			if s, _ := recover().(string); !strings.Contains(s, "index.html missing") {
				t.Errorf("panic = %q", s)
			}
		}()
		buildUIAssets(fsys)
	})
}

// make ui, npm (engine-strict) and CI's setup-node all read the Node version
// from these two places; if they disagree, one of them builds with a Node the
// other refuses.
func TestUINodeVersionPinsAgree(t *testing.T) {
	nvmrc, err := os.ReadFile(filepath.Join("..", "..", ".nvmrc"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Engines struct{ Node string } `json:"engines"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(nvmrc)); pkg.Engines.Node != want {
		t.Errorf("web/package.json engines.node = %q, .nvmrc = %q", pkg.Engines.Node, want)
	}
}
