-- The source a review came from, as data rather than as a sentence.
--
-- A review raised while generating a unit recorded which unit by appending
-- "(raised while generating doc:upload:report.pdf)" to the end of its detail
-- text. Two things were wrong with that. The reader met a cache key -- the
-- namespacing that keeps an uploaded README from colliding with a repo one,
-- which is true and none of their business -- and the provenance was welded
-- into prose, so no surface could render it as anything but that sentence.
--
-- As a column it can be shown the way the ingest view shows the same fact: the
-- source as a glyph and the document under its own name.
--
-- The backfill moves existing rows rather than leaving two shapes to render.
-- The pattern is exact and anchored, so a detail that merely discusses the
-- phrase is not rewritten, and a row that never carried a unit keeps an empty
-- string -- the same thing a review raised outside unit generation stores.
ALTER TABLE review_items ADD COLUMN unit TEXT NOT NULL DEFAULT '';

UPDATE review_items
SET unit   = substring(detail from '\(raised while generating (.+)\)\s*$'),
    detail = regexp_replace(detail, '\s*\(raised while generating .+\)\s*$', '')
WHERE detail ~ '\(raised while generating .+\)\s*$';
