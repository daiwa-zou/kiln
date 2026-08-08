// Package web materializes public web pages as a SourceSet.
//
// This is the third Connector, and deliberately the last (roadmap M4): a URL
// fetcher is the largest new attack surface in the system, because its
// configuration is API-writable data that makes the worker issue network
// requests. The guardrails are therefore not optional trimmings, they are the
// design: https only, hosts must resolve to public addresses with the check
// applied at connect time (resolve-then-connect pinning, so redirects and
// DNS rebinding meet the same wall), response sizes are capped, and no
// credentials of any kind are ever attached or forwarded.
package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/extract"
	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
)

// Fetch limits.
const (
	maxPageBytes = 2 << 20 // 2 MiB of HTML is an article, not a tarball
	maxRedirects = 5
	fetchTimeout = 30 * time.Second
)

// Connector fetches a fixed set of URLs.
type Connector struct {
	// Extractors converts fetched documents to text. Defaults to the
	// standard set (FormatHTML lands on pandoc).
	Extractors extract.Extractors

	// allowPrivate disables the address guard, for tests that must talk to
	// an httptest server on loopback. Unexported on purpose: connector
	// configs are API-writable data and must never be able to reach this.
	allowPrivate bool
}

func init() { connector.Register(&Connector{}) }

// Kind implements connector.Connector.
func (c *Connector) Kind() string { return "web" }

// Trigger implements connector.Connector. The web has no push; freshness
// comes from the worker's poll scheduler.
func (c *Connector) Trigger() connector.TriggerMode { return connector.TriggerPoll }

// Payload mirrors the upload connector's: extracted documents ready for the
// doc mapper, plus what was skipped and why.
type Payload struct {
	Docs    []docmap.Doc
	Skipped []Skip
}

// Skip records one URL that was not ingested.
type Skip struct {
	URL    string
	Reason string
}

// PayloadOf extracts a web payload, nil for any other connector's set.
func PayloadOf(set *connector.SourceSet) *Payload {
	if set == nil {
		return nil
	}
	p, _ := set.Native.(*Payload)
	return p
}

