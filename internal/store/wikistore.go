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

// WikiStore implements jobs.Store against Postgres.
//
// The interface is declared by the consumer (internal/jobs), so the SQL lives
// here and the pipeline stays testable against an in-memory fake.
type WikiStore struct {
	pool *pgxpool.Pool

	// onLinkResolve, when set, is told how many page_links rows each import
	// rewrote while resolving and unresolving. It exists so a test can prove
	// resolveLinks writes only the rows whose value changes; production code
	// leaves it nil.
	onLinkResolve func(resolved, unresolved int64)
}

// NewWikiStore wraps a pool.
func NewWikiStore(pool *pgxpool.Pool) *WikiStore { return &WikiStore{pool: pool} }

// Pool exposes the underlying pool, for callers (and tests) that need SQL the
// store does not wrap.
func (s *WikiStore) Pool() *pgxpool.Pool { return s.pool }

var _ jobs.Store = (*WikiStore)(nil)

// queryer is the read half that *pgxpool.Pool and pgx.Tx have in common, so a
// loader can serve both a plain read and a step inside a transaction. Cascade
// deletion needs the second: it plans from the same rows it is about to drop,
// and a read outside the transaction could plan against a state the drop no
// longer matches.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LoadSources returns the live source records for a workspace. These are the
// baseline for both incremental skip and cascade deletion.
func (s *WikiStore) LoadSources(ctx context.Context, workspaceID string) ([]diff.SourceRecord, error) {
	return loadSources(ctx, s.pool, workspaceID)
}

