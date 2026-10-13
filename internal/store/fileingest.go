package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/daiwa-zou/kiln/internal/diff"
)

// uploadDocKeyPrefix is the sources key of an uploaded document with the path
// left off. SQL that joins workspace_files to sources binds it as a parameter
// and appends the path, so the key is spelled by diff alone and the join can
// not drift from the producers; fileingest_test.go pins that appending the
// path is all diff does.
var uploadDocKeyPrefix = string(diff.DocKey(diff.UploadOrigin("")))

// FileSkip is one document the sync could not read: the path and sha256 of
// the bytes that were staged, and the class token that says why (no_text,
// extractor_missing, ...). Class is one of the upload connector's closed set
// (upload.SkipClasses), never extractor text; the column's CHECK caps it at
// 40 characters and left() below keeps a stray long token from failing the
// batch.
type FileSkip struct {
	Path   string
	SHA256 string
	Class  string
}

// sortFileSkips returns the skips ordered by path with one entry per path (the
// last one wins). An UPDATE ... FROM that matches one target row twice picks
// one of the two unpredictably, which collapsing here rules out; the sort
// makes the batch, and the changed list, deterministic for the caller's log.
// It is not a lock-ordering guarantee: the planner decides the order an
// UPDATE visits rows, whatever order unnest hands it. What keeps two sinks
// from writing the same bench at once is the one-active-run-per-bench index
// (runs_workspace_active_idx), not this.
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

// RecordFileSkips records what one sync did with the bench's uploads: staged
// is every file it materialized (path to the sha256 of the bytes it staged),
// skips the ones it could not read. One transaction, three statements:
//
//   - staged_at = at on every staged row whose sha256 still matches, so a
//     file replaced while the build ran is left alone (the stamp would claim a
//     build saw bytes it never had; it reads as waiting instead);
//   - the skip on the skipped rows, sha-guarded the same way;
//   - the skip cleared on the staged rows that were not skipped: the sync read
//     them, so a skip an earlier build recorded is stale. Nothing else is
//     cleared, so a paused file keeps the skip it earned before the pause, and
//     a skipped path is never cleared by the statement that follows its write.
//
// Repeating a skip is a no-op (updated_at is never touched here: the Ingest
// list sorts on it). changed lists the paths whose skip was set or replaced,
// so the caller can log a new skip louder than a repeat.
func (s *WikiStore) RecordFileSkips(ctx context.Context, workspaceID string, skips []FileSkip, staged map[string]string, at time.Time) (changed []string, err error) {
	skips = sortFileSkips(skips)
	if len(skips) == 0 && len(staged) == 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin record file skips: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	skipped := make(map[string]bool, len(skips))
	for _, sk := range skips {
		skipped[sk.Path] = true
	}
	var stagedPaths, stagedSHAs, readPaths []string
	for p := range staged {
		stagedPaths = append(stagedPaths, p)
		if !skipped[p] {
			readPaths = append(readPaths, p)
		}
	}
	sort.Strings(stagedPaths)
	sort.Strings(readPaths)
	for _, p := range stagedPaths {
		stagedSHAs = append(stagedSHAs, staged[p])
	}

	if len(stagedPaths) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE workspace_files f
			SET staged_at = $4
			FROM unnest($2::text[], $3::text[]) AS v(path, sha)
			WHERE f.workspace_id = $1 AND f.path = v.path AND f.sha256 = v.sha`,
			workspaceID, stagedPaths, stagedSHAs, at); err != nil {
			return nil, fmt.Errorf("store: stamp staged files: %w", err)
		}
	}
	if len(skips) > 0 {
		paths := make([]string, len(skips))
		shas := make([]string, len(skips))
		classes := make([]string, len(skips))
		for i, sk := range skips {
			paths[i], shas[i], classes[i] = sk.Path, sk.SHA256, sk.Class
		}
		// left() is a belt under the CHECK: a token longer than the column
		// takes is truncated rather than failing every other row in the batch.
		rows, err := tx.Query(ctx, `
			UPDATE workspace_files f
			SET skip_class = left(v.class, 40), skip_sha256 = v.sha
			FROM unnest($2::text[], $3::text[], $4::text[]) AS v(path, sha, class)
			WHERE f.workspace_id = $1 AND f.path = v.path AND f.sha256 = v.sha
			  AND (f.skip_sha256, f.skip_class) IS DISTINCT FROM (v.sha, left(v.class, 40))
			RETURNING f.path`,
			workspaceID, paths, shas, classes)
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
			SET skip_class = NULL, skip_sha256 = NULL
			WHERE workspace_id = $1 AND path = ANY($2) AND skip_class IS NOT NULL`,
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
	// StagedAt is when a build last staged the current bytes for the
	// connector (staged_at); nil when none has. A document staged since its
	// bytes changed, with no source row and no recorded skip, was passed over
	// by a build that saw it.
	StagedAt *time.Time
}

