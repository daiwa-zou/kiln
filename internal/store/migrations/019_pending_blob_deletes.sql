-- Blobs waiting to be deleted from object storage.
--
-- Object storage is not transactional with Postgres, so every place that
-- orphans a blob (a file deleted or replaced, a figure no document references
-- any more, a whole bench dropped) used to delete it best-effort after the
-- commit. A delete that failed, or a process that died in between, left the
-- bytes behind with nothing anywhere recording that they existed: there is no
-- list operation on the blob store, because everything stored was supposed to
-- be tracked in a table.
--
-- This is that table for the one case the other tables cannot cover -- bytes
-- whose rows are gone. A row is written in the SAME transaction that removes
-- the last row pointing at the blob, so the intent to delete commits or rolls
-- back with the decision to orphan it, and the periodic sweep drains it:
-- delete the object, then the row, retrying on failure.
--
-- No foreign key to workspaces, on purpose: a deleted bench takes its rows
-- with it, and these are exactly the rows that must outlive it.
--
-- blob_key is the primary key. Keys are content-addressed for figures, so the
-- same bytes can be orphaned twice; one pending delete says it all. Nothing
-- here is authoritative about whether a blob may be deleted -- the sweep
-- re-checks references when it runs, because a content-addressed key can be
-- claimed again between being queued and being swept.

CREATE TABLE pending_blob_deletes (
    blob_key        TEXT PRIMARY KEY,
    enqueued_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- attempts and last_error exist so an object store that keeps refusing is
    -- visible to an operator rather than silently retried forever.
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    last_error      TEXT
);

-- The sweep takes never-tried rows first, then the longest-waiting, so a key
-- the store keeps refusing cannot starve the rest.
CREATE INDEX pending_blob_deletes_order_idx
    ON pending_blob_deletes (last_attempt_at NULLS FIRST, enqueued_at);
