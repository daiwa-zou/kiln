-- A readable label for an uploaded document, derived from its filename at
-- upload time.
--
-- path stays exactly what it was: the sanitized relative path the file
-- materializes under, the unit key the pipeline derives, and the column the
-- row is unique on. display_name never participates in identity -- two uploads
-- can legitimately derive the same label, which is the point of collapsing
-- "budget_v1" and "budget_v2" -- so it is a display concern only.
--
-- Empty for rows uploaded before this migration. The API falls back to the
-- path, so old rows read exactly as they did before rather than showing a
-- blank name, and there is no backfill to get wrong: a filename is all the
-- derivation ever reads, so an old row's label can be computed on demand if
-- anyone ever wants it.

ALTER TABLE workspace_files ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
