-- A page a human deleted, and the record that keeps it deleted.
--
-- Soft-deleting a page is not enough on its own. Pages are derived: the source
-- that produced one is still there, still hashes the same, and the next run
-- would write it straight back. A delete that a rebuild undoes is not a delete,
-- it is a delay -- so the intent has to live outside the page, the way
-- page_corrections does. Same reasoning, opposite instruction: a correction
-- says "write it differently", a suppression says "do not write it".
--
-- Keyed on slug rather than path. Slug is the page's database identity -- the
-- live-page unique index is (wiki_id, slug) -- and a page whose type changes
-- moves directories while keeping its name. Keying on path would let a
-- concepts/ page come back as an entities/ page and count as a different page.
--
-- Not keyed to pages.id either, which looks like the obvious foreign key and is
-- the wrong one: the row it points at is swept once the retention window
-- closes, and the suppression has to outlive that. It is a statement about a
-- name in this wiki, not about one row.

CREATE TABLE page_suppressions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    slug         TEXT NOT NULL,
    -- reason is the human's note, shown wherever a suppression is listed and
    -- injected into the prompt so the agent is told why, not merely told no.
    -- Empty is allowed: making someone justify a deletion to their own wiki is
    -- friction with no payoff.
    reason       TEXT NOT NULL DEFAULT '',
    deleted_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, slug)
);
CREATE INDEX page_suppressions_workspace_idx ON page_suppressions (workspace_id);
