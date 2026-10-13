-- Per-document ingest status: whether a build has looked at a document since
-- its bytes last changed, and why it was skipped if it was.
--
-- A read upload leaves a sources row keyed 'doc:upload:' || path. A skipped
-- one (an image-only PDF, a missing extractor) left nothing: the connector's
-- skip list was discarded on server builds, so a document the wiki never read
-- looked identical to one it had. These columns are where the worker now
-- records that.
--
-- `staged_at` is when a build last staged these bytes for the connector. The
-- worker stamps it, sha-guarded, for every file it materialized, whatever the
-- build went on to do; so a document with no sources row and a stamp at or
-- after its last change was passed over by a build that saw it, and one with
-- no stamp is still waiting for a build to look. It is the fact the Ingest
-- view derives "not read" from, and the worker knows it exactly where a guess
-- from the runs table (which connector a run was pinned to, what triggered it)
-- could only approximate.
--
-- `skip_class` holds a short class token (no_text, extractor_missing, ...),
-- never the extractor's own text about the file. The API maps the token to a
-- fixed sentence; the raw text goes to the operator log only. The CHECK is the
-- store's cap on that promise. Anything that ever puts this column into a
-- prompt must still go through fenceAs.
--
-- `skip_sha256` is the sha256 of the bytes that were skipped. A skip is live
-- only while it equals the row's sha256, so a replacement voids it with no
-- clearing logic and re-uploading identical bytes keeps it (still image-only).
--
-- `content_changed_at` is when the bytes last changed. created_at never moves
-- on CreateFile's conflict path and updated_at moves on rename and pause, so
-- neither can say "changed since read". Readers coalesce to created_at.
--
-- All nullable, no default, no backfill: existing rows read as never staged
-- and never skipped, and as changed when they were created, which is what the
-- old code knew.

ALTER TABLE workspace_files
    ADD COLUMN skip_class         TEXT
        CHECK (skip_class IS NULL OR char_length(skip_class) <= 40),
    ADD COLUMN skip_sha256        TEXT,
    ADD COLUMN staged_at          TIMESTAMPTZ,
    ADD COLUMN content_changed_at TIMESTAMPTZ;
