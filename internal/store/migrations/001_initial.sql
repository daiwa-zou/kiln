-- kiln initial schema.
--
-- Design notes worth knowing before changing anything here:
--
--   * `sources` is the incremental cache. `input_hash` gates whether a unit is
--     regenerated at all, and `files_written` is what makes cascade deletion
--     possible: a page is removed only when no other live source claims it.
--   * Pages are soft-deleted, and `deleted_at` gives a retention window in
--     which a mistaken removal is recoverable. A source *disappearing from a
--     sync* is ambiguous and raises a review item rather than deleting
--     anything; a source someone explicitly deletes cascades immediately
--     (see internal/store/cascade.go for why the two differ).
--   * The index, overview, and log are derived from page frontmatter on every
--     run and are never agent-written, so they are not stored as pages.

-- Each migration is wrapped in a transaction by the runner, so no explicit
-- BEGIN/COMMIT here.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_user_id BIGINT UNIQUE,
    login          TEXT NOT NULL,
    email          TEXT,
    name           TEXT,
    avatar_url     TEXT,
    is_admin       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Sessions are server-side rather than signed cookies so they can be revoked.
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    data       JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

CREATE TABLE orgs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE org_members (
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

-- GitHub is the source of ACL truth for git-backed workspaces, so installation
-- membership is mirrored here rather than maintained as a separate model.
CREATE TABLE github_installations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id BIGINT NOT NULL UNIQUE,
    account_login   TEXT NOT NULL,
    account_type    TEXT NOT NULL,
    suspended_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_installations (
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    installation_id UUID NOT NULL REFERENCES github_installations(id) ON DELETE CASCADE,
    synced_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, installation_id)
);

-- ---------------------------------------------------------------------------
-- Workspaces, connectors, credentials
-- ---------------------------------------------------------------------------

