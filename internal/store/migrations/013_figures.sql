-- Pictures and graphs recovered from source documents.
--
-- A figure belongs to a source, not to a page. The source is what produced it
-- and what its lifetime is tied to: a page referencing a figure is a citation,
-- the same shape as a wikilink, and a page can be rewritten or deleted without
-- the figure meaning anything different. That is also what makes the deletion
-- cascade work unchanged -- figures go when their source does, and the blob
-- keys ride in sources.blob_keys so the existing cascade frees the bytes.
--
-- source_key rather than a foreign key to sources(id): the pipeline writes
-- figures during a sync, before the source row for that run exists, and the
-- sources table is keyed on (workspace_id, key) with rows dropped and recreated
-- freely. Keying on the same pair the rest of the pipeline uses means a figure
-- survives its source row being rewritten, which happens on every build.
--
-- sha256 is the identity. The same picture extracted twice is one figure, so a
-- rebuild that re-reads an unchanged document neither duplicates rows nor
-- invalidates the ids pages already reference.

CREATE TABLE figures (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    source_key   TEXT NOT NULL,
    -- ref is the extractor's within-document identifier ("p3-i2", or a media
    -- basename). Kept so a figure can be traced back to the document it came
    -- from when someone asks why it looks the way it does.
    ref          TEXT NOT NULL,
    blob_key     TEXT NOT NULL,
    content_type TEXT NOT NULL,
    width        INTEGER NOT NULL,
    height       INTEGER NOT NULL,
    size_bytes   BIGINT NOT NULL,
    -- page is 1-based, or 0 for formats without pages.
    page         INTEGER NOT NULL DEFAULT 0,
    -- caption is the document's own words: alt text, a figcaption, or a
    -- "Figure 3: ..." line found near the image. Empty when it had none.
    caption      TEXT NOT NULL DEFAULT '',
    -- ordinal is document order, which is the order a reader should meet them.
    ordinal      INTEGER NOT NULL DEFAULT 0,
    sha256       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, source_key, sha256)
);
CREATE INDEX figures_source_idx ON figures (workspace_id, source_key);