func loadSources(ctx context.Context, q queryer, workspaceID string) ([]diff.SourceRecord, error) {
	rows, err := q.Query(ctx, `
		SELECT key, input_hash, files_written, blob_keys, needs_regen
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
		if err := rows.Scan(&key, &rec.InputHash, &files, &blob, &rec.NeedsRegen); err != nil {
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

// LoadPages returns live pages for a workspace with their bodies. Only callers
// that read the prose want this; a caller that needs to know what pages exist
// -- slugs, titles, types, frontmatter -- should use LoadPageMeta, which does not
// ship every body on the bench across the wire.
func (s *WikiStore) LoadPages(ctx context.Context, workspaceID string) ([]wiki.Page, error) {
	return s.queryPages(ctx, `
		SELECT p.path, p.slug, p.type, p.title, p.frontmatter, p.body
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		ORDER BY p.path`, workspaceID)
}

// LoadPageMeta returns live pages for a workspace without their bodies: path,
// slug, type, title and frontmatter. The body column is never selected, so the
// cost is independent of how much prose the bench holds.
//
// This is what an incremental build needs to resolve wikilinks, preserve each
// page's created date and rebuild the index and overview. Page.Body is empty on
// everything it returns.
func (s *WikiStore) LoadPageMeta(ctx context.Context, workspaceID string) ([]wiki.Page, error) {
	return s.queryPages(ctx, `
		SELECT p.path, p.slug, p.type, p.title, p.frontmatter, ''::text
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		ORDER BY p.path`, workspaceID)
}

// LoadPagesBySlug returns the named live pages with their bodies, for a caller
// that has already chosen which pages it will read. Slugs that match nothing are
// absent from the result rather than an error.
func (s *WikiStore) LoadPagesBySlug(ctx context.Context, workspaceID string, slugs []string) ([]wiki.Page, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	return s.queryPages(ctx, `
		SELECT p.path, p.slug, p.type, p.title, p.frontmatter, p.body
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL AND p.slug = ANY($2)
		ORDER BY p.path`, workspaceID, slugs)
}

// LinkingSlugs returns the slugs of live pages that contain a wikilink to the
// given slug, answered from the materialized link table rather than by scanning
// bodies. The target is normalized the way links are on the way in, so
// "Retry-Policy", "concepts/retry-policy" and "retry-policy#backoff" all find
// the same pages, and a link to a page that does not exist yet is found too --
// which is how a gap's own referrers are located.
func (s *WikiStore) LinkingSlugs(ctx context.Context, workspaceID, slug string) ([]string, error) {
	target := wiki.NormalizeLinkTarget(slug)
	if target == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT p.slug
		FROM page_links l
		JOIN pages p ON p.id = l.from_page_id
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND l.to_slug = $2 AND p.deleted_at IS NULL
		ORDER BY p.slug`, workspaceID, target)
	if err != nil {
		return nil, fmt.Errorf("store: pages linking to %s: %w", target, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var linker string
		if err := rows.Scan(&linker); err != nil {
			return nil, fmt.Errorf("store: scan linking page: %w", err)
		}
		out = append(out, linker)
	}
	return out, rows.Err()
}

// queryPages runs a six-column page query (path, slug, type, title,
// frontmatter, body) and decodes the rows.
func (s *WikiStore) queryPages(ctx context.Context, sql string, args ...any) ([]wiki.Page, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: load pages: %w", err)
	}
	defer rows.Close()

	var out []wiki.Page
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// scanPage decodes one row of the six-column page shape (path, slug, type,
// title, frontmatter, body) shared by every full page loader. A scan error is
// returned wrapped, so a caller reading a single row can still match
// pgx.ErrNoRows with errors.Is.
func scanPage(row pgx.Row) (wiki.Page, error) {
	var (
		p        wiki.Page
		pageType string
		fm       []byte
	)
	if err := row.Scan(&p.Path, &p.Slug, &pageType, &p.Meta.Title, &fm, &p.Body); err != nil {
		return wiki.Page{}, fmt.Errorf("store: scan page: %w", err)
	}
	p.Meta.Type = wiki.PageType(pageType)

	// Frontmatter round-trips through jsonb so tags, related, and sources
	// survive without a second schema to keep in step.
	if len(fm) > 0 {
		if err := json.Unmarshal(fm, &p.Meta); err != nil {
			return wiki.Page{}, fmt.Errorf("store: decode frontmatter for %s: %w", p.Path, err)
		}
	}
	return p, nil
}

// LoadSteering returns the workspace's purpose and schema documents plus any
// active per-page corrections.
//
// Corrections are what let human knowledge survive regeneration: pages are
// never hand-edited, so a correction lives outside the page and is re-injected
// into every prompt that rebuilds it.
func (s *WikiStore) LoadSteering(ctx context.Context, workspaceID string) (jobs.Steering, error) {
	out := jobs.Steering{
		Corrections: map[string][]string{},
		Suppressed:  map[string]string{},
	}

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
	if err := corr.Err(); err != nil {
		return out, err
	}

	// Pages a human deleted. Loaded with the rest of the steering because it is
	// the same kind of fact -- a standing decision about this wiki that has to
	// survive every regeneration -- and because the generate step needs it to
	// avoid paying to write a page that import would then discard.
	sup, err := s.pool.Query(ctx,
		`SELECT slug, reason FROM page_suppressions WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return out, fmt.Errorf("store: load page suppressions: %w", err)
	}
	defer sup.Close()

	for sup.Next() {
		var slug, reason string
		if err := sup.Scan(&slug, &reason); err != nil {
			return out, fmt.Errorf("store: scan page suppression: %w", err)
		}
		out.Suppressed[slug] = reason
	}
	return out, sup.Err()
}

// Import commits one run's output.
//
// Everything lands in a single transaction on purpose: pages, links, sources,
// and the revision bump have to agree, or a crash mid-write leaves a wiki whose
// index describes pages that were never stored.
func (s *WikiStore) Import(ctx context.Context, in jobs.ImportRequest) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin import: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// One import per workspace at a time. Two concurrent imports would
	// interleave read-modify-write on pages, links, and artifacts with
	// last-writer-wins results; the transaction-scoped advisory lock
	// serializes them and releases automatically on commit or rollback. The
	// explicit deletes take the same lock, which is what lets the fence below
	// trust what it reads.
	if err := lockImports(ctx, tx, in.WorkspaceID); err != nil {
		return err
	}

	// A run is long and an explicit delete is not: by the time this lands, the
	// document or connector a source came from may be gone. Those sources, and
	// the pages only they wrote, are left out so the delete stays a delete; the
	// rest of the run still lands.
	fenced, err := fenceImport(ctx, tx, in)
	if err != nil {
		return err
	}
	in = fenced.req
	if fenced.pagesDropped {
		// The index and overview were built from a page set that included
		// pages that are not being stored. Keeping the previous ones leaves
		// them one run stale; writing these would link to pages that do not
		// exist.
		in.Index, in.Overview = "", ""
	}

	wikiID, err := ensureWiki(ctx, tx, in.WorkspaceID)
	if err != nil {
		return err
	}

	if err := upsertPages(ctx, tx, wikiID, in.UpsertPages); err != nil {
		return err
	}
	if err := softDeletePages(ctx, tx, wikiID, in.SoftDeletePages); err != nil {
		return err
	}
	resolved, unresolved, err := resolveLinksCounted(ctx, tx, wikiID)
	if err != nil {
		return err
	}
	if s.onLinkResolve != nil {
		s.onLinkResolve(resolved, unresolved)
	}
	for _, src := range in.UpsertSources {
		if err := upsertSource(ctx, tx, in.WorkspaceID, src); err != nil {
			return err
		}
	}
	if err := flagSourcesForRegeneration(ctx, tx, in.WorkspaceID, fenced.regen); err != nil {
		return err
	}
	if err := dropSources(ctx, tx, in.WorkspaceID, in.DropSources); err != nil {
		return err
	}
	// A dropped document's pictures leave with it, as they do for an explicit
	// delete; without this the approved-deletion path left figure rows (and
	// their bytes) behind for good.
	figureBlobs, err := dropFiguresForSources(ctx, tx, in.WorkspaceID, keyStrings(in.DropSources))
	if err != nil {
		return err
	}
	if err := enqueueBlobDeletes(ctx, tx, append(figureBlobs, in.DeleteBlobs...)); err != nil {
		return err
	}

	if err := writeArtifacts(ctx, tx, wikiID, in); err != nil {
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

// RenameDocuments records what ingest worked out each uploaded document is
// called. Keyed on path, which is what the row is unique on; a file deleted
// between the sync and this call simply matches nothing.
//
// Only writes where the name actually differs, so a rebuild that learned
// nothing new leaves updated_at alone -- the Ingest list sorts and reports on
// that column, and touching every row on every build would make an unchanged
// bench look freshly edited.
func (s *WikiStore) RenameDocuments(ctx context.Context, workspaceID string, names map[string]string) error {
	for path, name := range names {
		if name == "" {
			continue
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE workspace_files
			SET display_name = $3, updated_at = now()
			WHERE workspace_id = $1 AND path = $2 AND display_name IS DISTINCT FROM $3`,
			workspaceID, path, name); err != nil {
			return fmt.Errorf("store: rename document %q: %w", path, err)
		}
	}
	return nil
}

