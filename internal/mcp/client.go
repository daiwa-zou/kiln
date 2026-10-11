package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned for a 404, so a tool can say "no such page" rather
// than surfacing a transport error an agent would read as a broken server.
var ErrNotFound = errors.New("not found")

// AmbiguousError is a 409 from a bare-slug bench reference that more than one
// visible bench carries. Candidates are "org/slug (id ...)" strings, ready to
// show.
type AmbiguousError struct {
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return "that bench name matches several benches: " + strings.Join(e.Candidates, ", ")
}

// Client reads a kiln instance over its HTTP API.
//
// Deliberately the API rather than the database: an agent's MCP server should
// work against a kiln running anywhere, should carry the caller's own token so
// what it can read is exactly what that token can read, and should not need
// Postgres credentials on the machine the agent runs on.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient builds a client with a sane timeout. A read against a wiki is a
// database query, not a build, so a short timeout is the honest one.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Workspace is one bench the token can see. ID is the only reference that is
// unambiguous everywhere; Org+Slug ("org/slug") is the readable equivalent. A
// bare Slug is unique only within an org.
type Workspace struct {
	ID        string `json:"id"`
	Org       string `json:"org"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	PageCount int    `json:"pageCount"`
}

// PageSummary identifies a page without its body.
type PageSummary struct {
	Path    string   `json:"path"`
	Slug    string   `json:"slug"`
	Type    string   `json:"type"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags,omitempty"`
	Updated string   `json:"updated,omitempty"`
}

// SearchHit is one full-text match, with the snippet the API highlighted.
type SearchHit struct {
	Path    string  `json:"path"`
	Slug    string  `json:"slug"`
	Type    string  `json:"type"`
	Title   string  `json:"title"`
	Rank    float64 `json:"rank"`
	Snippet string  `json:"snippet,omitempty"`
}

// Page is a full page: frontmatter fields plus the markdown body.
type Page struct {
	Path       string   `json:"path"`
	Slug       string   `json:"slug"`
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Tags       []string `json:"tags,omitempty"`
	Related    []string `json:"related,omitempty"`
	Sources    []string `json:"sources,omitempty"`
	Updated    string   `json:"updated,omitempty"`
	BuiltAtRef string   `json:"builtAtRef,omitempty"`
	Body       string   `json:"body"`
}

// Gap is a page other pages link to that does not exist yet -- the wiki's own
// record of what it knows it is missing.
type Gap struct {
	Slug     string   `json:"slug"`
	WantedBy []string `json:"wantedBy"`
}

type artifact struct {
	Kind string `json:"kind"`
	Body string `json:"body"`
}

// Workspaces lists the benches this token can read.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	var out []Workspace
	return out, c.get(ctx, "/api/v1/workspaces", nil, &out)
}

