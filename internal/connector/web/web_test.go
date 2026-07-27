package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/extract"
)

// htmlStub extracts HTML by stripping tags, standing in for pandoc so the
// suite stays hermetic on machines (and CI) without it. Headings become
// markdown headings, matching what pandoc would emit closely enough.
type htmlStub struct{}

func (htmlStub) Handles(f extract.Format) bool { return f == extract.FormatHTML }
func (htmlStub) Available() bool               { return true }
func (htmlStub) Tool() string                  { return "html-stub" }
func (htmlStub) Extract(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := regexp.MustCompile(`<h1[^>]*>([^<]*)</h1>`).ReplaceAllString(string(b), "# $1\n")
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, " ")
	return strings.TrimSpace(s), nil
}

// testExtractors is the default set plus the stub fallback for HTML.
func testExtractors() extract.Extractors {
	return append(extract.DefaultExtractors(), htmlStub{})
}

func TestValidateURLPolicy(t *testing.T) {
	reject := map[string]string{
		"http":        "http://example.com/page",
		"ftp":         "ftp://example.com/x",
		"credentials": "https://user:pass@example.com/page",
		"no-host":     "https:///page",
		"garbage":     "://",
	}
	for name, raw := range reject {
		if _, err := ValidateURL(raw); err == nil {
			t.Errorf("%s: ValidateURL(%q) accepted", name, raw)
		}
	}
	if _, err := ValidateURL("https://example.com/docs/guide"); err != nil {
		t.Errorf("public https rejected: %v", err)
	}
}

func TestSyncRefusesPrivateAddresses(t *testing.T) {
	// A loopback httptest server with the guard ON: the pinned dialer must
	// refuse to connect, which is the whole SSRF defense in one assertion.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>secret internal page</body></html>"))
	}))
	defer srv.Close()

	// httptest is http:// anyway, so force the URL shape past ValidateURL by
	// testing the dialer directly through fetchOne via a https loopback URL.
	c := &Connector{} // guard on
	_, err := c.Sync(context.Background(), connector.Config{
		"urls": []any{"https://127.0.0.1:9/page"},
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("loopback fetch = %v, want the non-public refusal", err)
	}
}

func TestSyncFetchesExtractsAndStages(t *testing.T) {
	pages := map[string]string{
		"/guide": `<html><head><title>x</title></head><body>
			<h1>The Guide</h1><p>Prose about the system.</p></body></html>`,
		"/notes.md": "# Notes\n\nplain markdown body\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("fetch carried credentials")
		}
		if strings.HasSuffix(r.URL.Path, ".md") {
			w.Header().Set("Content-Type", "text/markdown")
		} else {
			w.Header().Set("Content-Type", "text/html")
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &Connector{allowPrivate: true, Extractors: testExtractors()} // tests must reach loopback
	staging := t.TempDir()
	set, err := c.Sync(context.Background(), connector.Config{
		"urls": []any{srv.URL + "/guide", srv.URL + "/notes.md", srv.URL + "/missing"},
	}, staging)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	payload := PayloadOf(set)
	if payload == nil || len(payload.Docs) != 2 {
		t.Fatalf("docs = %+v, want 2", payload)
	}
	// The dead link is reported, not fatal.
	if len(payload.Skipped) != 1 || !strings.Contains(payload.Skipped[0].Reason, "404") {
		t.Errorf("skipped = %+v, want the 404", payload.Skipped)
	}

	for _, d := range payload.Docs {
		if !strings.HasPrefix(d.Key, "doc:web:") {
			t.Errorf("key %q not in the web namespace", d.Key)
		}
		if d.Origin == "" || !strings.HasPrefix(d.Origin, srv.URL) {
			t.Errorf("origin %q does not deep-link to the page", d.Origin)
		}
	}
	// HTML went through an extractor and produced text with the heading.
	var guide string
	for _, d := range payload.Docs {
		if strings.HasSuffix(d.Origin, "/guide") {
			guide = d.Text
		}
	}
	if !strings.Contains(guide, "The Guide") {
		t.Errorf("extracted guide text missing heading: %q", guide)
	}

	// Determinism: an identical second sync produces identical hashes, which
	// is what makes an unchanged web sweep free downstream.
	set2, err := c.Sync(context.Background(), connector.Config{
		"urls": []any{srv.URL + "/guide"},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if PayloadOf(set2).Docs[0].Hash != guideHash(payload) {
		t.Error("re-fetch of unchanged page changed its hash")
	}
}

func guideHash(p *Payload) string {
	for _, d := range p.Docs {
		if strings.HasSuffix(d.Origin, "/guide") {
			return d.Hash
		}
	}
	return ""
}

func TestSyncAllURLsFailingIsAnError(t *testing.T) {
	c := &Connector{allowPrivate: true, Extractors: testExtractors()}
	_, err := c.Sync(context.Background(), connector.Config{
		"urls": []any{"https://127.0.0.1:1/nope"},
	}, t.TempDir())
	if err == nil {
		t.Fatal("sync with every url dead succeeded")
	}
}

func TestSyncCapsResponseSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(make([]byte, maxPageBytes+1))
	}))
	defer srv.Close()

	c := &Connector{allowPrivate: true, Extractors: testExtractors()}
	_, err := c.Sync(context.Background(), connector.Config{"urls": []any{srv.URL + "/big"}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "byte cap") {
		t.Fatalf("oversized page = %v, want the cap error", err)
	}
}

func TestURLsFromShapes(t *testing.T) {
	if got := URLsFrom(connector.Config{"urls": []any{"https://a", " https://b "}}); len(got) != 2 || got[1] != "https://b" {
		t.Errorf("array form: %v", got)
	}
	if got := URLsFrom(connector.Config{"url": "https://solo"}); len(got) != 1 {
		t.Errorf("single form: %v", got)
	}
	if got := URLsFrom(connector.Config{}); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
}