// writeArtifacts persists the derived index, overview, and log.
//
// The index and overview are replaced wholesale: they are pure functions of the
// current page set, so the newest run's version is the only correct one. The log
// is appended, because it is the record of how the wiki reached its current
// state and rewriting it would erase that.
func writeArtifacts(ctx context.Context, tx pgx.Tx, wikiID string, in jobs.ImportRequest) error {
	// Fixed order, not a map: concurrent imports taking these row locks in
	// randomized order is a deadlock waiting for traffic.
	for _, artifact := range []struct{ kind, body string }{
		{"index", in.Index},
		{"overview", in.Overview},
	} {
		kind, body := artifact.kind, artifact.body
		if body == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wiki_artifacts (wiki_id, kind, body) VALUES ($1,$2,$3)
			ON CONFLICT (wiki_id, kind) DO UPDATE
			SET body = EXCLUDED.body, updated_at = now()`,
			wikiID, kind, body); err != nil {
			return fmt.Errorf("store: write %s artifact: %w", kind, err)
		}
	}

	if in.LogEntry == "" {
		return nil
	}
	// Append-only: the new entry is concatenated onto whatever is already
	// there, seeded with a header the first time.
	if _, err := tx.Exec(ctx, `
		INSERT INTO wiki_artifacts (wiki_id, kind, body)
		VALUES ($1, 'log', $2 || $3)
		ON CONFLICT (wiki_id, kind) DO UPDATE
		SET body = wiki_artifacts.body || E'\n' || $3, updated_at = now()`,
		wikiID, wiki.LogHeader+"\n\n", in.LogEntry); err != nil {
		return fmt.Errorf("store: append log: %w", err)
	}
	return nil
}

// LoadArtifact returns a derived artifact, or empty when it has not been built.
func (s *WikiStore) LoadArtifact(ctx context.Context, workspaceID, kind string) (string, error) {
	var body string
	err := s.pool.QueryRow(ctx, `
		SELECT a.body FROM wiki_artifacts a
		JOIN wikis w ON w.id = a.wiki_id
		WHERE w.workspace_id = $1 AND a.kind = $2`, workspaceID, kind).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: load %s artifact: %w", kind, err)
	}
	return body, nil
}

func ensureWiki(ctx context.Context, tx pgx.Tx, workspaceID string) (string, error) {
	// A single upsert rather than check-then-insert: two concurrent first
	// builds would otherwise race to the INSERT and one would fail on the
	// unique constraint. The no-op DO UPDATE exists so RETURNING yields the id
	// on the conflict path too.
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO wikis (workspace_id) VALUES ($1)
		ON CONFLICT (workspace_id) DO UPDATE SET workspace_id = EXCLUDED.workspace_id
		RETURNING id`, workspaceID).Scan(&id); err != nil {
		return "", fmt.Errorf("store: ensure wiki: %w", err)
	}
	return id, nil
}

