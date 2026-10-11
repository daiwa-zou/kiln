-- Review items an agent files through MCP, and the corrections a human pins
-- from them.
--
-- An agent may now ask a question about a page or propose a correction to it,
-- but nothing it sends changes a page: a proposal is a review item, and only a
-- person promoting it pins a correction. Three things have to be recorded for
-- that to hold up.
--
-- `proposed` is the text of a proposed correction, kept apart from `detail`
-- (the agent's rationale) so promotion pins exactly the text the human read
-- and confirmed, never the rationale around it.
--
-- `origin` is who filed the item, for display only: the key's name, or
-- "session" / "local". It is free text a key's minter chose, so nothing
-- decides anything on it. `origin_kind` says what filed it -- 'key', 'session'
-- or 'local', empty for items the pipeline raised -- and is what the inbox
-- badge and the bench-wide cap read. `origin_token` is the filing key's id,
-- which the per-key cap counts: names repeat, and a key named after someone
-- else's must not eat into their allowance.
--
-- No foreign key on origin_token, on purpose. The housekeeping sweep deletes
-- tokens a week after they are revoked or expire, and ON DELETE SET NULL
-- would then drop that key's open items out of both caps and off the badge --
-- an agent could escape its cap by having its key revoked. The id is kept as
-- a plain value: an item records which key filed it, whether or not that key
-- still exists.
--
-- `page_corrections.proposed_by` records that a pinned correction began as a
-- proposal (the filing key's name), so the prompt can say "proposed by an
-- agent, confirmed by a member" rather than presenting promoted agent text as
-- something a member wrote. Empty for every correction a member wrote.
--
-- All additive with defaults: existing rows read as member-written and
-- human-filed, which is what they are.

ALTER TABLE review_items
    ADD COLUMN proposed     TEXT NOT NULL DEFAULT '',
    ADD COLUMN origin       TEXT NOT NULL DEFAULT '',
    ADD COLUMN origin_kind  TEXT NOT NULL DEFAULT ''
        CHECK (origin_kind IN ('', 'key', 'session', 'local')),
    ADD COLUMN origin_token UUID;

ALTER TABLE page_corrections
    ADD COLUMN proposed_by TEXT NOT NULL DEFAULT '';

-- The caps count a bench's open agent-filed items, in total and per key, on
-- every filing. Partial on exactly that predicate, so the index holds only the
-- handful of rows the caps bound and human-filed history never enters it.
CREATE INDEX review_items_agent_open_idx ON review_items (workspace_id, origin_token)
    WHERE status = 'open' AND origin_kind = 'key';