CREATE TABLE workspaces (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    model       TEXT,
    budget_usd  NUMERIC(10,2),
    settings    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

-- Envelope-encrypted connector credentials. Decrypted only in the worker and
-- never passed into the generation sandbox.
CREATE TABLE credentials (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    ciphertext BYTEA NOT NULL,
    nonce      BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE connectors (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id   UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    name           TEXT NOT NULL,
    config         JSONB NOT NULL DEFAULT '{}'::jsonb,
    credential_id  UUID REFERENCES credentials(id) ON DELETE SET NULL,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    trigger_mode   TEXT NOT NULL DEFAULT 'manual',
    last_synced_at TIMESTAMPTZ,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX connectors_workspace_idx ON connectors (workspace_id);

-- ---------------------------------------------------------------------------
-- Wiki content
-- ---------------------------------------------------------------------------

CREATE TABLE wikis (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE UNIQUE,
    revision     BIGINT NOT NULL DEFAULT 0,
    page_count   INTEGER NOT NULL DEFAULT 0,
    last_run_id  UUID,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE pages (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    wiki_id      UUID NOT NULL REFERENCES wikis(id) ON DELETE CASCADE,
    path         TEXT NOT NULL,
    type         TEXT NOT NULL CHECK (type IN
                     ('entity','concept','source','query','comparison','synthesis','overview')),
    slug         TEXT NOT NULL,
    title        TEXT NOT NULL,
    frontmatter  JSONB NOT NULL DEFAULT '{}'::jsonb,
    body         TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL,
    provenance   TEXT NOT NULL DEFAULT 'source',
    built_at_ref TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ,
    search       TSVECTOR GENERATED ALWAYS AS (
                     setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
                     setweight(to_tsvector('english', coalesce(body, '')),  'B')
                 ) STORED
);
-- Slug uniqueness applies only to live pages, so a soft-deleted page does not
-- block regenerating one with the same name.
CREATE UNIQUE INDEX pages_wiki_slug_live_idx ON pages (wiki_id, slug) WHERE deleted_at IS NULL;
CREATE INDEX pages_wiki_type_idx ON pages (wiki_id, type) WHERE deleted_at IS NULL;
CREATE INDEX pages_search_idx ON pages USING GIN (search);
CREATE INDEX pages_deleted_idx ON pages (deleted_at) WHERE deleted_at IS NOT NULL;

-- Materializing links makes the graph view one query instead of re-parsing
-- markdown, and unresolved rows double as the cheapest gap signal available.
CREATE TABLE page_links (
    from_page_id UUID NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    to_slug      TEXT NOT NULL,
    to_page_id   UUID REFERENCES pages(id) ON DELETE SET NULL,
    resolved     BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (from_page_id, to_slug)
);
CREATE INDEX page_links_to_page_idx ON page_links (to_page_id);
CREATE INDEX page_links_unresolved_idx ON page_links (to_slug) WHERE NOT resolved;

-- ---------------------------------------------------------------------------
-- Sources: the incremental cache and cascade-deletion ledger
-- ---------------------------------------------------------------------------

CREATE TABLE sources (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    connector_id  UUID REFERENCES connectors(id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    kind          TEXT NOT NULL,
    title         TEXT,
    origin        TEXT,
    provenance    TEXT NOT NULL DEFAULT 'source',
    input_hash    TEXT NOT NULL,
    files_written JSONB NOT NULL DEFAULT '[]'::jsonb,
    blob_keys     JSONB NOT NULL DEFAULT '[]'::jsonb,
    fetched_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ,
    UNIQUE (workspace_id, key)
);
CREATE INDEX sources_connector_idx ON sources (connector_id);

-- ---------------------------------------------------------------------------
-- Runs and spend
-- ---------------------------------------------------------------------------

CREATE TABLE runs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id   UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    connector_id   UUID REFERENCES connectors(id) ON DELETE SET NULL,
    trigger        TEXT NOT NULL,
    ref            TEXT,
    status         TEXT NOT NULL DEFAULT 'pending',
    cost_usd       NUMERIC(10,4) NOT NULL DEFAULT 0,
    tokens         BIGINT NOT NULL DEFAULT 0,
    pages_created  INTEGER NOT NULL DEFAULT 0,
    pages_updated  INTEGER NOT NULL DEFAULT 0,
    pages_deleted  INTEGER NOT NULL DEFAULT 0,
    error          TEXT,
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX runs_workspace_created_idx ON runs (workspace_id, created_at DESC);

ALTER TABLE wikis
    ADD CONSTRAINT wikis_last_run_fk
    FOREIGN KEY (last_run_id) REFERENCES runs(id) ON DELETE SET NULL;

CREATE TABLE run_items (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    cache_key    TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',
    cost_usd     NUMERIC(10,4) NOT NULL DEFAULT 0,
    est_cost_usd NUMERIC(10,4),
    turns        INTEGER NOT NULL DEFAULT 0,
    error        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);
CREATE INDEX run_items_run_idx ON run_items (run_id);

CREATE TABLE spend_ledger (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    run_id       UUID REFERENCES runs(id) ON DELETE SET NULL,
    amount_usd   NUMERIC(10,4) NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX spend_ledger_workspace_time_idx ON spend_ledger (workspace_id, occurred_at DESC);

-- ---------------------------------------------------------------------------
-- Human steering
-- ---------------------------------------------------------------------------

-- purpose and schema documents, injected into every prompt. This is the main
-- lever for changing a wiki's character without touching code.
CREATE TABLE steering_docs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('purpose','schema')),
    body         TEXT NOT NULL DEFAULT '',
    updated_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, kind)
);

-- Pages are never hand-edited -- an edit would be clobbered on regeneration --
-- so corrections are pinned here and re-injected into every future prompt.
CREATE TABLE page_corrections (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    page_id    UUID NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX page_corrections_page_idx ON page_corrections (page_id) WHERE active;

CREATE TABLE review_items (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    run_id       UUID REFERENCES runs(id) ON DELETE SET NULL,
    page_id      UUID REFERENCES pages(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    title        TEXT NOT NULL,
    detail       TEXT NOT NULL DEFAULT '',
    actions      JSONB NOT NULL DEFAULT '[]'::jsonb,
    status       TEXT NOT NULL DEFAULT 'open',
    resolved_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    resolved_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX review_items_workspace_open_idx ON review_items (workspace_id) WHERE status = 'open';

CREATE TABLE digests (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    channel      TEXT NOT NULL,
    target       TEXT NOT NULL,
    cadence      TEXT NOT NULL DEFAULT 'weekly',
    enabled      BOOLEAN NOT NULL DEFAULT FALSE,
    last_sent_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Access tokens (REST + MCP)
-- ---------------------------------------------------------------------------

CREATE TABLE tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       TEXT[] NOT NULL DEFAULT ARRAY['read'],
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX tokens_user_idx ON tokens (user_id);
