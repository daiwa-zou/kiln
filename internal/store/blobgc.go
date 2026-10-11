package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// BlobDeleter is the half of object storage the sweep needs. blob.Store
// satisfies it; declared here so the store does not import the driver package.
// Delete must treat an absent key as success, which every blob driver does.
type BlobDeleter interface {
	Delete(ctx context.Context, key string) error
}

// blobSweepBatch bounds one sweep's work. The sweep runs hourly, so a backlog
// larger than this drains over several sweeps rather than holding one open.
const blobSweepBatch = 500

// blobDeleteTimeout caps one object-store delete. An S3 client retries with
// backoff on its own, and one unanswered key must not spend the whole sweep's
// budget: it is recorded as a failed attempt and retried next sweep, and the
// rest of the batch proceeds. A variable so tests can shorten it.
var blobDeleteTimeout = 30 * time.Second

// enqueueBlobDeletes records blobs to delete, inside the transaction that
// removes the rows referencing them.
//
// Blank keys are skipped: some rows legitimately have no blob, and a delete of
// "" is an invalid key rather than a no-op.
func enqueueBlobDeletes(ctx context.Context, tx pgx.Tx, keys []string) error {
	keys = distinctNonEmpty(keys)
	if len(keys) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pending_blob_deletes (blob_key)
		SELECT unnest($1::text[])
		ON CONFLICT (blob_key) DO NOTHING`, keys); err != nil {
		return fmt.Errorf("store: queue blob deletes: %w", err)
	}
	return nil
}

// cancelBlobDeletes withdraws pending deletes for blobs that are being
// referenced again. Figure keys are content digests, so a picture removed from
// one document can be added to another before the sweep runs; the sweep
// re-checks references too, and this keeps the queue from carrying rows it
// already knows are wrong.
func cancelBlobDeletes(ctx context.Context, tx pgx.Tx, keys []string) error {
	keys = distinctNonEmpty(keys)
	if len(keys) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM pending_blob_deletes WHERE blob_key = ANY($1)`, keys); err != nil {
		return fmt.Errorf("store: withdraw blob deletes: %w", err)
	}
	return nil
}

func distinctNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, k := range in {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// blobStillReferenced reports whether a live row still points at the blob.
// Uploaded files and figures are the two things that serve bytes by key; a
// source's blob_keys is a record of what it read, not a claim on the bytes.
func (s *WikiStore) blobStillReferenced(ctx context.Context, key string) (bool, error) {
	var referenced bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM workspace_files WHERE blob_key = $1)
		    OR EXISTS (SELECT 1 FROM figures WHERE blob_key = $1)`, key).Scan(&referenced)
	if err != nil {
		return false, fmt.Errorf("store: check blob references: %w", err)
	}
	return referenced, nil
}

// PendingBlobDeletes counts the queued blob deletes, so a sweep that has no
// object store to drain them with can say how many are waiting.
func (s *WikiStore) PendingBlobDeletes(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pending_blob_deletes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count pending blob deletes: %w", err)
	}
	return n, nil
}

// SweepBlobs drains the pending blob deletes: for each, delete the object and
// then the row. It returns how many blobs were deleted or found re-referenced
// (done) and how many the object store refused (failed).
//
// Object first, row second, so a crash or a refusal leaves the row for the next
// sweep rather than the bytes with no record of them. A blob that a live row
// references again is not deleted -- its row is simply cleared, because the
// intent it recorded no longer holds. A refused delete is counted against the
// row and retried on the next sweep; an absent object counts as deleted, since
// that is the outcome asked for.
//
// There is one window this does not close: a figure re-extracted at this very
// moment writes its object before its row, so a sweep that checked in between
// would delete bytes about to be referenced. The next build of that document
// writes the object again, and the window is the gap between two statements of
// one build against one sweep an hour -- narrow enough to accept, and a
// self-healing failure (a missing picture) rather than a corrupted one.
func (s *WikiStore) SweepBlobs(ctx context.Context, blobs BlobDeleter) (done, failed int, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT blob_key FROM pending_blob_deletes
		ORDER BY last_attempt_at NULLS FIRST, enqueued_at
		LIMIT $1`, blobSweepBatch)
	if err != nil {
		return 0, 0, fmt.Errorf("store: load pending blob deletes: %w", err)
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: scan pending blob delete: %w", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: load pending blob deletes: %w", err)
	}

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return done, failed, err
		}
		referenced, err := s.blobStillReferenced(ctx, key)
		if err != nil {
			return done, failed, err
		}
		if !referenced {
			dctx, cancel := context.WithTimeout(ctx, blobDeleteTimeout)
			derr := blobs.Delete(dctx, key)
			cancel()
			if derr != nil {
				failed++
				if _, err := s.pool.Exec(ctx, `
					UPDATE pending_blob_deletes
					SET attempts = attempts + 1, last_attempt_at = now(), last_error = $2
					WHERE blob_key = $1`, key, derr.Error()); err != nil {
					return done, failed, fmt.Errorf("store: record blob delete failure: %w", err)
				}
				continue
			}
		}
		if _, err := s.pool.Exec(ctx,
			`DELETE FROM pending_blob_deletes WHERE blob_key = $1`, key); err != nil {
			return done, failed, fmt.Errorf("store: clear pending blob delete: %w", err)
		}
		done++
	}
	return done, failed, nil
}
