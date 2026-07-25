-- Derived wiki artifacts: the index, the overview, and the log.
--
-- These are generated deterministically from page frontmatter after every run
-- and are never agent-writable, which is what stops navigation drifting from
-- content. Until now the pipeline computed them and the store discarded them,
-- so a built wiki had pages and no way to navigate them.
--
-- index and overview are pure functions of the current page set and could be
-- recomputed on read; they are stored so a reader does not pay for the rebuild
-- and so the exact bytes a run produced remain inspectable. The log cannot be
-- recomputed at all -- it is append-only history of how the wiki got here.

CREATE TABLE wiki_artifacts (
    wiki_id    UUID NOT NULL REFERENCES wikis(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('index', 'overview', 'log')),
    body       TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (wiki_id, kind)
);
