// Package mcp exposes a kiln wiki to agents over the Model Context Protocol.
//
// The wiki is already the artifact worth reading: prose compiled from sources
// and kept current, rather than raw material an agent has to re-derive on every
// question. What was missing was a way for an agent to reach it without being
// told an HTTP schema. These tools are that.
//
// The shape is deliberately search-first. An agent that can search, read, and
// follow links has everything it needs; adding a tool per API endpoint would
// spend the model's attention on choosing between them.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultBenchFallback is used when the server was started without --workspace
// and a tool call omits one. Naming the failure is better than guessing at a
// bench, so this is only ever used to build the error message.
const noBenchConfigured = ""

// Server wires kiln's read API to MCP tools.
type Server struct {
	client *Client
	// bench is the default workspace slug, from --workspace. Empty means every
	// tool call must name one, which is the right default for an instance with
	// several benches.
	bench string
	// Version is reported to the client during initialize.
	Version string
}

// NewServer builds the MCP server. bench may be empty.
func NewServer(client *Client, bench, version string) *Server {
	return &Server{client: client, bench: bench, Version: version}
}

// benchFor resolves the bench a call should read, preferring an explicit
// argument over the configured default.
func (s *Server) benchFor(arg string) (string, error) {
	if b := strings.TrimSpace(arg); b != "" {
		return b, nil
	}
	if s.bench != noBenchConfigured {
		return s.bench, nil
	}
	return "", errors.New(
		"no bench specified: pass `bench` (list_benches shows what is available), or start the server with --workspace")
}

// --- tool arguments ---------------------------------------------------------
//
// The jsonschema tags are the only description an agent sees for a parameter,
// so they say what to pass rather than restating the field name.

type benchArgs struct {
	Bench string `json:"bench,omitempty" jsonschema:"the bench (workspace) slug; omit to use the server's default"`
}

type searchArgs struct {
	Query string `json:"query" jsonschema:"what to look for; matched against page text, not just titles"`
	Bench string `json:"bench,omitempty" jsonschema:"the bench (workspace) slug; omit to use the server's default"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum hits to return (default 10)"`
}

type readArgs struct {
	Page  string `json:"page" jsonschema:"the page's slug or its wiki-relative path, exactly as search returned it"`
	Bench string `json:"bench,omitempty" jsonschema:"the bench (workspace) slug; omit to use the server's default"`
}

type listArgs struct {
	Bench  string `json:"bench,omitempty" jsonschema:"the bench (workspace) slug; omit to use the server's default"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum pages to return (default 100)"`
	Offset int    `json:"offset,omitempty" jsonschema:"how many pages to skip, for paging through a large bench"`
}

// Register adds every tool to an MCP server.
func (s *Server) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_benches",
		Description: "List the benches (workspaces) on this kiln instance. " +
			"A bench is one wiki, compiled from its own sources. Call this first when you do not " +
			"know which bench holds what you need, or when a tool reports an unknown bench.",
	}, s.listBenches)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "search_wiki",
		Description: "Full-text search across a bench's pages. This is the way in: the wiki is " +
			"compiled prose about the underlying sources, so searching it answers questions " +
			"without reading the sources themselves. Returns page slugs with matching snippets; " +
			"pass a slug to read_page for the whole page.",
	}, s.searchWiki)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "read_page",
		Description: "Read one page in full: its markdown body plus the pages it relates to and " +
			"the sources it was written from. Takes the slug or path from search_wiki, " +
			"list_pages, or another page's related list.",
	}, s.readPage)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "wiki_overview",
		Description: "Read a bench's overview and index: what this wiki covers and every page in " +
			"it, grouped by type. Use this for orientation before searching, when you need to " +
			"know what a bench knows about rather than to answer a specific question.",
	}, s.wikiOverview)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_pages",
		Description: "List a bench's pages with their titles and types, ordered by path. " +
			"Prefer search_wiki when you know what you are looking for; this is for enumerating " +
			"a bench you intend to walk through.",
	}, s.listPages)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "page_backlinks",
		Description: "List the pages that link to a given page. Useful for finding the context " +
			"something is discussed in: what refers to this concept, and from where.",
	}, s.pageBacklinks)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "wiki_gaps",
		Description: "List pages the wiki links to but has not written yet -- what it knows it is " +
			"missing, and which pages wanted them. Use this to tell the difference between " +
			"'the wiki says nothing about X' and 'the wiki has not covered X yet'.",
	}, s.wikiGaps)
}

// --- tool implementations ---------------------------------------------------

