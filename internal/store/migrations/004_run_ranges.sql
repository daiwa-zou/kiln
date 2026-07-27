-- M5: incremental builds. A queued run may carry the pushed head it should
-- build toward (ref_to) and an earliest start time (not_before), which
-- replaces the webhook completion cooldown's skip-and-drop behavior: pushes
-- always enqueue, debounced pushes advance ref_to on the waiting run, and
-- the claim query simply ignores runs whose time has not come.
--
-- ref_from is deliberately NOT stored: the worker derives it from the
-- workspace's last successful build ref at claim time, so a debounced or
-- dropped push can never leave a hole in the diffed range.

ALTER TABLE runs
    ADD COLUMN ref_to TEXT,
    ADD COLUMN not_before TIMESTAMPTZ;