// FileIngestBench is the bench-level half of the derivation: what the runs
// table says about the next ingest.
type FileIngestBench struct {
	// PendingRun says a build is queued or running (research aside), so a
	// document the last one did not read is still waiting rather than passed
	// over. Any build counts: which uploads a run stages is recorded on the
	// rows (StagedAt), so the bench need not guess from the run's connector.
	PendingRun bool
	// BudgetPaused says the bench's most recent run (research aside) was
	// refused over budget: anything unread stays unread until the window
	// resets, and "press Ingest now" would be refused too.
	BudgetPaused bool
}

// listFilesWithIngestSQL is ListFilesWithIngest's row query. $2 is the upload
// key prefix (uploadDocKeyPrefix).
//
// Pages unions the document's own files_written with those of its section
// units ('doc:upload:<path>#<section>'), which collect.go attributes to the
// same document; starts_with rather than LIKE so a path containing % or _ is
// not a pattern.
const listFilesWithIngestSQL = `
	SELECT f.id, f.workspace_id, f.path, f.display_name, f.blob_key, f.size_bytes,
	       coalesce(f.content_type, ''), f.sha256, coalesce(f.uploaded_by::text, ''),
	       f.enabled, f.created_at, f.updated_at,
	       f.skip_class,
	       coalesce(f.skip_sha256 = f.sha256, false),
	       s.id IS NOT NULL,
	       CASE WHEN s.id IS NULL THEN NULL ELSE (
	           SELECT count(DISTINCT p.page)
	           FROM sources x, jsonb_array_elements_text(x.files_written) AS p(page)
	           WHERE x.workspace_id = f.workspace_id AND x.deleted_at IS NULL
	             AND (x.key = s.key OR starts_with(x.key, s.key || '#'))) END,
	       coalesce(s.fetched_at, s.updated_at),
	       coalesce(f.content_changed_at, f.created_at),
	       f.staged_at
	FROM workspace_files f
	LEFT JOIN sources s
	       ON s.workspace_id = f.workspace_id
	      AND s.key = $2::text || f.path
	      AND s.deleted_at IS NULL
	WHERE f.workspace_id = $1
	ORDER BY f.path`

// ListFilesWithIngest is ListFiles with the facts the per-document ingest
// status is derived from, for the API. The worker keeps ListFiles: it
// materializes files and has no use for the join.
//
// The bench facts are read before the rows. A build finishing between the two
// statements then leaves a row stamped and sourced against a bench that still
// says a run is pending, which derives waiting or read; the other order could
// pair a stamped, sourceless row with an idle bench and derive a false "not
// read" until the next load.
func (s *WikiStore) ListFilesWithIngest(ctx context.Context, workspaceID string) ([]FileIngestRow, FileIngestBench, error) {
	// 'research' is jobs.TriggerResearch, kept as a SQL literal as queue.go
	// does. A research run materializes nothing, so it is never the ingest
	// about to read a document.
	var bench FileIngestBench
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM runs
		               WHERE workspace_id = $1 AND trigger <> 'research'
		                 AND status IN ('queued', 'running')),
		       coalesce((SELECT status FROM runs
		                 WHERE workspace_id = $1 AND trigger <> 'research'
		                 ORDER BY created_at DESC LIMIT 1) = 'over_budget', false)`,
		workspaceID).Scan(&bench.PendingRun, &bench.BudgetPaused); err != nil {
		return nil, FileIngestBench{}, fmt.Errorf("store: load ingest runs: %w", err)
	}

	rows, err := s.pool.Query(ctx, listFilesWithIngestSQL, workspaceID, uploadDocKeyPrefix)
	if err != nil {
		return nil, FileIngestBench{}, fmt.Errorf("store: load files with ingest: %w", err)
	}
	defer rows.Close()

	out := []FileIngestRow{}
	for rows.Next() {
		var f FileIngestRow
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.Path, &f.DisplayName, &f.BlobKey, &f.SizeBytes,
			&f.ContentType, &f.SHA256, &f.UploadedBy, &f.Enabled, &f.CreatedAt, &f.UpdatedAt,
			&f.SkipClass, &f.SkipLive, &f.HasSource, &f.Pages, &f.ReadAt, &f.ContentAt, &f.StagedAt); err != nil {
			return nil, FileIngestBench{}, fmt.Errorf("store: scan file with ingest: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, FileIngestBench{}, fmt.Errorf("store: load files with ingest: %w", err)
	}
	return out, bench, nil
}
