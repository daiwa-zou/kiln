package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// uploadDocKeyPrefix is what the SQL in this file and in SetFileEnabled spells
// out as 'doc:upload:' || path: the sources key of an uploaded document. It
// must equal diff.DocKey(diff.UploadOrigin(path)) for every path, or the join
// quietly reports every document unread; fileingest_test.go pins the parity.
const uploadDocKeyPrefix = "doc:upload:"

// FileSkip is one document the sync could not read: the path and sha256 of
// the bytes that were staged, and the class token that says why (no_text,
// extractor_missing, ...). Class is a token from the worker's allow-list, never
// extractor text; the column's CHECK caps it at 40 characters.
type FileSkip struct {
	Path   string
	SHA256 string
	Class  string
}

// sortFileSkips returns the skips ordered by path with one entry per path (the
// last one wins). Postgres takes row locks in the order the update visits
// them, so two sinks writing the same bench must agree on that order or they
// can deadlock; and an UPDATE ... FROM that matches one target row twice picks
// one of the two unpredictably, which collapsing here rules out.
func sortFileSkips(skips []FileSkip) []FileSkip {
	byPath := make(map[string]FileSkip, len(skips))
	for _, sk := range skips {
		byPath[sk.Path] = sk
	}
	out := make([]FileSkip, 0, len(byPath))
	for _, sk := range byPath {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// RecordFileSkips persists what a sync could not read and clears what it
// could. skips are set on the file rows whose sha256 still matches the staged
// bytes, so a file replaced during the build is left alone (its skip would
// describe bytes that are gone; it reads as waiting). readPaths are the
// documents the same sync did read: their stale skips are cleared, and nothing
// else is, so a paused file keeps the skip it earned before the pause.
//
// Repeating a skip is a no-op (updated_at is never touched here: the Ingest
// list sorts on it). changed lists the paths whose skip was set or replaced,
// so the caller can log a new skip louder than a repeat.
func (s *WikiStore) RecordFileSkips(ctx context.Context, workspaceID string, skips []FileSkip, readPaths []string, at time.Time) (changed []string, err error) {
	skips = sortFileSkips(skips)
	if len(skips) == 0 && len(readPaths) == 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin record file skips: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	if len(skips) > 0 {
		paths := make([]string, len(skips))
		shas := make([]string, len(skips))
		classes := make([]string, len(skips))
		for i, sk := range skips {
			paths[i], shas[i], classes[i] = sk.Path, sk.SHA256, sk.Class
		}
		// left() is a belt under the CHECK: a class the allow-list should have
		// caught is truncated rather than failing every other row in the batch.
		rows, err := tx.Query(ctx, `
			UPDATE workspace_files f
			SET skip_reason = left(v.class, 40), skip_sha256 = v.sha, skipped_at = $5
			FROM unnest($2::text[], $3::text[], $4::text[]) AS v(path, sha, class)
			WHERE f.workspace_id = $1 AND f.path = v.path AND f.sha256 = v.sha
			  AND (f.skip_sha256, f.skip_reason) IS DISTINCT FROM (v.sha, left(v.class, 40))
			RETURNING f.path`,
			workspaceID, paths, shas, classes, at)
		if err != nil {
			return nil, fmt.Errorf("store: record file skips: %w", err)
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scan skipped path: %w", err)
			}
			changed = append(changed, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: record file skips: %w", err)
		}
	}
	if len(readPaths) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE workspace_files
			SET skip_reason = NULL, skip_sha256 = NULL, skipped_at = NULL
			WHERE workspace_id = $1 AND path = ANY($2) AND skip_reason IS NOT NULL`,
			workspaceID, readPaths); err != nil {
			return nil, fmt.Errorf("store: clear file skips: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit record file skips: %w", err)
	}
	sort.Strings(changed)
	return changed, nil
}

// FileIngestRow is a FileRow plus the facts the ingest status is derived from.
// It is facts only: the derivation (read, waiting, skipped) is a pure function
// in internal/api, so it can be table-tested without a database.
type FileIngestRow struct {
	FileRow
	// SkipClass is the recorded skip's class token, nil when none was recorded.
	SkipClass *string
	// SkipLive says the recorded skip describes the current bytes
	// (skip_sha256 = sha256). A skip on replaced bytes is history, not status.
	SkipLive bool
	// HasSource says a live sources row exists for the document.
	HasSource bool
	// Pages counts the distinct pages the document and its section units
	// wrote; nil without a source row. A page two documents share counts for
	// both.
	Pages *int
	// ReadAt is when the document's source was last read (fetched_at, falling
	// back to updated_at for rows written before fetched_at was recorded).
	ReadAt *time.Time
	// ContentAt is when the bytes last changed (content_changed_at, falling
	// back to created_at for rows uploaded before it was recorded).
	ContentAt time.Time
}

// FileIngestBench is the bench-level half of the derivation: what the runs
// table says about the last ingest and the next one.
type FileIngestBench struct {
	// LastRun is when the last ingest that could have read a document began:
	// not research, and not failed. Nil when the bench has never built.
	LastRun *time.Time
	// PendingRun says an ingest is queued or running, so a document the last
	// run did not read is still waiting rather than passed over.
	PendingRun bool
}

// listFilesWithIngestSQL is ListFilesWithIngest's row query. A named constant
// so the hermetic parity test can see the key literal it joins on.
//
// Pages unions the document's own files_written with those of its section
// units ('doc:upload:<path>#<section>'), which collect.go attributes to the
// same document; starts_with rather than LIKE so a path containing % or _ is
// not a pattern.
const listFilesWithIngestSQL = `
	SELECT f.id, f.workspace_id, f.path, f.display_name, f.blob_key, f.size_bytes,
	       coalesce(f.content_type, ''), f.sha256, coalesce(f.uploaded_by::text, ''),
	       f.enabled, f.created_at, f.updated_at,
	       f.skip_reason,
	       coalesce(f.skip_sha256 = f.sha256, false),
	       s.id IS NOT NULL,
	       CASE WHEN s.id IS NULL THEN NULL ELSE (
	           SELECT count(DISTINCT p.page)
	           FROM sources x, jsonb_array_elements_text(x.files_written) AS p(page)
	           WHERE x.workspace_id = f.workspace_id AND x.deleted_at IS NULL
	             AND (x.key = s.key OR starts_with(x.key, s.key || '#'))) END,
	       coalesce(s.fetched_at, s.updated_at),
	       coalesce(f.content_changed_at, f.created_at)
	FROM workspace_files f
	LEFT JOIN sources s
	       ON s.workspace_id = f.workspace_id
	      AND s.key = 'doc:upload:' || f.path
	      AND s.deleted_at IS NULL
	WHERE f.workspace_id = $1
	ORDER BY f.path`

// ListFilesWithIngest is ListFiles with the facts the per-document ingest
// status is derived from, for the API. The worker keeps ListFiles: it
// materializes files and has no use for the join.
func (s *WikiStore) ListFilesWithIngest(ctx context.Context, workspaceID string) ([]FileIngestRow, FileIngestBench, error) {
	rows, err := s.pool.Query(ctx, listFilesWithIngestSQL, workspaceID)
	if err != nil {
		return nil, FileIngestBench{}, fmt.Errorf("store: load files with ingest: %w", err)
	}
	defer rows.Close()

	out := []FileIngestRow{}
	for rows.Next() {
		var f FileIngestRow
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.Path, &f.DisplayName, &f.BlobKey, &f.SizeBytes,
			&f.ContentType, &f.SHA256, &f.UploadedBy, &f.Enabled, &f.CreatedAt, &f.UpdatedAt,
			&f.SkipClass, &f.SkipLive, &f.HasSource, &f.Pages, &f.ReadAt, &f.ContentAt); err != nil {
			return nil, FileIngestBench{}, fmt.Errorf("store: scan file with ingest: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, FileIngestBench{}, fmt.Errorf("store: load files with ingest: %w", err)
	}

	// 'research' is jobs.TriggerResearch, kept as a SQL literal as queue.go
	// does. A research run materializes nothing, so it never read a document;
	// a failed run may have read none of them.
	var bench FileIngestBench
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT max(started_at) FROM runs
		        WHERE workspace_id = $1 AND trigger <> 'research'
		          AND status IN ('succeeded', 'partial', 'no_changes')
		          AND started_at IS NOT NULL),
		       EXISTS (SELECT 1 FROM runs
		               WHERE workspace_id = $1 AND trigger <> 'research'
		                 AND status IN ('queued', 'running'))`,
		workspaceID).Scan(&bench.LastRun, &bench.PendingRun); err != nil {
		return nil, FileIngestBench{}, fmt.Errorf("store: load ingest runs: %w", err)
	}
	return out, bench, nil
}