// URLsFrom reads the connector config's url list, accepting both a JSON
// array and a single "url" string.
func URLsFrom(cfg connector.Config) []string {
	var out []string
	if raw, ok := cfg["urls"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	if raw, ok := cfg["urls"].([]string); ok {
		for _, s := range raw {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	if s, ok := cfg["url"].(string); ok && strings.TrimSpace(s) != "" {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// ValidateURL enforces the fetch policy on a configured URL, for the API to
// reject bad configs at write time exactly as the fetch would at sync time.
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("connector/web: url: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("connector/web: url must be https, got %q", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("connector/web: url must not embed credentials")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("connector/web: url has no host")
	}
	return u, nil
}

// Sync fetches every configured URL, extracts text, and stages it under dst.
//
// A page that cannot be fetched or extracted is skipped rather than failing
// the sync, mirroring the upload connector: one dead link must not cost the
// living pages. Skips are reported so they are visible rather than silent.
func (c *Connector) Sync(ctx context.Context, cfg connector.Config, dst string) (*connector.SourceSet, error) {
	urls := URLsFrom(cfg)
	if len(urls) == 0 {
		return nil, fmt.Errorf("connector/web: config needs urls")
	}
	if dst == "" {
		return nil, fmt.Errorf("connector/web: a staging directory is required")
	}

	extractors := c.Extractors
	if extractors == nil {
		extractors = extract.DefaultExtractors()
	}
	client := c.client()

	set := &connector.SourceSet{Root: dst, Kind: c.Kind()}
	payload := &Payload{}

	for _, raw := range urls {
		doc, err := c.fetchOne(ctx, client, extractors, dst, raw)
		if err != nil {
			payload.Skipped = append(payload.Skipped, Skip{URL: raw, Reason: firstLine(err.Error())})
			continue
		}
		payload.Docs = append(payload.Docs, *doc)
		set.Items = append(set.Items, connector.Item{
			Key:    doc.Key,
			Kind:   "doc",
			Title:  doc.Title,
			Hash:   doc.Hash,
			Path:   doc.Path,
			Origin: doc.Origin,
			Meta:   map[string]any{"format": "html"},
		})
	}
	if len(payload.Docs) == 0 && len(payload.Skipped) > 0 {
		// Every URL failing is a configuration or outage problem the run
		// should surface, not quietly produce an empty wiki from.
		return nil, fmt.Errorf("connector/web: all %d url(s) failed; first: %s",
			len(payload.Skipped), payload.Skipped[0].Reason)
	}

	set.Native = payload
	return set, nil
}

func (c *Connector) fetchOne(ctx context.Context, client *http.Client, extractors extract.Extractors, dst, raw string) (*docmap.Doc, error) {
	u, err := ValidateURL(raw)
	if err != nil {
		// The test escape hatch also relaxes the scheme: httptest servers
		// speak plain http on loopback. Unreachable in production, where
		// allowPrivate cannot be set.
		if !c.allowPrivate {
			return nil, err
		}
		u, err = url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("connector/web: unparseable test url %q", raw)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	// Identify honestly; carry nothing else. No cookies, no auth, ever.
	req.Header.Set("User-Agent", "kiln-web-connector")
	req.Header.Set("Accept", "text/html, text/markdown;q=0.9, text/plain;q=0.8")

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch: %s", res.Status)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPageBytes {
		return nil, fmt.Errorf("fetch: page exceeds the %d byte cap", maxPageBytes)
	}

	// Stage the raw fetch, extract text from it, then stage the text — the
	// same two-step the upload connector performs on disk files.
	rel := stagedName(u)
	rawPath := filepath.Join(dst, rel+stagedExt(res.Header.Get("Content-Type")))
	if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(rawPath, body, 0o644); err != nil {
		return nil, err
	}
	result, err := extractors.Extract(ctx, rawPath)
	if err != nil {
		return nil, err
	}
	staged := rel + ".md"
	if err := os.WriteFile(filepath.Join(dst, staged), []byte(result.Text), 0o644); err != nil {
		return nil, err
	}

	key := string(diff.DocKey(diff.WebOrigin(u.String())))
	return &docmap.Doc{
		Key:    key,
		Path:   staged,
		Title:  titleOf(u, result.Text),
		Text:   result.Text,
		Hash:   result.Hash,
		Origin: u.String(),
	}, nil
}

// client builds the pinned-dial HTTP client.
//
// SECURITY: the guard lives in DialContext, not in a pre-flight check, so it
// applies to every connection the client ever opens — including redirect
// hops and fresh DNS answers. Resolve, filter, then dial the vetted IP
// directly: a host that re-resolves to something private mid-session never
// gets connected to.
func (c *Connector) client() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if c.allowPrivate || !isForbiddenAddress(ip) {
					return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				}
			}
			return nil, fmt.Errorf("connector/web: %s resolves only to non-public addresses", host)
		},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("connector/web: more than %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "https" && !c.allowPrivate {
				return fmt.Errorf("connector/web: redirect to non-https %s", req.URL)
			}
			// Belt and braces: Go already strips Authorization on cross-host
			// redirects, and this client never sets credentials at all.
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
}

func isForbiddenAddress(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || // includes 169.254.169.254 cloud metadata
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// stagedName maps a URL to a stable filesystem-safe relative name.
func stagedName(u *url.URL) string {
	sum := sha256.Sum256([]byte(u.String()))
	base := strings.Trim(path.Base(u.Path), "/")
	if base == "" || base == "." {
		base = "index"
	}
	return sanitize(u.Hostname()) + "-" + sanitize(base) + "-" + hex.EncodeToString(sum[:])[:8]
}

func stagedExt(contentType string) string {
	switch {
	case strings.Contains(contentType, "markdown"):
		return ".raw.md"
	case strings.Contains(contentType, "text/plain"):
		return ".raw.txt"
	default:
		return ".raw.html"
	}
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "page"
	}
	return out
}

// titleOf prefers the page's first markdown heading, falling back to the URL.
func titleOf(u *url.URL, text string) string {
	for _, line := range strings.Split(text, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			if t := strings.TrimSpace(after); t != "" {
				return t
			}
		}
	}
	return u.Hostname() + u.Path
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
