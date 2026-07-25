package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/wiki"
)

// JobStore implements jobs.Store against Postgres.
//
// The interface is declared by the consumer (internal/jobs), so the SQL lives
// here and the pipeline stays testable against an in-memory fake.
type JobStore struct {
	pool *pgxpool.Pool
}

// NewJobStore wraps a pool.
func NewJobStore(pool *pgxpool.Pool) *JobStore { return &JobStore{pool: pool} }

var _ jobs.Store = (*JobStore)(nil)

// LoadSources returns the live source records for a workspace. These are the
// baseline for both incremental skip and cascade deletion.
func (s *JobStore) LoadSources(ctx context.Context, workspaceID string) ([]diff.SourceRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key, input_hash, files_written, blob_keys
		FROM sources
		WHERE workspace_id = $1 AND deleted_at IS NULL
		ORDER BY key`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: load sources: %w", err)
	}
	defer rows.Close()

	var out []diff.SourceRecord
	for rows.Next() {
		var (
			rec         diff.SourceRecord
			key         string
			files, blob []byte
		)
		if err := rows.Scan(&key, &rec.InputHash, &files, &blob); err != nil {
			return nil, fmt.Errorf("store: scan source: %w", err)
		}
		rec.Key = diff.Key(key)
		if err := json.Unmarshal(files, &rec.FilesWritten); err != nil {
			return nil, fmt.Errorf("store: decode files_written for %s: %w", key, err)
		}
		if err := json.Unmarshal(blob, &rec.BlobKeys); err != nil {
			return nil, fmt.Errorf("store: decode blob_keys for %s: %w", key, err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// LoadPages returns live pages for a workspace, used to resolve wikilinks and
// to rebuild the index.
func (s *JobStore) LoadPages(ctx context.Context, workspaceID string) ([]wiki.Page, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.path, p.slug, p.type, p.title, p.frontmatter, p.body
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		ORDER BY p.path`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: load pages: %w", err)
	}
	defer rows.Close()

	var out []wiki.Page
	for rows.Next() {
		var (
			p        wiki.Page
			pageType string
			fm       []byte
		)
		if err := rows.Scan(&p.Path, &p.Slug, &pageType, &p.Meta.Title, &fm, &p.Body); err != nil {
			return nil, fmt.Errorf("store: scan page: %w", err)
		}
		p.Meta.Type = wiki.PageType(pageType)

		// Frontmatter round-trips through jsonb so tags, related, and sources
		// survive without a second schema to keep in step.
		if len(fm) > 0 {
			if err := json.Unmarshal(fm, &p.Meta); err != nil {
				return nil, fmt.Errorf("store: decode frontmatter for %s: %w", p.Path, err)
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LoadSteering returns the workspace's purpose and schema documents plus any
// active per-page corrections.
//
// Corrections are what let human knowledge survive regeneration: pages are
// never hand-edited, so a correction lives outside the page and is re-injected
// into every prompt that rebuilds it.
func (s *JobStore) LoadSteering(ctx context.Context, workspaceID string) (jobs.Steering, error) {
	out := jobs.Steering{Corrections: map[string][]string{}}

	rows, err := s.pool.Query(ctx,
		`SELECT kind, body FROM steering_docs WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return out, fmt.Errorf("store: load steering docs: %w", err)
	}
	for rows.Next() {
		var kind, body string
		if err := rows.Scan(&kind, &body); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: scan steering doc: %w", err)
		}
		switch kind {
		case "purpose":
			out.Purpose = body
		case "schema":
			out.Schema = body
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	corr, err := s.pool.Query(ctx, `
		SELECT p.slug, c.body
		FROM page_corrections c
		JOIN pages p ON p.id = c.page_id
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND c.active AND p.deleted_at IS NULL
		ORDER BY p.slug, c.created_at`, workspaceID)
	if err != nil {
		return out, fmt.Errorf("store: load corrections: %w", err)
	}
	defer corr.Close()

	for corr.Next() {
		var slug, body string
		if err := corr.Scan(&slug, &body); err != nil {
			return out, fmt.Errorf("store: scan correction: %w", err)
		}
		out.Corrections[slug] = append(out.Corrections[slug], body)
	}
	return out, corr.Err()
}

// Import commits one run's output.
//
// Everything lands in a single transaction on purpose: pages, links, sources,
// and the revision bump have to agree, or a crash mid-write leaves a wiki whose
// index describes pages that were never stored.
func (s *JobStore) Import(ctx context.Context, in jobs.ImportRequest) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin import: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	wikiID, err := ensureWiki(ctx, tx, in.WorkspaceID)
	if err != nil {
		return err
	}

	for _, p := range in.UpsertPages {
		if err := upsertPage(ctx, tx, wikiID, p); err != nil {
			return err
		}
	}
	if err := softDeletePages(ctx, tx, wikiID, in.SoftDeletePages); err != nil {
		return err
	}
	if err := resolveLinks(ctx, tx, wikiID); err != nil {
		return err
	}
	for _, src := range in.UpsertSources {
		if err := upsertSource(ctx, tx, in.WorkspaceID, src); err != nil {
			return err
		}
	}
	if err := dropSources(ctx, tx, in.WorkspaceID, in.DropSources); err != nil {
		return err
	}

	// The revision is what busts UI caches, so it moves only when the rest of
	// the transaction succeeds.
	if _, err := tx.Exec(ctx, `
		UPDATE wikis
		SET revision = revision + 1,
		    updated_at = now(),
		    page_count = (SELECT count(*) FROM pages WHERE wiki_id = $1 AND deleted_at IS NULL)
		WHERE id = $1`, wikiID); err != nil {
		return fmt.Errorf("store: bump revision: %w", err)
	}

	return tx.Commit(ctx)
}

func ensureWiki(ctx context.Context, tx pgx.Tx, workspaceID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM wikis WHERE workspace_id = $1`, workspaceID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: find wiki: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO wikis (workspace_id) VALUES ($1) RETURNING id`,
		workspaceID).Scan(&id); err != nil {
		return "", fmt.Errorf("store: create wiki: %w", err)
	}
	return id, nil
}

func upsertPage(ctx context.Context, tx pgx.Tx, wikiID string, p wiki.Page) error {
	fm, err := json.Marshal(p.Meta)
	if err != nil {
		return fmt.Errorf("store: encode frontmatter for %s: %w", p.Path, err)
	}

	var pageID string
	// created_at is preserved on update: a regenerated page keeps the date it
	// first appeared, which is what the frontmatter contract promises.
	if err := tx.QueryRow(ctx, `
		INSERT INTO pages (wiki_id, path, type, slug, title, frontmatter, body, content_hash, provenance, built_at_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (wiki_id, slug) WHERE deleted_at IS NULL
		DO UPDATE SET
			path = EXCLUDED.path,
			type = EXCLUDED.type,
			title = EXCLUDED.title,
			frontmatter = EXCLUDED.frontmatter,
			body = EXCLUDED.body,
			content_hash = EXCLUDED.content_hash,
			provenance = EXCLUDED.provenance,
			built_at_ref = EXCLUDED.built_at_ref,
			updated_at = now()
		RETURNING id`,
		wikiID, p.Path, string(p.Meta.Type), p.Slug, p.Meta.Title, fm, p.Body,
		p.ContentHash(), provenanceOf(p), nullable(p.Meta.BuiltAtRef),
	).Scan(&pageID); err != nil {
		return fmt.Errorf("store: upsert page %s: %w", p.Path, err)
	}

	// Links are replaced wholesale rather than diffed: a page's outbound links
	// are entirely determined by its current body.
	if _, err := tx.Exec(ctx, `DELETE FROM page_links WHERE from_page_id = $1`, pageID); err != nil {
		return fmt.Errorf("store: clear links for %s: %w", p.Path, err)
	}
	for _, target := range wiki.LinkTargets(p.Body) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO page_links (from_page_id, to_slug, resolved)
			VALUES ($1,$2,false)
			ON CONFLICT (from_page_id, to_slug) DO NOTHING`, pageID, target); err != nil {
			return fmt.Errorf("store: insert link %s -> %s: %w", p.Path, target, err)
		}
	}
	return nil
}

// resolveLinks points every link at its target where one exists.
//
// Unresolved rows are left in place deliberately: a link to a page that does
// not exist is the cheapest and most precise gap signal available, since the
// wiki has explicitly declared it wants that page.
func resolveLinks(ctx context.Context, tx pgx.Tx, wikiID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE page_links l
		SET to_page_id = t.id, resolved = true
		FROM pages t
		WHERE t.wiki_id = $1
		  AND t.deleted_at IS NULL
		  AND t.slug = l.to_slug
		  AND l.from_page_id IN (SELECT id FROM pages WHERE wiki_id = $1)`, wikiID)
	if err != nil {
		return fmt.Errorf("store: resolve links: %w", err)
	}

	// A target that has since been deleted must not stay marked resolved.
	if _, err := tx.Exec(ctx, `
		UPDATE page_links l
		SET to_page_id = NULL, resolved = false
		WHERE l.resolved
		  AND NOT EXISTS (
		      SELECT 1 FROM pages t
		      WHERE t.id = l.to_page_id AND t.deleted_at IS NULL)`); err != nil {
		return fmt.Errorf("store: unresolve dead links: %w", err)
	}
	return nil
}

// softDeletePages marks pages removed without destroying them.
//
// kiln never deletes automatically, and a retention window makes an approved
// deletion recoverable, so this is a timestamp rather than a DELETE.
func softDeletePages(ctx context.Context, tx pgx.Tx, wikiID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pages SET deleted_at = now(), updated_at = now()
		WHERE wiki_id = $1 AND path = ANY($2) AND deleted_at IS NULL`,
		wikiID, paths); err != nil {
		return fmt.Errorf("store: soft delete pages: %w", err)
	}
	return nil
}

func upsertSource(ctx context.Context, tx pgx.Tx, workspaceID string, src diff.SourceRecord) error {
	files, err := json.Marshal(orEmpty(src.FilesWritten))
	if err != nil {
		return fmt.Errorf("store: encode files_written: %w", err)
	}
	blobs, err := json.Marshal(orEmpty(src.BlobKeys))
	if err != nil {
		return fmt.Errorf("store: encode blob_keys: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO sources (workspace_id, key, kind, input_hash, files_written, blob_keys)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (workspace_id, key) DO UPDATE SET
			input_hash = EXCLUDED.input_hash,
			files_written = EXCLUDED.files_written,
			blob_keys = EXCLUDED.blob_keys,
			deleted_at = NULL,
			updated_at = now()`,
		workspaceID, string(src.Key), src.Key.Prefix(), src.InputHash, files, blobs,
	); err != nil {
		return fmt.Errorf("store: upsert source %s: %w", src.Key, err)
	}
	return nil
}

func dropSources(ctx context.Context, tx pgx.Tx, workspaceID string, keys []diff.Key) error {
	if len(keys) == 0 {
		return nil
	}
	raw := make([]string, len(keys))
	for i, k := range keys {
		raw[i] = string(k)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM sources WHERE workspace_id = $1 AND key = ANY($2)`,
		workspaceID, raw); err != nil {
		return fmt.Errorf("store: drop sources: %w", err)
	}
	return nil
}

// RecordRun persists the run summary and its per-unit outcomes.
func (s *JobStore) RecordRun(ctx context.Context, run jobs.RunSummary) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin run record: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var runID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO runs (workspace_id, trigger, ref, status, cost_usd, tokens,
		                  pages_created, pages_updated, pages_deleted, error,
		                  started_at, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now(), now())
		RETURNING id`,
		run.WorkspaceID, run.Trigger, nullable(run.Ref), run.Status, run.CostUSD, run.Tokens,
		run.Created, run.Updated, run.Deleted, nullable(run.Err),
	).Scan(&runID); err != nil {
		return fmt.Errorf("store: insert run: %w", err)
	}

	for _, item := range run.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO run_items (run_id, kind, cache_key, status, cost_usd, turns, error, finished_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7, now())`,
			runID, item.Key.Prefix(), string(item.Key), item.Status,
			item.CostUSD, item.Turns, nullable(item.Err),
		); err != nil {
			return fmt.Errorf("store: insert run item %s: %w", item.Key, err)
		}
	}

	// Spend is ledgered separately so budget windows can be queried without
	// scanning run history.
	if run.CostUSD > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO spend_ledger (workspace_id, run_id, amount_usd) VALUES ($1,$2,$3)`,
			run.WorkspaceID, runID, run.CostUSD); err != nil {
			return fmt.Errorf("store: insert spend: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE wikis SET last_run_id = $1 WHERE workspace_id = $2`,
		runID, run.WorkspaceID); err != nil {
		return fmt.Errorf("store: link run to wiki: %w", err)
	}

	return tx.Commit(ctx)
}

// EnsureWorkspace finds or creates the org/workspace/wiki chain for a slug and
// returns the workspace ID. Used to bootstrap a local build without a UI.
func (s *JobStore) EnsureWorkspace(ctx context.Context, orgSlug, wsSlug, name string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin ensure workspace: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var orgID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO orgs (name, slug) VALUES ($1,$1)
		ON CONFLICT (slug) DO UPDATE SET name = orgs.name
		RETURNING id`, orgSlug).Scan(&orgID); err != nil {
		return "", fmt.Errorf("store: ensure org: %w", err)
	}

	var wsID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO workspaces (org_id, name, slug) VALUES ($1,$2,$3)
		ON CONFLICT (org_id, slug) DO UPDATE SET updated_at = now()
		RETURNING id`, orgID, name, wsSlug).Scan(&wsID); err != nil {
		return "", fmt.Errorf("store: ensure workspace: %w", err)
	}

	if _, err := ensureWiki(ctx, tx, wsID); err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return wsID, nil
}

// nullable maps an empty string to SQL NULL, so absent values are absent rather
// than empty strings.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func provenanceOf(p wiki.Page) string {
	if p.Meta.Provenance != "" {
		return p.Meta.Provenance
	}
	return "source"
}
