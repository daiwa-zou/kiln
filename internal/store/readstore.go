package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/daiwa-zou/kiln/internal/wiki"
)

// ErrNotFound marks a lookup whose subject does not exist (or is invisible to
// the caller, which must answer identically so absence cannot be probed).
var ErrNotFound = errors.New("store: not found")

// WorkspaceRow is a workspace as the read API sees it.
type WorkspaceRow struct {
	ID        string
	Slug      string
	Name      string
	PageCount int
	Revision  int64
}

// PageInfo is a page without its body, for listings. Loading bodies to render
// a sidebar was the single largest waste on the read path.
type PageInfo struct {
	Path    string
	Slug    string
	Type    string
	Title   string
	Tags    []string
	Updated string
}

// SearchHit is one full-text match.
type SearchHit struct {
	Path  string
	Slug  string
	Type  string
	Title string
	Rank  float64
	// Snippet is a ts_headline excerpt with the match marked up, so results
	// show why they matched instead of a bare title.
	Snippet string
}

// Gap is a page the wiki links to but does not have.
type Gap struct {
	Slug     string
	WantedBy int
}

// ListWorkspaces returns the workspaces visible to a caller: all of them for
// an admin (or when auth is disabled), otherwise those in orgs the user
// belongs to.
func (s *WikiStore) ListWorkspaces(ctx context.Context, userID string, admin bool) ([]WorkspaceRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ws.id, ws.slug, ws.name,
		       coalesce(wk.page_count, 0), coalesce(wk.revision, 0)
		FROM workspaces ws
		LEFT JOIN wikis wk ON wk.workspace_id = ws.id
		WHERE $1 OR EXISTS (
		    SELECT 1 FROM org_members m
		    WHERE m.org_id = ws.org_id AND m.user_id = $2::uuid)
		ORDER BY ws.slug`, admin, nullable(userID))
	if err != nil {
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	defer rows.Close()

	out := []WorkspaceRow{}
	for rows.Next() {
		var ws WorkspaceRow
		if err := rows.Scan(&ws.ID, &ws.Slug, &ws.Name, &ws.PageCount, &ws.Revision); err != nil {
			return nil, fmt.Errorf("store: scan workspace: %w", err)
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// ResolveWorkspace turns a slug or UUID into a workspace row, bounded to what
// the caller can see. Slugs are only unique per org, so the visibility bound
// is also the correctness bound; ORDER BY makes the residual ambiguity (one
// user in two orgs reusing a slug) deterministic.
func (s *WikiStore) ResolveWorkspace(ctx context.Context, ref, userID string, admin bool) (WorkspaceRow, error) {
	var ws WorkspaceRow
	err := s.pool.QueryRow(ctx, `
		SELECT ws.id, ws.slug, ws.name,
		       coalesce(wk.page_count, 0), coalesce(wk.revision, 0)
		FROM workspaces ws
		LEFT JOIN wikis wk ON wk.workspace_id = ws.id
		WHERE (ws.slug = $1 OR ws.id::text = $1)
		  AND ($2 OR EXISTS (
		      SELECT 1 FROM org_members m
		      WHERE m.org_id = ws.org_id AND m.user_id = $3::uuid))
		ORDER BY ws.created_at
		LIMIT 1`, ref, admin, nullable(userID)).
		Scan(&ws.ID, &ws.Slug, &ws.Name, &ws.PageCount, &ws.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceRow{}, ErrNotFound
	}
	if err != nil {
		return WorkspaceRow{}, fmt.Errorf("store: resolve workspace: %w", err)
	}
	return ws, nil
}

// LoadPageSummaries lists live pages without their bodies.
func (s *WikiStore) LoadPageSummaries(ctx context.Context, workspaceID string, limit, offset int) ([]PageInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.path, p.slug, p.type, p.title, p.frontmatter
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		ORDER BY p.path
		LIMIT $2 OFFSET $3`, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: load page summaries: %w", err)
	}
	defer rows.Close()

	out := []PageInfo{}
	for rows.Next() {
		var (
			p  PageInfo
			fm []byte
		)
		if err := rows.Scan(&p.Path, &p.Slug, &p.Type, &p.Title, &fm); err != nil {
			return nil, fmt.Errorf("store: scan page summary: %w", err)
		}
		if len(fm) > 0 {
			var meta wiki.Frontmatter
			if err := json.Unmarshal(fm, &meta); err == nil {
				p.Tags, p.Updated = meta.Tags, meta.Updated
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LoadPage returns one live page with its body, addressable by path or slug: a
// wikilink carries a slug, the tree carries a path, and both must resolve.
func (s *WikiStore) LoadPage(ctx context.Context, workspaceID, ref string) (wiki.Page, error) {
	var (
		p        wiki.Page
		pageType string
		fm       []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT p.path, p.slug, p.type, p.title, p.frontmatter, p.body
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		  AND (p.path = $2 OR p.slug = $2)
		LIMIT 1`, workspaceID, ref).
		Scan(&p.Path, &p.Slug, &pageType, &p.Meta.Title, &fm, &p.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return wiki.Page{}, ErrNotFound
	}
	if err != nil {
		return wiki.Page{}, fmt.Errorf("store: load page: %w", err)
	}

	p.Meta.Type = wiki.PageType(pageType)
	if len(fm) > 0 {
		if err := json.Unmarshal(fm, &p.Meta); err != nil {
			return wiki.Page{}, fmt.Errorf("store: decode frontmatter for %s: %w", p.Path, err)
		}
	}
	return p, nil
}

// Search runs weighted full-text search over live pages.
//
// Two queries are built from the input, and the difference is what makes this
// usable by something that asks questions rather than typing keywords.
// plainto_tsquery ANDs every lexeme, so "what happens when a worker dies"
// requires `happen` as much as `worker` -- and a page containing the sentence
// "worker dies mid-build" matches nothing, because one incidental word from the
// question is missing. Every term ANDed is the right default for a keyword box
// and the wrong one for a sentence.
//
// So the AND query still decides *order* -- a page matching every term ranks
// above one matching some, and keyword searches return what they always did --
// while the OR query decides *membership*. Nothing that used to rank first
// stops ranking first; results simply continue past where they used to stop.
//
// Rewriting `&` to `|` textually is safe: plainto_tsquery has already parsed
// and sanitized the input, and its output is a well-formed tsquery containing
// no other operators (phrase distance comes from phraseto_tsquery, negation
// from to_tsquery, and neither is used here).
//
// prefix matches the final lexeme as a prefix, which is what a search box being
// typed into needs and what a finished query does not. Mid-word, every keystroke
// is a term no document contains -- "kuber" is not "kubernetes" -- so an
// as-you-type search without it spends most of its keystrokes showing nothing
// and only settles once a word happens to end. Only the last lexeme is widened:
// the earlier words are ones the user finished typing and meant. Callers that
// submit a completed query leave it off, so the endpoint agents use keeps its
// exact-term meaning.
func (s *WikiStore) Search(ctx context.Context, workspaceID, query string, limit, offset int, prefix bool) ([]SearchHit, error) {
	// The headline marker is [[[match]]] rather than HTML: the UI escapes all
	// content before rendering, so an HTML marker would arrive escaped and
	// useless, while a bracket marker survives escaping and is swapped for a
	// highlight span afterwards.
	rows, err := s.pool.Query(ctx, `
		WITH raw AS (
		    SELECT CAST(plainto_tsquery('english', $2) AS text) AS t,
		           CAST($5 AS boolean) AS pre
		), q AS (
		    SELECT CAST(CASE WHEN t = '' OR NOT pre THEN t
		                     ELSE t || ':*' END AS tsquery) AS strict,
		           CAST(CASE WHEN t = '' THEN t
		                     WHEN pre THEN replace(t, ' & ', ' | ') || ':*'
		                     ELSE replace(t, ' & ', ' | ') END AS tsquery) AS loose
		    FROM raw
		)
		SELECT p.path, p.slug, p.type, p.title,
		       ts_rank(p.search, q.loose) AS rank,
		       ts_headline('english', p.body, q.loose,
		                   'StartSel=[[[, StopSel=]]], MaxWords=25, MinWords=10, MaxFragments=1')
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		CROSS JOIN q
		WHERE w.workspace_id = $1
		  AND p.deleted_at IS NULL
		  AND p.search @@ q.loose
		ORDER BY (p.search @@ q.strict) DESC, rank DESC, p.slug
		LIMIT $3 OFFSET $4`, workspaceID, query, limit, offset, prefix)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	defer rows.Close()

	out := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.Path, &h.Slug, &h.Type, &h.Title, &h.Rank, &h.Snippet); err != nil {
			return nil, fmt.Errorf("store: scan search hit: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GraphNode is one live page as the graph view renders it.
type GraphNode struct {
	Slug  string
	Title string
	Type  string
	// Links counts inbound resolved links, so the view can size hubs.
	Links int
}

// GraphEdge is one resolved wikilink.
type GraphEdge struct {
	From string
	To   string
}

// Graph returns up to limit live pages — the best-connected first, so a
// truncated graph shows the wiki's hubs — and the resolved links between the
// pages that made the cut. This was the one unpaginated read in the API; a
// large bench must not ship its entire link table in one payload.
func (s *WikiStore) Graph(ctx context.Context, workspaceID string, limit int) (nodes []GraphNode, edges []GraphEdge, total int, err error) {
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL`,
		workspaceID).Scan(&total); err != nil {
		return nil, nil, 0, fmt.Errorf("store: graph total: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT p.slug, p.title, p.type,
		       (SELECT count(*) FROM page_links l WHERE l.to_page_id = p.id) AS inbound
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		ORDER BY inbound DESC, p.slug
		LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("store: graph nodes: %w", err)
	}
	defer rows.Close()

	nodes = []GraphNode{}
	included := map[string]bool{}
	for rows.Next() {
		var n GraphNode
		if err := rows.Scan(&n.Slug, &n.Title, &n.Type, &n.Links); err != nil {
			return nil, nil, 0, fmt.Errorf("store: scan graph node: %w", err)
		}
		nodes = append(nodes, n)
		included[n.Slug] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}

	links, err := s.pool.Query(ctx, `
		SELECT f.slug, t.slug
		FROM page_links l
		JOIN pages f ON f.id = l.from_page_id AND f.deleted_at IS NULL
		JOIN pages t ON t.id = l.to_page_id AND t.deleted_at IS NULL
		JOIN wikis w ON w.id = f.wiki_id
		WHERE w.workspace_id = $1 AND l.resolved`, workspaceID)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("store: graph edges: %w", err)
	}
	defer links.Close()

	edges = []GraphEdge{}
	for links.Next() {
		var e GraphEdge
		if err := links.Scan(&e.From, &e.To); err != nil {
			return nil, nil, 0, fmt.Errorf("store: scan graph edge: %w", err)
		}
		// An edge whose endpoint fell outside the node cap would render as a
		// line to nowhere.
		if included[e.From] && included[e.To] {
			edges = append(edges, e)
		}
	}
	return nodes, edges, total, links.Err()
}

// Gaps lists unresolved wikilink targets: pages the wiki has declared it wants
// and does not have, the cheapest and most precise missing-page signal there is.
func (s *WikiStore) Gaps(ctx context.Context, workspaceID string, limit, offset int) ([]Gap, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.to_slug, count(*) AS wanted_by
		FROM page_links l
		JOIN pages p ON p.id = l.from_page_id
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND NOT l.resolved AND p.deleted_at IS NULL
		GROUP BY l.to_slug
		ORDER BY wanted_by DESC, l.to_slug
		LIMIT $2 OFFSET $3`, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: gaps: %w", err)
	}
	defer rows.Close()

	out := []Gap{}
	for rows.Next() {
		var g Gap
		if err := rows.Scan(&g.Slug, &g.WantedBy); err != nil {
			return nil, fmt.Errorf("store: scan gap: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