// upsertBatchSize bounds how many page upserts travel in one pgx batch, so a
// build of thousands of pages does not queue every body in memory at once.
const upsertBatchSize = 200

// upsertPages writes the run's pages and replaces their outbound links.
//
// This runs inside the import's advisory-locked transaction, so every round
// trip is time other writers wait. Page upserts therefore go out in pgx
// batches (the page ids come back in order), and the links of all pages are
// replaced with one DELETE and one INSERT rather than a pair per page plus one
// statement per link.
func upsertPages(ctx context.Context, tx pgx.Tx, wikiID string, pages []wiki.Page) error {
	if len(pages) == 0 {
		return nil
	}

	// created_at is preserved on update: a regenerated page keeps the date it
	// first appeared, which is what the frontmatter contract promises.
	const upsertSQL = `
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
		RETURNING id`

	// Two pages in one run can share a slug and so land on one row; the later
	// page wins, links included, exactly as when each page was written in turn.
	// Keyed by page id, kept in first-seen order for deterministic SQL.
	var (
		order   []string
		targets = map[string][]string{}
	)
	for start := 0; start < len(pages); start += upsertBatchSize {
		chunk := pages[start:min(start+upsertBatchSize, len(pages))]

		batch := &pgx.Batch{}
		for _, p := range chunk {
			fm, err := json.Marshal(p.Meta)
			if err != nil {
				return fmt.Errorf("store: encode frontmatter for %s: %w", p.Path, err)
			}
			batch.Queue(upsertSQL,
				wikiID, p.Path, string(p.Meta.Type), p.Slug, p.Meta.Title, fm, p.Body,
				p.ContentHash(), provenanceOf(p), nullable(p.Meta.BuiltAtRef))
		}

		ids, err := sendUpserts(ctx, tx, batch, chunk)
		if err != nil {
			return err
		}
		for i, id := range ids {
			if _, seen := targets[id]; !seen {
				order = append(order, id)
			}
			targets[id] = wiki.LinkTargets(chunk[i].Body)
		}
	}

	// Links are replaced wholesale rather than diffed: a page's outbound links
	// are entirely determined by its current body.
	if _, err := tx.Exec(ctx, `DELETE FROM page_links WHERE from_page_id = ANY($1::uuid[])`, order); err != nil {
		return fmt.Errorf("store: clear links: %w", err)
	}

	var fromIDs, toSlugs []string
	for _, id := range order {
		for _, target := range targets[id] {
			fromIDs = append(fromIDs, id)
			toSlugs = append(toSlugs, target)
		}
	}
	if len(fromIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO page_links (from_page_id, to_slug, resolved)
		SELECT f, t, false FROM unnest($1::uuid[], $2::text[]) AS u(f, t)
		ON CONFLICT (from_page_id, to_slug) DO NOTHING`, fromIDs, toSlugs); err != nil {
		return fmt.Errorf("store: insert links: %w", err)
	}
	return nil
}

// sendUpserts runs one batch of page upserts and returns the page ids in the
// order the pages were queued.
func sendUpserts(ctx context.Context, tx pgx.Tx, batch *pgx.Batch, pages []wiki.Page) ([]string, error) {
	br := tx.SendBatch(ctx, batch)
	ids := make([]string, 0, len(pages))
	for _, p := range pages {
		var id string
		if err := br.QueryRow().Scan(&id); err != nil {
			_ = br.Close()
			return nil, fmt.Errorf("store: upsert page %s: %w", p.Path, err)
		}
		ids = append(ids, id)
	}
	if err := br.Close(); err != nil {
		return nil, fmt.Errorf("store: upsert pages: %w", err)
	}
	return ids, nil
}

// resolveLinks points every link at its target where one exists.
func resolveLinks(ctx context.Context, tx pgx.Tx, wikiID string) error {
	_, _, err := resolveLinksCounted(ctx, tx, wikiID)
	return err
}

// resolveLinksCounted is resolveLinks, also reporting how many rows each of its
// two statements rewrote.
//
// Unresolved rows are left in place deliberately: a link to a page that does
// not exist is the cheapest and most precise gap signal available, since the
// wiki has explicitly declared it wants that page.
//
// Both statements write only rows whose value would change. Postgres writes a
// new tuple version for every row an UPDATE matches, whether or not any column
// differs, so without the predicate each import would rewrite every link in the
// wiki -- WAL, bloat, and vacuum work proportional to the whole graph, inside
// the advisory-locked transaction, for an import that touched one page.
func resolveLinksCounted(ctx context.Context, tx pgx.Tx, wikiID string) (resolved, unresolved int64, err error) {
	tag, err := tx.Exec(ctx, `
		UPDATE page_links l
		SET to_page_id = t.id, resolved = true
		FROM pages t
		WHERE t.wiki_id = $1
		  AND t.deleted_at IS NULL
		  AND t.slug = l.to_slug
		  AND l.from_page_id IN (SELECT id FROM pages WHERE wiki_id = $1)
		  AND (NOT l.resolved OR l.to_page_id IS DISTINCT FROM t.id)`, wikiID)
	if err != nil {
		return 0, 0, fmt.Errorf("store: resolve links: %w", err)
	}
	resolved = tag.RowsAffected()

	// A target that has since been deleted must not stay marked resolved.
	// Scoped to this wiki's own links: without the wiki_id predicate this
	// statement would scan and lock page_links for every tenant on every import.
	// Already restricted to resolved rows, so every match is a real change.
	tag, err = tx.Exec(ctx, `
		UPDATE page_links l
		SET to_page_id = NULL, resolved = false
		WHERE l.resolved
		  AND l.from_page_id IN (SELECT id FROM pages WHERE wiki_id = $1)
		  AND NOT EXISTS (
		      SELECT 1 FROM pages t
		      WHERE t.id = l.to_page_id AND t.deleted_at IS NULL)`, wikiID)
	if err != nil {
		return 0, 0, fmt.Errorf("store: unresolve dead links: %w", err)
	}
	return resolved, tag.RowsAffected(), nil
}

// softDeletePages marks pages removed without destroying them.
//
// kiln never deletes automatically, and a retention window makes an approved
// deletion recoverable, so this is a timestamp rather than a DELETE.
//
// Matching is by slug, the page's database identity, as well as by the
// recorded path: a page whose type changed moved directories, and its old
// source record still names the old path. A path-only match would silently
// delete nothing.
func softDeletePages(ctx context.Context, tx pgx.Tx, wikiID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	slugs := make([]string, 0, len(paths))
	for _, p := range paths {
		slugs = append(slugs, wiki.SlugFromPath(p))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pages SET deleted_at = now(), updated_at = now()
		WHERE wiki_id = $1 AND (path = ANY($2) OR slug = ANY($3)) AND deleted_at IS NULL`,
		wikiID, paths, slugs); err != nil {
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

	// Attribution is written exactly as this run produced it — including
	// NULL for CLI builds — so a connector swap or a hand rebuild never
	// leaves a source pointing at a connector that did not sync it.
	//
	// needs_regen clears here and only here: this row is being written because
	// the unit just regenerated, which is precisely the work the flag was
	// asking for. A unit that failed never reaches this statement, so its flag
	// survives and the next run tries again.
	if _, err := tx.Exec(ctx, `
		INSERT INTO sources (workspace_id, connector_id, key, kind, input_hash, files_written, blob_keys)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (workspace_id, key) DO UPDATE SET
			connector_id = EXCLUDED.connector_id,
			input_hash = EXCLUDED.input_hash,
			files_written = EXCLUDED.files_written,
			blob_keys = EXCLUDED.blob_keys,
			needs_regen = FALSE,
			deleted_at = NULL,
			updated_at = now()`,
		workspaceID, nullable(src.ConnectorID), string(src.Key), src.Key.Prefix(),
		src.InputHash, files, blobs,
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
func (s *WikiStore) RecordRun(ctx context.Context, run jobs.RunSummary) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin run record: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// A queued run already has its row: the worker passes the database id as
	// RunID, and the summary finishes that row in place. A CLI run id
	// ("run-<hex>") is not a UUID, so it takes the insert path below.
	//
	// When the context carries a worker's claim, the update is fenced to it: the
	// row must still be running and still ours. Without that, a worker whose
	// claim lapsed would mark the run succeeded while the worker that took it
	// over is mid-build, which frees the workspace's one active-run slot and
	// lets a third run start alongside it. A fenced miss must not fall through
	// to the insert below either -- the row exists, it just is not ours.
	var runID string
	worker, fenced := claimedBy(ctx)
	if isUUID(run.RunID) {
		var owner any // nil leaves the update unfenced, as the CLI path expects
		if fenced {
			owner = worker
		}
		err := tx.QueryRow(ctx, `
			UPDATE runs SET status = $2, ref = COALESCE($3, ref), cost_usd = $4,
			                tokens = $5, pages_created = $6, pages_updated = $7,
			                pages_deleted = $8, error = $9, finished_at = now()
			WHERE id = $1
			  AND ($10::text IS NULL OR (claimed_by = $10::text AND status = 'running'))
			RETURNING id`,
			run.RunID, run.Status, nullable(run.Ref), run.CostUSD, run.Tokens,
			run.Created, run.Updated, run.Deleted, nullable(run.Err), owner,
		).Scan(&runID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("store: finish run %s: %w", run.RunID, err)
		}
		if runID == "" && fenced {
			return s.recordLostRun(ctx, tx, run)
		}
	}
	if runID == "" {
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
	}

	// Upsert, not insert: a queued run seeded its plan as pending items before
	// generating anything, so most of these rows already exist. This settles
	// them, and remains correct for a CLI run whose items are all new.
	for _, item := range run.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO run_items (run_id, kind, cache_key, status, cost_usd, est_cost_usd, turns, tokens, error, finished_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now())
			ON CONFLICT (run_id, cache_key) DO UPDATE SET
			    status       = EXCLUDED.status,
			    cost_usd     = EXCLUDED.cost_usd,
			    est_cost_usd = COALESCE(EXCLUDED.est_cost_usd, run_items.est_cost_usd),
			    turns        = EXCLUDED.turns,
			    tokens       = EXCLUDED.tokens,
			    error        = EXCLUDED.error,
			    finished_at  = EXCLUDED.finished_at`,
			runID, item.Key.Prefix(), string(item.Key), item.Status,
			item.CostUSD, nullableFloat(item.EstCostUSD), item.Turns, item.Tokens, nullable(item.Err),
		); err != nil {
			return fmt.Errorf("store: record run item %s: %w", item.Key, err)
		}
	}

	// Anything still pending or running when the run ends was planned and never
	// reached -- held back by the page cap, or dropped when the run stopped
	// early. Left alone it would read as work in progress on a finished run,
	// which is worse than saying nothing: it is a progress bar that never
	// completes. The units keep their stale hashes and are picked up next run.
	if _, err := tx.Exec(ctx, `
		UPDATE run_items SET status = $2, finished_at = now()
		WHERE run_id = $1 AND status IN ('pending', 'running')`,
		runID, jobs.StatusDeferred,
	); err != nil {
		return fmt.Errorf("store: settle unreached run items: %w", err)
	}

	// Agent-raised review flags land with the run that raised them.
	// Deduplicated against open items so a model that keeps flagging the same
	// contradiction across runs asks the question once, not once per run.
	for _, rv := range run.Reviews {
		// The unit rides in its own column rather than being appended to the
		// detail: which source raised a question is a fact about the review,
		// and a reader should meet the document rather than its cache key.
		if _, err := tx.Exec(ctx, `
			INSERT INTO review_items (workspace_id, run_id, kind, title, detail, unit)
			SELECT $1, $2, $3, $4, $5, $6
			WHERE NOT EXISTS (
			    SELECT 1 FROM review_items
			    WHERE workspace_id = $1 AND kind = $3 AND title = $4 AND status = 'open')`,
			run.WorkspaceID, runID, rv.Kind, rv.Title, rv.Detail, rv.Unit); err != nil {
			return fmt.Errorf("store: insert review item: %w", err)
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

// recordLostRun is what a fenced RecordRun does when the run is no longer the
// caller's. Nothing about the run, its items, its reviews or the wiki's last-run
// pointer is touched -- those now belong to whoever holds the claim -- but the
// spend is still ledgered. The model calls were made and billed, and the other
// worker will ledger only its own; dropping this one would leave every budget
// window undercounting exactly when a build is being paid for twice.
func (s *WikiStore) recordLostRun(ctx context.Context, tx pgx.Tx, run jobs.RunSummary) error {
	if run.CostUSD > 0 {
		// Guarded by existence: the run may have been swept since, and the
		// ledger's reference to it must not dangle.
		if _, err := tx.Exec(ctx, `
			INSERT INTO spend_ledger (workspace_id, run_id, amount_usd)
			SELECT $1, id, $3 FROM runs WHERE id = $2`,
			run.WorkspaceID, run.RunID, run.CostUSD); err != nil {
			return fmt.Errorf("store: insert spend for lost run: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return ErrClaimLost
}

// EnsureWorkspace finds or creates the org/workspace/wiki chain for a slug and
// returns the workspace ID. Used to bootstrap a local build without a UI.
func (s *WikiStore) EnsureWorkspace(ctx context.Context, orgSlug, wsSlug, name string) (string, error) {
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

// isUUID reports whether s is a canonical 8-4-4-4-12 UUID, which is how
// RecordRun tells a queued run's database id from a CLI-minted run id.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// nullable maps an empty string to SQL NULL, so absent values are absent rather
// than empty strings.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableFloat maps zero to SQL NULL: an estimate of zero means "none was
// made", and NULL keeps it out of averages.
func nullableFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
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