func (s *Server) listBenches(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	benches, err := s.client.Workspaces(ctx)
	if err != nil {
		return toolError(err), nil, nil
	}
	if len(benches) == 0 {
		return text("This kiln instance has no benches yet."), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d bench(es):\n\n", len(benches))
	for _, w := range benches {
		fmt.Fprintf(&b, "- %s — %s (%d pages)\n", w.Slug, w.Name, w.PageCount)
		// An empty bench is worth calling out: it has been created but never
		// built, so searching it will return nothing for reasons that have
		// nothing to do with the query.
		if w.PageCount == 0 {
			b.WriteString("  (no pages yet — this bench has not been built)\n")
		}
	}
	return text(b.String()), nil, nil
}

func (s *Server) searchWiki(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	bench, err := s.benchFor(args.Bench)
	if err != nil {
		return toolError(err), nil, nil
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return toolError(errors.New("query is empty")), nil, nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}

	hits, err := s.client.Search(ctx, bench, query, limit)
	if err != nil {
		return toolError(benchErr(err, bench)), nil, nil
	}
	if len(hits) == 0 {
		return text(fmt.Sprintf(
			"No pages in %q match %q.\n\n"+
				"The wiki may not cover this yet — wiki_gaps lists what it knows it is missing, "+
				"and wiki_overview shows what it does cover.", bench, query)), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s) in %s for %q:\n\n", len(hits), bench, query)
	for _, h := range hits {
		fmt.Fprintf(&b, "## %s\n", h.Title)
		fmt.Fprintf(&b, "slug: %s (%s)\n", h.Slug, h.Type)
		if h.Snippet != "" {
			fmt.Fprintf(&b, "%s\n", highlightToMarkdown(h.Snippet))
		}
		b.WriteString("\n")
	}
	b.WriteString("Read any of these in full with read_page.\n")
	return text(b.String()), nil, nil
}

func (s *Server) readPage(ctx context.Context, _ *mcp.CallToolRequest, args readArgs) (*mcp.CallToolResult, any, error) {
	bench, err := s.benchFor(args.Bench)
	if err != nil {
		return toolError(err), nil, nil
	}
	ref := strings.TrimSpace(args.Page)
	if ref == "" {
		return toolError(errors.New("page is empty: pass a slug or a wiki-relative path")), nil, nil
	}

	p, err := s.client.Page(ctx, bench, ref)
	if errors.Is(err, ErrNotFound) {
		return toolError(fmt.Errorf(
			"no page %q in bench %q — search_wiki will find the right slug, and wiki_gaps says whether this page is simply not written yet", ref, bench)), nil, nil
	}
	if err != nil {
		return toolError(benchErr(err, bench)), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", p.Title)
	fmt.Fprintf(&b, "slug: %s\ntype: %s\npath: %s\n", p.Slug, p.Type, p.Path)
	if p.Updated != "" {
		fmt.Fprintf(&b, "updated: %s\n", p.Updated)
	}
	// The ref a page was built at is the honest answer to "how current is
	// this", which an agent citing the page should be able to say.
	if p.BuiltAtRef != "" {
		fmt.Fprintf(&b, "built at: %s\n", p.BuiltAtRef)
	}
	if len(p.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(p.Tags, ", "))
	}
	if len(p.Related) > 0 {
		fmt.Fprintf(&b, "related pages: %s\n", strings.Join(p.Related, ", "))
	}
	if len(p.Sources) > 0 {
		fmt.Fprintf(&b, "written from: %s\n", strings.Join(p.Sources, ", "))
	}
	b.WriteString("\n---\n\n")
	b.WriteString(p.Body)
	return text(b.String()), nil, nil
}

func (s *Server) wikiOverview(ctx context.Context, _ *mcp.CallToolRequest, args benchArgs) (*mcp.CallToolResult, any, error) {
	bench, err := s.benchFor(args.Bench)
	if err != nil {
		return toolError(err), nil, nil
	}

	overview, err := s.client.Artifact(ctx, bench, "overview")
	if err != nil {
		return toolError(benchErr(err, bench)), nil, nil
	}
	index, err := s.client.Artifact(ctx, bench, "index")
	if err != nil {
		return toolError(benchErr(err, bench)), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Bench: %s\n\n", bench)
	b.WriteString(strings.TrimSpace(overview))
	b.WriteString("\n\n---\n\n")
	b.WriteString(strings.TrimSpace(index))
	b.WriteString("\n")
	return text(b.String()), nil, nil
}

func (s *Server) listPages(ctx context.Context, _ *mcp.CallToolRequest, args listArgs) (*mcp.CallToolResult, any, error) {
	bench, err := s.benchFor(args.Bench)
	if err != nil {
		return toolError(err), nil, nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}

	pages, err := s.client.Pages(ctx, bench, limit, args.Offset)
	if err != nil {
		return toolError(benchErr(err, bench)), nil, nil
	}
	if len(pages) == 0 {
		return text(fmt.Sprintf(
			"Bench %q has no pages at this offset. If it has none at all, it has not been built yet.", bench)), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d page(s) in %s:\n\n", len(pages), bench)
	for _, p := range pages {
		fmt.Fprintf(&b, "- %s — %s (%s)\n", p.Slug, p.Title, p.Type)
	}
	// Paging only matters when there might be more, and saying so beats an
	// agent assuming it has seen the whole bench.
	if len(pages) == limit {
		fmt.Fprintf(&b, "\nThere may be more; call again with offset=%d.\n", args.Offset+limit)
	}
	return text(b.String()), nil, nil
}

func (s *Server) pageBacklinks(ctx context.Context, _ *mcp.CallToolRequest, args readArgs) (*mcp.CallToolResult, any, error) {
	bench, err := s.benchFor(args.Bench)
	if err != nil {
		return toolError(err), nil, nil
	}
	slug := strings.TrimSpace(args.Page)
	if slug == "" {
		return toolError(errors.New("page is empty: pass the slug to find links to")), nil, nil
	}

	rows, err := s.client.Backlinks(ctx, bench, slug)
	if err != nil {
		return toolError(benchErr(err, bench)), nil, nil
	}
	if len(rows) == 0 {
		return text(fmt.Sprintf("Nothing in %s links to %q.", bench, slug)), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d page(s) in %s link to %s:\n\n", len(rows), bench, slug)
	for _, p := range rows {
		fmt.Fprintf(&b, "- %s — %s (%s)\n", p.Slug, p.Title, p.Type)
	}
	return text(b.String()), nil, nil
}

func (s *Server) wikiGaps(ctx context.Context, _ *mcp.CallToolRequest, args listArgs) (*mcp.CallToolResult, any, error) {
	bench, err := s.benchFor(args.Bench)
	if err != nil {
		return toolError(err), nil, nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 50
	}

	gaps, err := s.client.Gaps(ctx, bench, limit)
	if err != nil {
		return toolError(benchErr(err, bench)), nil, nil
	}
	if len(gaps) == 0 {
		return text(fmt.Sprintf("Bench %q has no unresolved links: every page it refers to exists.", bench)), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d page(s) referred to but not written in %s:\n\n", len(gaps), bench)
	for _, g := range gaps {
		fmt.Fprintf(&b, "- %s", g.Slug)
		if len(g.WantedBy) > 0 {
			fmt.Fprintf(&b, " — wanted by %s", strings.Join(g.WantedBy, ", "))
		}
		b.WriteString("\n")
	}
	return text(b.String()), nil, nil
}

// --- result helpers ---------------------------------------------------------

// searchHighlight is how Postgres marks matched terms in a snippet
// (ts_headline StartSel/StopSel). The UI renders those markers as a highlight;
// handed to a model as markdown they read as a wikilink to a page named after
// the matched word, which is a page that does not exist. Bold says the same
// thing without inventing a link.
const (
	highlightOpen  = "[[["
	highlightClose = "]]]"
)

func highlightToMarkdown(snippet string) string {
	s := strings.TrimSpace(snippet)
	// Only rewrite balanced pairs: a truncated fragment can end mid-marker,
	// and turning a stray opener into ** would bold the rest of the snippet.
	for strings.Count(s, highlightOpen) > 0 && strings.Count(s, highlightClose) > 0 {
		open := strings.Index(s, highlightOpen)
		close := strings.Index(s[open:], highlightClose)
		if close < 0 {
			break
		}
		close += open
		term := s[open+len(highlightOpen) : close]
		s = s[:open] + "**" + term + "**" + s[close+len(highlightClose):]
	}
	// Whatever markers survive are unbalanced; drop them rather than leave
	// bracket noise the model has to interpret.
	s = strings.ReplaceAll(s, highlightOpen, "")
	s = strings.ReplaceAll(s, highlightClose, "")
	return s
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// toolError returns a failure the model can read and act on, rather than a
// protocol-level error that surfaces as a broken tool. A wrong bench name or a
// missing page is a normal thing for an agent to hit and recover from.
func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

// benchErr turns a 404 on a bench-scoped call into the question actually worth
// asking: kiln answers "no such bench" and "you cannot see that bench"
// identically, so the message must cover both.
func benchErr(err error, bench string) error {
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("no bench %q, or this token cannot read it — list_benches shows what is available", bench)
	}
	return err
}
