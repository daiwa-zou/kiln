-- Indexes for two lookups that were scanning more than they needed to.
--
-- These are plain CREATE INDEX, not CONCURRENTLY: migrations run inside a
-- transaction (see applyOne), and CONCURRENTLY cannot. Building takes a lock
-- that blocks writes to the table for the duration, which is short for tables
-- of this size, and `kiln admin migrate` is run as a deliberate step rather
-- than on boot.

-- unreferencedBlobs asks "does any figure still point at this blob?" for each
-- candidate key. The check is deliberately not scoped to a workspace -- keys
-- embed the workspace id, but asking globally stays correct if that ever stops
-- being true -- so figures_source_idx, which leads with workspace_id, cannot
-- serve it, and every deleted or replaced figure was a sequential scan of every
-- tenant's figures.
CREATE INDEX figures_blob_key_idx ON figures (blob_key);

-- Deletion reviews are looked up by (workspace, kind, title) while they are
-- live: filing one first checks that none is already open or approved for the
-- same source key, approving one is read back ordered by title, and a cascade
-- closes them by title. The existing review_items_workspace_open_idx only
-- covers status = 'open' and only on workspace_id, so none of those could use
-- it, and a namespace vanishing meant a scan of the workspace's whole review
-- history once per disappeared source.
--
-- Partial on the two statuses that mean "still live", which keeps resolved
-- history -- the bulk of the table over time -- out of the index. The open-only
-- duplicate checks on agent-raised flags (status = 'open') are implied by the
-- predicate and use it too.
CREATE INDEX review_items_live_title_idx ON review_items (workspace_id, kind, title)
    WHERE status IN ('open', 'approved');
