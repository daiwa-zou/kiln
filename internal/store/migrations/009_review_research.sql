-- Research runs: the worker answering a review item instead of only asking it.
--
-- The review queue has always been one-directional. A build that finds a
-- contradiction between two sources, an assertion it cannot corroborate, or a
-- page the wiki refers to and has never written files the question and stops
-- there. The human who opens the queue gets the question and none of the work
-- behind answering it -- and the one reader who has already read the whole
-- corpus, and could answer most of these in a single pass, is the agent that
-- raised the flag.
--
-- These columns are the return path. `runs.review_id` marks a run as existing
-- to answer one review item rather than to build pages, which is what the
-- worker branches on after claiming it; the FK cascades because a research run
-- outliving its question is a run with nothing to do. `review_items.research`
-- holds what came back, and `research_at` says when, so a card can show the
-- answer beside the question and a human decides with the reading already done.
--
-- Note what is *not* here: a status for "being researched". An item with a
-- worker reading for it is still an open question -- nobody has answered it --
-- so it stays 'open' and in the inbox, and whether a read is in flight is
-- derived from the run queue rather than stored. A derived flag cannot get
-- stuck: a worker that dies mid-read leaves a run the stale-requeue handles,
-- where a stored one would leave a question the queue refuses to let anyone
-- answer.
--
-- Nothing here resolves anything on its own. Research is evidence, not
-- authority: the item keeps its place in the queue with findings attached
-- unless the pass reports the question conclusively settled, and an approved
-- deletion still needs a human to approve it.
ALTER TABLE runs
    ADD COLUMN review_id UUID REFERENCES review_items(id) ON DELETE CASCADE;

ALTER TABLE review_items
    ADD COLUMN research    TEXT NOT NULL DEFAULT '',
    ADD COLUMN research_at TIMESTAMPTZ;
