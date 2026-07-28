package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// FileRow is one uploaded workspace document. Path is the sanitized relative
// path the worker materializes the file under at sync time; BlobKey names the
// bytes in object storage and is server-constructed, never user input.
type FileRow struct {
	ID          string
	WorkspaceID string
	Path        string
	BlobKey     string
	SizeBytes   int64
	ContentType string
	SHA256      string
	UploadedBy  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ListFiles returns a workspace's uploaded files ordered by path, the order
// both the UI and the worker's materialization want.
func (s *WikiStore) ListFiles(ctx context.Context, workspaceID string) ([]FileRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, path, blob_key, size_bytes,
		       coalesce(content_type, ''), sha256, coalesce(uploaded_by::text, ''),
		       created_at, updated_at
		FROM workspace_files WHERE workspace_id = $1 ORDER BY path`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("store: load files: %w", err)
	}
	defer rows.Close()

	out := []FileRow{}
	for rows.Next() {
		var f FileRow
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.Path, &f.BlobKey, &f.SizeBytes,
			&f.ContentType, &f.SHA256, &f.UploadedBy, &f.CreatedAt, &f.UpdatedAt); err != nil {
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
			(workspace_id, path, blob_key, size_bytes, content_type, sha256, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (workspace_id, path) DO UPDATE SET
			blob_key     = excluded.blob_key,
			size_bytes   = excluded.size_bytes,
			content_type = excluded.content_type,
			sha256       = excluded.sha256,
			uploaded_by  = excluded.uploaded_by,
			updated_at   = now()
		RETURNING id`,
		f.WorkspaceID, f.Path, f.BlobKey, f.SizeBytes,
		nullable(f.ContentType), f.SHA256, nullable(f.UploadedBy)).Scan(&id); err != nil {
		return "", "", fmt.Errorf("store: create file: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("store: commit create file: %w", err)
	}
	return id, prevBlobKey, nil
}

// DeleteFile removes a file row, scoped to the workspace so an id from
// another tenant is indistinguishable from a missing one. It returns the blob
// key for the caller to delete after the row is gone.
func (s *WikiStore) DeleteFile(ctx context.Context, workspaceID, id string) (blobKey string, err error) {
	err = s.pool.QueryRow(ctx, `
		DELETE FROM workspace_files WHERE workspace_id = $1 AND id = $2
		RETURNING blob_key`, workspaceID, id).Scan(&blobKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: delete file: %w", err)
	}
	return blobKey, nil
}
