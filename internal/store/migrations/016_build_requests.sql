-- Build requests that arrived while the workspace's one active-run slot was
-- taken by a run that cannot do the work.
--
-- runs_workspace_active_idx allows a single queued-or-running run per
-- workspace, of any kind. That is what collapses a push storm into one build,
-- but it also meant a request could "merge" into a run that would never honor
-- it: a queued research run, or a build already past the point of reading its
-- sources. The caller got a 202, the worker ran something else, and the push
-- was never built. Connectors in webhook mode are never polled, so nothing
-- would ever notice.
--
-- A request that cannot merge into a waiting build is recorded here instead,
-- and the worker's claim step turns it into a queued build as soon as the
-- workspace has no active run. A row, not a second run: the unique index stays
-- exactly as it was, so research and builds still take turns on the bench, and
-- a row left behind by a crash is promoted by whichever worker looks next.
--
-- At most one per workspace. A second request while one is waiting merges into
-- it, the same way two webhooks merge into one queued build.

CREATE TABLE build_requests (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    -- The trigger that made the request; kept from the first one, so the run it
    -- becomes reads as what first asked for it.
    trigger      TEXT NOT NULL,
    -- NULL means every connector. A request for one connector widens to NULL
    -- when a request for another arrives, and never narrows back.
    connector_id UUID REFERENCES connectors(id) ON DELETE CASCADE,
    ref_to       TEXT,
    not_before   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
