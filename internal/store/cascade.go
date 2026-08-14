package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/daiwa-zou/kiln/internal/diff"
)

// Deleting a source is the one path in kiln where content is destroyed without
// a human answering a review first, and the asymmetry is deliberate.
//
// The review queue exists because a source *disappearing from a sync* is
// ambiguous: a half-fetched repository, a connector whose token expired, and a
// genuine removal all look identical at the sync layer, so the pipeline asks
// before it destroys anything. An explicit delete carries none of that
// ambiguity -- someone picked this source out of a list and confirmed -- and
// leaving the wiki asserting things about material the bench no longer holds
// is the worse of the two failures. Asking again, about a deletion that was
// itself the answer, is how a review queue trains people to approve without
// reading.
//
// What "removed" means does not change. Pages are soft-deleted, so the
// retention window still makes a mistake recoverable for as long as it ever
// was, and a page some other live source also claims survives -- flagged for
// regeneration rather than amended in place, because its body still describes
// the departed source.

// cascadeDeleteSources removes the given source keys and the wiki content only
// they produced, inside an already-open transaction. Callers hold the
// transaction so the source rows and whatever else the delete removes (a file
// row, a connector row) commit or roll back together: a file that is gone
// while its pages remain is exactly the state this is meant to prevent.
//
// The returned cascade names the blobs no live source references any more, for
// the caller to delete after the commit.
func cascadeDeleteSources(ctx context.Context, tx pgx.Tx, workspaceID string, removed []diff.Key) (diff.Cascade, error) {
	if len(removed) == 0 {
		return diff.Cascade{}, nil
	}

	// Serialized against a run's import on the same lock: a build writing pages
	// for a source this transaction is dropping would otherwise resurrect it.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('kiln:import:' || $1))`, workspaceID); err != nil {
		return diff.Cascade{}, fmt.Errorf("store: acquire import lock: %w", err)
	}

	sources, err := loadSources(ctx, tx, workspaceID)
	if err != nil {
		return diff.Cascade{}, err
	}
	plan := diff.PlanCascade(sources, expandSections(sources, removed))

	if err := dropSources(ctx, tx, workspaceID, plan.DropSources); err != nil {
		return diff.Cascade{}, err
	}
	// A document's figures leave with the document. They are reachable at a
	// URL of their own, so a source deletion that left them behind would leave
	// the pictures from a deleted PDF still being served.
	figureBlobs, err := dropFiguresForSources(ctx, tx, workspaceID, keyStrings(plan.DropSources))
	if err != nil {
		return diff.Cascade{}, err
	}
	plan.DeleteBlobs = append(plan.DeleteBlobs, figureBlobs...)
	// An open review asking whether this source should go has been answered by
	// the deletion itself. Leaving it would put a question in the queue whose
	// subject no longer exists and whose approval would do nothing.
	if err := closeDeletionReviews(ctx, tx, workspaceID, plan.DropSources); err != nil {
		return diff.Cascade{}, err
	}
	if err := flagRegeneration(ctx, tx, workspaceID, plan.RegeneratePages, plan.DropSources); err != nil {
		return diff.Cascade{}, err
	}

	// No pages to remove means no wiki to touch. Worth the branch: this is the
	// common case for a source that only ever contributed to shared pages, and
	// bumping the revision would invalidate every reader's cache for nothing.
	if len(plan.DeletePages) == 0 {
		return plan, nil
	}

	wikiID, err := ensureWiki(ctx, tx, workspaceID)
	if err != nil {
		return diff.Cascade{}, err
	}
	if err := softDeletePages(ctx, tx, wikiID, plan.DeletePages); err != nil {
		return diff.Cascade{}, err
	}
	// Links into the removed pages become unresolved rather than dangling,
	// which is what puts them back in front of a human as gaps.
	if err := resolveLinks(ctx, tx, wikiID); err != nil {
		return diff.Cascade{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE wikis
		SET revision = revision + 1,
		    updated_at = now(),
		    page_count = (SELECT count(*) FROM pages WHERE wiki_id = $1 AND deleted_at IS NULL)
		WHERE id = $1`, wikiID); err != nil {
		return diff.Cascade{}, fmt.Errorf("store: bump revision: %w", err)
	}

	return plan, nil
}

// expandSections widens a removal to the section units of the documents being
// removed. A section is a span of one file rather than a source in its own
// right, so deleting the file deletes them; without this their rows would
// outlive the document and keep claiming pages nothing can regenerate.
func expandSections(all []diff.SourceRecord, removed []diff.Key) []diff.Key {
	out := make([]diff.Key, 0, len(removed))
	seen := make(map[diff.Key]bool, len(removed))
	for _, k := range removed {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, s := range all {
		if seen[s.Key] {
			continue
		}
		for _, k := range removed {
			if strings.HasPrefix(string(s.Key), string(k)+"#") {
				seen[s.Key] = true
				out = append(out, s.Key)
				break
			}
		}
	}
	return out
}

// flagRegeneration marks the live sources that still claim a shared page the
// cascade spared, so the next run rewrites their prose.
//
// Matching is on the pages themselves rather than on a source list, because
// which surviving source owns a shared page is exactly what files_written
// records and nothing else knows. Sources being dropped are excluded: they are
// about to stop existing, and a flag on a deleted row is a flag nothing reads.
func flagRegeneration(ctx context.Context, tx pgx.Tx, workspaceID string, pages []string, dropping []diff.Key) error {
	if len(pages) == 0 {
		return nil
	}
	drop := keyStrings(dropping)
	if _, err := tx.Exec(ctx, `
		UPDATE sources SET needs_regen = TRUE, updated_at = now()
		WHERE workspace_id = $1
		  AND deleted_at IS NULL
		  AND NOT (key = ANY($3))
		  AND files_written ?| $2`,
		workspaceID, pages, drop); err != nil {
		return fmt.Errorf("store: flag sources for regeneration: %w", err)
	}
	return nil
}

// closeDeletionReviews resolves the open questions a dropped source had
// pending. Recorded as resolved rather than deleted: the queue is a log of what
// was asked and what became of it, and "answered by an explicit delete" is an
// outcome worth being able to read back.
func closeDeletionReviews(ctx context.Context, tx pgx.Tx, workspaceID string, keys []diff.Key) error {
	if len(keys) == 0 {
		return nil
	}
	titles := keyStrings(keys)
	if _, err := tx.Exec(ctx, `
		UPDATE review_items
		SET status = 'resolved', resolved_at = now()
		WHERE workspace_id = $1 AND kind = 'deletion'
		  AND title = ANY($2) AND status IN ('open', 'approved')`,
		workspaceID, titles); err != nil {
		return fmt.Errorf("store: close deletion reviews: %w", err)
	}
	return nil
}

// keyStrings converts source keys for use as a SQL text[] parameter.
func keyStrings(keys []diff.Key) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = string(k)
	}
	return out
}

// sourceKeysForConnector lists the sources a connector's syncs produced. Read
// before the connector row goes, because the schema's ON DELETE CASCADE takes
// these rows with it -- and files_written on the rows about to vanish is the
// only record of which pages they wrote.
func sourceKeysForConnector(ctx context.Context, tx pgx.Tx, workspaceID, connectorID string) ([]diff.Key, error) {
	rows, err := tx.Query(ctx, `
		SELECT key FROM sources
		WHERE workspace_id = $1 AND connector_id = $2 AND deleted_at IS NULL
		ORDER BY key`, workspaceID, connectorID)
	if err != nil {
		return nil, fmt.Errorf("store: load connector sources: %w", err)
	}
	defer rows.Close()

	var out []diff.Key
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("store: scan connector source: %w", err)
		}
		out = append(out, diff.Key(key))
	}
	return out, rows.Err()
}
