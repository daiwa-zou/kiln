-- M2: server-side builds. The queue is the runs table itself: a run is created
-- with status 'queued' and claimed by a worker with FOR UPDATE SKIP LOCKED, so
-- claim and state change are one transaction and no external queue is needed.

ALTER TABLE runs
    ADD COLUMN claimed_by TEXT,
    ADD COLUMN claimed_at TIMESTAMPTZ;

-- One active run per workspace. A second enqueue while one is queued or
-- running is refused at insert, which is also the webhook debounce M3 relies
-- on: a push storm collapses into the single run already waiting.
CREATE UNIQUE INDEX runs_workspace_active_idx
    ON runs (workspace_id) WHERE status IN ('queued', 'running');

-- The claim query walks queued runs in arrival order.
CREATE INDEX runs_queued_idx ON runs (created_at) WHERE status = 'queued';
