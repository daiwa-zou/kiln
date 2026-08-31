package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Deleting a page is two facts recorded together, and it needs both.
//
// The soft delete is what a reader sees: the page leaves every listing, search
// result and link target immediately, and stays recoverable for the retention
// window like any other removed page. The suppression is what makes it stick.
// A page is derived from a source that is still present and still hashes the
// same, so without a durable record the very next build would write it back --
// a delete that a rebuild undoes is a delay, not a delete.
//
// This is the same shape as page_corrections and for the same reason: pages are
// never hand-edited, so anything a human decides about a page has to live
// outside it. A correction says "write this differently". A suppression says
// "do not write this at all".

// DeletedPage is one suppressed page, for listing what a bench has removed.
type DeletedPage struct {
	Slug string
	// Path and Title are the page's last known ones, empty once the soft-deleted
	// row has been swept and only the suppression remains.
	Path      string
	Title     string
	Reason    string
	DeletedAt time.Time
	// Live reports whether the page has since been written again -- which
	// happens only if the suppression was lifted, so it is a UI hint rather
	// than an ordinary state.
	Live bool
}

// DeletePage removes a page and records that it should stay removed.
//
// Addressed by slug or path, like corrections, because that is what a URL and a
// link both give you. Returns ErrNotFound when the workspace has no live page
// by that name, so deleting twice reads as a missing page rather than silently
// suppressing a name that never existed -- which would be a way to pre-empt
// pages the wiki has not written yet.
func (s *WikiStore) DeletePage(ctx context.Context, workspaceID, pageRef, reason, deletedBy string) (slug string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin delete page: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var wikiID string
	err = tx.QueryRow(ctx, `
		SELECT p.slug, p.wiki_id
		FROM pages p
		JOIN wikis w ON w.id = p.wiki_id
		WHERE w.workspace_id = $1 AND p.deleted_at IS NULL
		  AND (p.slug = $2 OR p.path = $2)
		LIMIT 1`, workspaceID, pageRef).Scan(&slug, &wikiID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: find page to delete: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE pages SET deleted_at = now(), updated_at = now()
		WHERE wiki_id = $1 AND slug = $2 AND deleted_at IS NULL`, wikiID, slug); err != nil {
		return "", fmt.Errorf("store: soft delete page: %w", err)
	}

	// Re-deleting a page whose suppression was lifted refreshes the reason and
	// the attribution rather than failing: the second decision is the current
	// one.
	if _, err := tx.Exec(ctx, `
		INSERT INTO page_suppressions (workspace_id, slug, reason, deleted_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workspace_id, slug) DO UPDATE SET
			reason = EXCLUDED.reason,
			deleted_by = EXCLUDED.deleted_by,
			created_at = now()`,
		workspaceID, slug, reason, nullable(deletedBy)); err != nil {
		return "", fmt.Errorf("store: suppress page: %w", err)
	}

	// Links into the page become unresolved, which is what turns them back into
	// the wiki's own gap signal rather than links to nothing.
	if err := resolveLinks(ctx, tx, wikiID); err != nil {
		return "", err
	}
	if err := bumpRevision(ctx, tx, wikiID); err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit delete page: %w", err)
	}
	return slug, nil
}

// RestorePage lifts a suppression so the next build may write the page again,
// and brings back the soft-deleted row if it is still inside the retention
// window.
//
// Two outcomes worth telling apart, which is what restored reports: the page
// came back immediately, or the suppression was lifted but the row had already
// been swept and only a rebuild can produce it again.
func (s *WikiStore) RestorePage(ctx context.Context, workspaceID, slug string) (restored bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: begin restore page: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	tag, err := tx.Exec(ctx,
		`DELETE FROM page_suppressions WHERE workspace_id = $1 AND slug = $2`, workspaceID, slug)
	if err != nil {
		return false, fmt.Errorf("store: lift suppression: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, ErrNotFound
	}

	// Only one row can come back: the live-page index is unique on
	// (wiki_id, slug), so restoring two soft-deleted generations of the same
	// page would violate it. The most recent is the one anyone means.
	var wikiID string
	err = tx.QueryRow(ctx, `
		UPDATE pages SET deleted_at = NULL, updated_at = now()
		WHERE id = (
			SELECT p.id FROM pages p
			JOIN wikis w ON w.id = p.wiki_id
			WHERE w.workspace_id = $1 AND p.slug = $2 AND p.deleted_at IS NOT NULL
			ORDER BY p.deleted_at DESC
			LIMIT 1
		)
		RETURNING wiki_id`, workspaceID, slug).Scan(&wikiID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Swept already. The suppression is lifted, so a rebuild will write it.
		restored = false
	case err != nil:
		return false, fmt.Errorf("store: restore page: %w", err)
	default:
		restored = true
		if err := resolveLinks(ctx, tx, wikiID); err != nil {
			return false, err
		}
		if err := bumpRevision(ctx, tx, wikiID); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit restore page: %w", err)
	}
	return restored, nil
}

// ListDeletedPages returns what this bench has deleted, newest first.
//
// Left-joined against pages so a suppression outlives the swept row it refers
// to: the entry keeps its name and reason even when there is no longer a page
// to show a title for.
func (s *WikiStore) ListDeletedPages(ctx context.Context, workspaceID string) ([]DeletedPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.slug, s.reason, s.created_at,
		       coalesce(p.path, ''), coalesce(p.title, ''),
		       coalesce(p.deleted_at IS NULL, false)
		FROM page_suppressions s
		LEFT JOIN LATERAL (
			SELECT p.path, p.title, p.deleted_at
			FROM pages p
			JOIN wikis w ON w.id = p.wiki_id
			WHERE w.workspace_id = s.workspace_id AND p.slug = s.slug
			ORDER BY p.updated_at DESC
			LIMIT 1
		) p ON true
		WHERE s.workspace_id = $1
		ORDER BY s.created_at DESC`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: list deleted pages: %w", err)
	}
	defer rows.Close()

	out := []DeletedPage{}
	for rows.Next() {
		var d DeletedPage
		if err := rows.Scan(&d.Slug, &d.Reason, &d.DeletedAt, &d.Path, &d.Title, &d.Live); err != nil {
			return nil, fmt.Errorf("store: scan deleted page: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// bumpRevision moves the counter readers cache against. Shared by every path
// that changes what pages exist.
func bumpRevision(ctx context.Context, tx pgx.Tx, wikiID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE wikis
		SET revision = revision + 1,
		    updated_at = now(),
		    page_count = (SELECT count(*) FROM pages WHERE wiki_id = $1 AND deleted_at IS NULL)
		WHERE id = $1`, wikiID); err != nil {
		return fmt.Errorf("store: bump revision: %w", err)
	}
	return nil
}
