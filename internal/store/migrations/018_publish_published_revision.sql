-- The wiki revision the repository last matched.
--
-- Publishing runs after every build, and on an established bench most builds
-- change nothing: a poll finds no new source and the wiki is untouched. Without
-- this column the only way to learn that was to load every page body and every
-- figure blob, clone the repository, stage the tree, and find there was nothing
-- to commit.
--
-- wikis.revision already moves on every path that changes what a mirror would
-- contain -- an import, a page delete, a restore, a source-delete cascade -- so
-- "the revision the repository last matched" is enough to say whether a push
-- could possibly do anything.
--
-- Written only by a successful push (or one that found the repository already
-- identical). A failed push leaves it where it was, so the next build retries
-- instead of believing the mirror is current. Zero means never published, and
-- is also what reconfiguring the target resets it to: a different repository,
-- branch or path has not been given anything yet.

ALTER TABLE publish_targets
    ADD COLUMN published_revision BIGINT NOT NULL DEFAULT 0;
