package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/daiwa-zou/kiln/internal/jobs"
)

// FigureRow is one picture recovered from a source document.
type FigureRow struct {
	ID          string
	WorkspaceID string
	// SourceKey is the unit key of the document this came out of, the same key
	// the pipeline and the deletion cascade use.
	SourceKey string
	// Ref is the extractor's within-document identifier, for tracing a figure
	// back to where in the document it sat.
	Ref         string
	BlobKey     string
	ContentType string
	Width       int
	Height      int
	SizeBytes   int64
	Page        int
	Caption     string
	Ordinal     int
	SHA256      string
}

// asRecord converts a stored row to the pipeline's view of a figure.
//
// Two types on purpose. The pipeline needs a figure it can offer to a model;
// the API needs one it can stream bytes for. They overlap but are not the same
// question, and jobs cannot import store -- store already imports jobs -- so
// the conversion has to live on this side regardless.
func (f FigureRow) asRecord() jobs.FigureRecord {
	return jobs.FigureRecord{
		ID: f.ID, SourceKey: f.SourceKey, Ref: f.Ref, BlobKey: f.BlobKey,
		ContentType: f.ContentType, Width: f.Width, Height: f.Height,
		SizeBytes: f.SizeBytes, Page: f.Page, Caption: f.Caption,
		Ordinal: f.Ordinal, SHA256: f.SHA256,
	}
}

// ReplaceFigures makes the recorded figures for one source exactly those
// given, and returns the blob keys of figures that are no longer referenced so
// the caller can delete them after the transaction commits.
//
// Replace rather than insert, because a document's figures are a property of
// its current bytes: re-extracting an edited PDF whose third chart was removed
// must leave that chart recorded nowhere. Rows that survive keep their ids --
// matched on sha256, the identity of a picture -- so a page that already
// references a figure keeps working across a rebuild that did not change it.
func (s *WikiStore) ReplaceFigures(ctx context.Context, workspaceID, sourceKey string, figs []jobs.FigureRecord) (orphaned []string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin replace figures: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	keep := make(map[string]bool, len(figs))
	for _, f := range figs {
		keep[f.SHA256] = true
	}

	rows, err := tx.Query(ctx, `
		SELECT sha256, blob_key FROM figures
		WHERE workspace_id = $1 AND source_key = $2`, workspaceID, sourceKey)
	if err != nil {
		return nil, fmt.Errorf("store: load existing figures: %w", err)
	}
	existing := map[string]string{}
	for rows.Next() {
		var sha, blobKey string
		if err := rows.Scan(&sha, &blobKey); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan existing figure: %w", err)
		}
		existing[sha] = blobKey
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read existing figures: %w", err)
	}

	var candidates []string
	for sha, blobKey := range existing {
		if !keep[sha] {
			candidates = append(candidates, blobKey)
		}
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM figures
		WHERE workspace_id = $1 AND source_key = $2 AND NOT (sha256 = ANY($3))`,
		workspaceID, sourceKey, shaList(figs)); err != nil {
		return nil, fmt.Errorf("store: prune figures: %w", err)
	}

	// Asked after the delete, so the answer accounts for the rows just
	// removed. Blob keys are content digests, which means two documents that
	// contain the same chart -- or the same document ingested under two names
	// -- share one blob; dropping it because one of them stopped referencing
	// it would blank the picture on the other's page.
	orphaned, err = unreferencedBlobs(ctx, tx, candidates)
	if err != nil {
		return nil, err
	}

	for _, f := range figs {
		// Everything except identity is refreshed: a document edit can move a
		// figure to another page or give it a caption it did not have, without
		// changing the picture itself.
		if _, err := tx.Exec(ctx, `
			INSERT INTO figures
				(workspace_id, source_key, ref, blob_key, content_type,
				 width, height, size_bytes, page, caption, ordinal, sha256)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (workspace_id, source_key, sha256) DO UPDATE SET
				ref = EXCLUDED.ref,
				page = EXCLUDED.page,
				caption = EXCLUDED.caption,
				ordinal = EXCLUDED.ordinal`,
			workspaceID, sourceKey, f.Ref, f.BlobKey, f.ContentType,
			f.Width, f.Height, f.SizeBytes, f.Page, f.Caption, f.Ordinal, f.SHA256,
		); err != nil {
			return nil, fmt.Errorf("store: upsert figure %s: %w", f.Ref, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit replace figures: %w", err)
	}
	return orphaned, nil
}

func shaList(figs []jobs.FigureRecord) []string {
	out := make([]string, 0, len(figs))
	for _, f := range figs {
		out = append(out, f.SHA256)
	}
	return out
}

// ListFigures returns a workspace's figures in document order, which is the
// order both a reader and the model should meet them in.
func (s *WikiStore) ListFigures(ctx context.Context, workspaceID string) ([]FigureRow, error) {
	return s.queryFigures(ctx, `
		SELECT id, workspace_id, source_key, ref, blob_key, content_type,
		       width, height, size_bytes, page, caption, ordinal, sha256
		FROM figures WHERE workspace_id = $1
		ORDER BY source_key, ordinal, page, ref`, workspaceID)
}

// FiguresForSources returns the figures belonging to the given source keys.
// Used to tell one unit's generation which pictures it may cite.
func (s *WikiStore) FiguresForSources(ctx context.Context, workspaceID string, keys []string) ([]jobs.FigureRecord, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := s.queryFigures(ctx, `
		SELECT id, workspace_id, source_key, ref, blob_key, content_type,
		       width, height, size_bytes, page, caption, ordinal, sha256
		FROM figures WHERE workspace_id = $1 AND source_key = ANY($2)
		ORDER BY source_key, ordinal, page, ref`, workspaceID, keys)
	if err != nil {
		return nil, err
	}
	out := make([]jobs.FigureRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.asRecord())
	}
	return out, nil
}

func (s *WikiStore) queryFigures(ctx context.Context, sql string, args ...any) ([]FigureRow, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: load figures: %w", err)
	}
	defer rows.Close()

	out := []FigureRow{}
	for rows.Next() {
		var f FigureRow
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.SourceKey, &f.Ref, &f.BlobKey,
			&f.ContentType, &f.Width, &f.Height, &f.SizeBytes, &f.Page,
			&f.Caption, &f.Ordinal, &f.SHA256); err != nil {
			return nil, fmt.Errorf("store: scan figure: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Figure loads one figure by id, scoped to the workspace so an id from another
// tenant is indistinguishable from a missing one.
func (s *WikiStore) Figure(ctx context.Context, workspaceID, id string) (FigureRow, error) {
	var f FigureRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, source_key, ref, blob_key, content_type,
		       width, height, size_bytes, page, caption, ordinal, sha256
		FROM figures WHERE workspace_id = $1 AND id = $2`, workspaceID, id).
		Scan(&f.ID, &f.WorkspaceID, &f.SourceKey, &f.Ref, &f.BlobKey,
			&f.ContentType, &f.Width, &f.Height, &f.SizeBytes, &f.Page,
			&f.Caption, &f.Ordinal, &f.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return FigureRow{}, ErrNotFound
	}
	if err != nil {
		// An id that is not a UUID reads as absent rather than as a server
		// error: it is a request for something that cannot exist.
		if isInvalidUUID(err) {
			return FigureRow{}, ErrNotFound
		}
		return FigureRow{}, fmt.Errorf("store: load figure: %w", err)
	}
	return f, nil
}

