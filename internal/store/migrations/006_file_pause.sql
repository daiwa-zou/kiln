-- Per-document pause. A disabled file is skipped when the worker stages the
-- documents source: not re-read, not regenerated, and -- because the build
-- reports it as skipped rather than absent -- never flagged as a deletion.
-- Its pages simply stop updating until it is resumed.

ALTER TABLE workspace_files
    ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE;
