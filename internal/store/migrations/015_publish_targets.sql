-- Where a bench's wiki is mirrored to.
--
-- The shape deliberately echoes `connectors`, because this is the same kind of
-- thing pointed the other way: a connector says where material comes from, a
-- publish target says where the finished wiki goes. Same ownership (a
-- workspace), same credential model, same last_synced_at / last_error pair so
-- a failure is visible on the page where it was configured rather than only in
-- a log.
--
-- One target per workspace, enforced by the unique index rather than by
-- convention. Two targets would need a policy for what happens when one push
-- succeeds and the other fails, and there is no good answer to that yet; a
-- second row is a feature to add deliberately, not a state to fall into.
--
-- The database stays authoritative. Nothing here is read back during a build:
-- publishing is a mirror, and a repository that has been edited by hand is
-- overwritten by the next push rather than merged.

CREATE TABLE publish_targets (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- kind is 'github' today. Named rather than assumed so a future GitLab or
    -- plain-git target does not need a migration to distinguish itself.
    kind          TEXT NOT NULL DEFAULT 'github',
    repo_url      TEXT NOT NULL,
    branch        TEXT NOT NULL DEFAULT 'main',
    -- path_prefix is the subdirectory the wiki owns; empty means the
    -- repository root. Only that subtree is replaced on a push, so a repo can
    -- hold a hand-written README, CI workflows, and a published wiki at once.
    path_prefix   TEXT NOT NULL DEFAULT '',
    credential_id UUID REFERENCES credentials(id) ON DELETE SET NULL,
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    -- last_commit is the sha of the most recent successful push, which is what
    -- lets the UI link straight to what was published.
    last_commit      TEXT NOT NULL DEFAULT '',
    last_published_at TIMESTAMPTZ,
    last_error       TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id)
);
