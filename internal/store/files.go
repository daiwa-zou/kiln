package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/daiwa-zou/kiln/internal/diff"
)

// FileRow is one uploaded workspace document. Path is the sanitized relative
// path the worker materializes the file under at sync time; BlobKey names the
// bytes in object storage and is server-constructed, never user input.
type FileRow struct {
	ID          string
	WorkspaceID string
	Path        string
	// DisplayName is a readable label derived from the filename at upload
	// time. Never part of identity: Path is what the row is unique on, and two
	// uploads may derive the same label.
	DisplayName string
	BlobKey     string
	SizeBytes   int64
	ContentType string
	SHA256      string
	UploadedBy  string
	// Enabled gates ingestion: a paused file is skipped when the documents
	// source is staged, without reading as a deletion.
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListFiles returns a workspace's uploaded files ordered by path, the order
// both the UI and the worker's materialization want.
func (s *WikiStore) ListFiles(ctx context.Context, workspaceID string) ([]FileRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, path, display_name, blob_key, size_bytes,
		       coalesce(content_type, ''), sha256, coalesce(uploaded_by::text, ''),
		       enabled, created_at, updated_at
		FROM workspace_files WHERE workspace_id = $1 ORDER BY path`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: load files: %w", err)
	}
	defer rows.Close()

	out := []FileRow{}
	for rows.Next() {
		var f FileRow
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.Path, &f.DisplayName, &f.BlobKey, &f.SizeBytes,
			&f.ContentType, &f.SHA256, &f.UploadedBy, &f.Enabled, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan file: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CreateFile records an uploaded file, replacing any existing row at the same
// (workspace, path). It returns the stored row's id and, when a row was
// replaced, the superseded blob key so the caller can delete the old blob
// AFTER the transaction has committed -- deleting first would strand the row
// pointing at nothing if the commit fails.
func (s *WikiStore) CreateFile(ctx context.Context, f FileRow) (id, replacedBlobKey string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("store: begin create file: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// Lock any existing row so two concurrent uploads to the same path
	// serialize; the loser replaces the winner rather than racing it.
	var prevBlobKey string
	err = tx.QueryRow(ctx, `
		SELECT blob_key FROM workspace_files
		WHERE workspace_id = $1 AND path = $2 FOR UPDATE`,
		f.WorkspaceID, f.Path).Scan(&prevBlobKey)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("store: lock file row: %w", err)
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO workspace_files
			(workspace_id, path, display_name, blob_key, size_bytes, content_type, sha256, uploaded_by,
			 content_changed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (workspace_id, path) DO UPDATE SET
			display_name = excluded.display_name,
			blob_key     = excluded.blob_key,
			size_bytes   = excluded.size_bytes,
			content_type = excluded.content_type,
			sha256       = excluded.sha256,
			uploaded_by  = excluded.uploaded_by,
			-- Re-uploading a paused path resumes it: fresh content is the
			-- clearest possible signal the file is wanted again.
			enabled      = TRUE,
			updated_at   = now(),
			-- Only a change of bytes is a change of content. Re-uploading the
			-- same file must not make a read document look unread, and
			-- updated_at cannot carry this: the build's own naming pass bumps it.
			content_changed_at = CASE
				WHEN workspace_files.sha256 IS DISTINCT FROM excluded.sha256 THEN now()
				ELSE workspace_files.content_changed_at END
		RETURNING id`,
		f.WorkspaceID, f.Path, f.DisplayName, f.BlobKey, f.SizeBytes,
		nullable(f.ContentType), f.SHA256, nullable(f.UploadedBy)).Scan(&id); err != nil {
		return "", "", fmt.Errorf("store: create file: %w", err)
	}
	// The superseded blob is garbage the moment this commits. Queued in the same
	// transaction so a failed delete after the commit, or a crash, leaves a
	// record for the sweep instead of bytes nothing remembers.
	if prevBlobKey != f.BlobKey {
		if err := enqueueBlobDeletes(ctx, tx, []string{prevBlobKey}); err != nil {
			return "", "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("store: commit create file: %w", err)
	}
	return id, prevBlobKey, nil
}

// SetFileEnabled pauses or resumes one file's ingestion, scoped to the
// workspace so a cross-tenant id reads as absent.
//
// Resuming a file the wiki has never read marks its content as changed, so
// the Ingest view says "waiting" rather than "not read by the last ingest":
// the last ingest skipped it on purpose. A resumed file that has a live source
// row keeps its stamp, because its pages are still current. $4 is the upload
// key prefix ListFilesWithIngest joins on (uploadDocKeyPrefix).
func (s *WikiStore) SetFileEnabled(ctx context.Context, workspaceID, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE workspace_files SET enabled = $3, updated_at = now(),
		    content_changed_at = CASE
		        WHEN $3::boolean AND NOT EXISTS (
		            SELECT 1 FROM sources s
		            WHERE s.workspace_id = workspace_files.workspace_id
		              AND s.key = $4::text || workspace_files.path
		              AND s.deleted_at IS NULL)
		        THEN now() ELSE content_changed_at END
		WHERE workspace_id = $1 AND id = $2`, workspaceID, id, enabled, uploadDocKeyPrefix)
	if err != nil {
		return fmt.Errorf("store: set file enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteFile removes a file row and the wiki content only that document
// produced, scoped to the workspace so an id from another tenant is
// indistinguishable from a missing one.
//
// One transaction on purpose. A document whose row is gone while its pages
// remain is the state this exists to prevent, and splitting the two would open
// exactly that window on any failure between them. The blob key and the
// cascade's released blobs both come back for the caller to delete after the
// commit: deleting bytes the transaction might still roll back is the one
// ordering that cannot be undone.
func (s *WikiStore) DeleteFile(ctx context.Context, workspaceID, id string) (blobKey string, c diff.Cascade, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", diff.Cascade{}, fmt.Errorf("store: begin delete file: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// Before the row goes, so an import in flight finishes first and its output
	// is visible to the cascade below; one that starts afterwards finds the file
	// gone and leaves this document out (see fenceImport).
	if err := lockImports(ctx, tx, workspaceID); err != nil {
		return "", diff.Cascade{}, err
	}

	var path string
	err = tx.QueryRow(ctx, `
		DELETE FROM workspace_files WHERE workspace_id = $1 AND id = $2
		RETURNING path, blob_key`, workspaceID, id).Scan(&path, &blobKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", diff.Cascade{}, ErrNotFound
	}
	if err != nil {
		return "", diff.Cascade{}, fmt.Errorf("store: delete file: %w", err)
	}

	// The unit key an uploaded document maps to. Built through UploadOrigin
	// like every other producer, so a repo README and an uploaded one stay
	// distinct sources here as well.
	c, err = cascadeDeleteSources(ctx, tx, workspaceID, []diff.Key{diff.DocKey(diff.UploadOrigin(path))})
	if err != nil {
		return "", diff.Cascade{}, err
	}
	// The document's own blob and everything the cascade released, queued with
	// the row deletes so the intent survives a failed delete after the commit.
	if err := enqueueBlobDeletes(ctx, tx, append([]string{blobKey}, c.DeleteBlobs...)); err != nil {
		return "", diff.Cascade{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", diff.Cascade{}, fmt.Errorf("store: commit delete file: %w", err)
	}
	return blobKey, c, nil
}