// isInvalidUUID reports whether an error is Postgres refusing to parse an id.
//
// Figure ids travel in page URLs, which means this endpoint gets hit with
// truncated and hand-edited ids as a matter of course. "22P02" is
// invalid_text_representation: the id could not be a row, so the honest answer
// is 404, not a 500 that looks like the server broke.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// unreferencedBlobs narrows a set of figure blob keys to those no figure row
// references any more, so only genuinely dead bytes are deleted.
//
// The check is deliberately not workspace-scoped. Keys embed the workspace id,
// so a key can only be shared within one bench -- and asking globally means
// this stays correct if that ever stops being true.
func unreferencedBlobs(ctx context.Context, tx pgx.Tx, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT c FROM unnest($1::text[]) AS c
		WHERE NOT EXISTS (SELECT 1 FROM figures f WHERE f.blob_key = c)`, candidates)
	if err != nil {
		return nil, fmt.Errorf("store: check figure blob references: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("store: scan unreferenced figure blob: %w", err)
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// dropFiguresForSources removes the figure rows belonging to sources being
// deleted, returning the blob keys nothing references any more so the cascade
// can free the bytes.
//
// Inside the caller's transaction: figures and the source that produced them
// have to leave together, or a deleted document's pictures would stay
// reachable at their API path.
func dropFiguresForSources(ctx context.Context, tx pgx.Tx, workspaceID string, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		DELETE FROM figures
		WHERE workspace_id = $1 AND source_key = ANY($2)
		RETURNING blob_key`, workspaceID, keys)
	if err != nil {
		return nil, fmt.Errorf("store: drop figures: %w", err)
	}
	var candidates []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan dropped figure: %w", err)
		}
		candidates = append(candidates, key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: drop figures: %w", err)
	}
	return unreferencedBlobs(ctx, tx, candidates)
}