// Search runs full-text search within one bench.
func (c *Client) Search(ctx context.Context, bench, query string, limit int) ([]SearchHit, error) {
	var out []SearchHit
	q := url.Values{"q": {query}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return out, c.get(ctx, c.benchPath(bench, "/search"), q, &out)
}

// Pages lists a bench's pages, ordered by path. There is no server-side type
// filter, so callers wanting one page type either filter what comes back or
// read the index artifact, which is already grouped by type.
func (c *Client) Pages(ctx context.Context, bench string, limit, offset int) ([]PageSummary, error) {
	var out []PageSummary
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	return out, c.get(ctx, c.benchPath(bench, "/pages"), q, &out)
}

// Page fetches one page. ref is a slug or a wiki-relative path; the store
// accepts either, so an agent can pass back whatever a search hit gave it.
func (c *Client) Page(ctx context.Context, bench, ref string) (*Page, error) {
	var out Page
	if err := c.get(ctx, c.benchPath(bench, "/pages/"+ref), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Artifact fetches one of the deterministic documents: overview, index, or log.
func (c *Client) Artifact(ctx context.Context, bench, kind string) (string, error) {
	var out artifact
	if err := c.get(ctx, c.benchPath(bench, "/"+kind), nil, &out); err != nil {
		return "", err
	}
	return out.Body, nil
}

// Backlinks lists the pages linking to a slug.
func (c *Client) Backlinks(ctx context.Context, bench, slug string) ([]PageSummary, error) {
	var out []PageSummary
	return out, c.get(ctx, c.benchPath(bench, "/backlinks/"+slug), nil, &out)
}

// Gaps lists slugs the wiki links to but has not written.
func (c *Client) Gaps(ctx context.Context, bench string, limit int) ([]Gap, error) {
	var out []Gap
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return out, c.get(ctx, c.benchPath(bench, "/gaps"), q, &out)
}

// StatusError is a refusal from a filing route, carrying the status so a tool
// can tell "this key cannot file" (403) from "no such page" (404) from "slow
// down" (429), and the API's own message, which says what to do about it.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("kiln returned %d %s", e.Code, http.StatusText(e.Code))
	}
	return fmt.Sprintf("kiln returned %d %s: %s", e.Code, http.StatusText(e.Code), e.Msg)
}

// FileRequest is one filing into a bench's review queue: a question (kind
// uncertain or contradiction, with a title and detail) or a proposed
// correction (kind correction, with text and the rationale as detail).
type FileRequest struct {
	Kind   string `json:"kind"`
	Page   string `json:"page"`
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
	Text   string `json:"text,omitempty"`
}

// FileResult is the API's answer to a filing: the review item's id, and
// whether it was filed now or was already open.
type FileResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// FileReview files a question or a proposed correction into a bench's review
// queue. It changes no page: a person reads it, and only they can pin one.
func (c *Client) FileReview(ctx context.Context, bench string, req FileRequest) (FileResult, error) {
	var out FileResult
	return out, c.post(ctx, c.benchPath(bench, "/reviews"), req, &out)
}

// RequestGap asks for a page the wiki links to and has not written. The slug
// is one path segment, escaped as such, so it cannot address another route.
func (c *Client) RequestGap(ctx context.Context, bench, slug string) error {
	return c.post(ctx, c.benchPath(bench, "/gaps/"+url.PathEscape(slug)+"/request"), struct{}{}, nil)
}

// post sends a JSON body to a filing route. Refusals come back as a
// *StatusError rather than as get's prose, because the tools answer each
// status differently.
func (c *Client) post(ctx context.Context, path string, body, into any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach kiln at %s: %w (is it running? try `make dev-up`)", c.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Error == "" {
			msg.Error = strings.TrimSpace(string(raw))
		}
		return &StatusError{Code: resp.StatusCode, Msg: msg.Error}
	}
	if into == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

// benchPath addresses a bench by id, "org/slug" or bare slug. A path segment
// cannot carry a "/", so org/slug is sent as "org~slug", which the API reads
// back as org/slug (slugs never contain "~").
func (c *Client) benchPath(bench, suffix string) string {
	ref := strings.Replace(bench, "/", "~", 1)
	return "/api/v1/workspaces/" + url.PathEscape(ref) + suffix
}

func (c *Client) get(ctx context.Context, path string, query url.Values, into any) error {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The overwhelmingly likely cause is that kiln is not running, and an
		// agent reading a bare dial error will report the wiki as broken. Say
		// what to check instead.
		return fmt.Errorf("cannot reach kiln at %s: %w (is it running? try `make dev-up`)", c.BaseURL, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("kiln refused the request (%s): check the token passed as --token or KILN_TOKEN", resp.Status)
	case resp.StatusCode == http.StatusConflict:
		var amb struct {
			Candidates []struct {
				Ref string `json:"ref"`
				ID  string `json:"id"`
			} `json:"candidates"`
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(body, &amb) == nil && len(amb.Candidates) > 0 {
			e := &AmbiguousError{}
			for _, c := range amb.Candidates {
				e.Candidates = append(e.Candidates, fmt.Sprintf("%s (id %s)", c.Ref, c.ID))
			}
			return e
		}
		return fmt.Errorf("kiln returned %s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 512)])))
	case resp.StatusCode >= 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("kiln returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}
