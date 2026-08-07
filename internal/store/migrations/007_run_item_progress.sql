-- Run items become progress rather than a post-mortem.
--
-- They were written once, in the transaction that finished the run, so a build
-- in flight had nothing to show: the dashboard could say "running" and no more,
-- and a bench ingesting a large repository looked identical five seconds and
-- five minutes in. The plan is now seeded as pending rows before any unit runs
-- and each row is settled as its unit reaches a conclusion, which makes both
-- halves of the question -- what is done, what is still coming -- answerable
-- from the table at any moment.
--
-- That means the same row is written more than once, so it needs an identity.
-- A unit key appears at most once per run by construction (the plan is a set),
-- which was true before and merely unenforced.

CREATE UNIQUE INDEX run_items_run_key_idx ON run_items (run_id, cache_key);

-- Ordering the queue view by insertion is what keeps a partially built run
-- listing in plan order instead of jumping around as costs land.
CREATE INDEX run_items_run_created_idx ON run_items (run_id, created_at);
