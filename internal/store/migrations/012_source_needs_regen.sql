-- A durable "this source's prose is stale" flag, set by cascade deletion.
--
-- Deleting a source removes the pages only it produced, but a page two sources
-- wrote survives -- and its body still describes material the bench no longer
-- holds. The pipeline already handles that case for a review-approved
-- deletion: it plans the cascade and the surviving owners of shared pages ride
-- along in the same run (regenKeysFor). An explicit delete has no run to ride,
-- so the intent has to outlive the request that formed it.
--
-- input_hash cannot carry this. Clearing it would make the unit look changed,
-- which is true, but the router decides what is even considered before the
-- hash gate runs -- an unchanged file is never routed, so a cleared hash would
-- never be read. This flag is consulted after routing, exactly where
-- regenKeysFor injects its keys.
--
-- Cleared on the next successful import of that source: a regenerated page is
-- by definition no longer stale. A source whose unit failed keeps the flag and
-- retries, which is the behavior a lost regeneration should have.

ALTER TABLE sources ADD COLUMN needs_regen BOOLEAN NOT NULL DEFAULT FALSE;

-- Partial: the flag is false for all but a handful of rows at any time, and
-- the pipeline's question is "which sources in this workspace are flagged".
CREATE INDEX sources_needs_regen_idx ON sources (workspace_id) WHERE needs_regen;
