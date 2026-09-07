-- 0011_work_kind_vocabulary.sql
--
-- The Work kind is a closed, server-owned vocabulary. The database enforces it
-- so no path can store a kind no read path filters and no Facet renders.

-- ── retire the vision lane ───────────────────────────────────────────────────
-- kind='vision' moved to the fleets table in 0008, which soft-deleted the rows
-- it left behind. Those rows keep their media identity rather than a dead name.
UPDATE works SET kind = 'video' WHERE kind = 'vision';

-- Anything else outside the vocabulary predates it and has no renderer, so it
-- is a post. Recorded here rather than repaired silently at read time.
UPDATE works
   SET kind = 'post'
 WHERE kind NOT IN ('post', 'reply', 'quote', 'poll', 'video', 'voice', 'thread_post', 'react_video');

-- ── the vocabulary ───────────────────────────────────────────────────────────
-- Mirrors handler.workKinds. Change one and the other must change with it.
ALTER TABLE works DROP CONSTRAINT IF EXISTS works_kind_vocabulary;
ALTER TABLE works ADD CONSTRAINT works_kind_vocabulary
    CHECK (kind IN ('post', 'reply', 'quote', 'poll', 'video', 'voice', 'thread_post', 'react_video'));

-- ── the scan verdict is a verdict ────────────────────────────────────────────
-- works.scan_state opened as 'clean' at insert while the scan was still in
-- flight, so every work carried a verdict nothing had reached. It now opens as
-- 'pending' and the scan transitions it. Legacy rows carrying the posts-table
-- spelling are normalised onto the works vocabulary.
UPDATE works SET scan_state = 'pending' WHERE scan_state IN ('pending_scan', '') OR scan_state IS NULL;

ALTER TABLE works DROP CONSTRAINT IF EXISTS works_scan_state_vocabulary;
ALTER TABLE works ADD CONSTRAINT works_scan_state_vocabulary
    CHECK (scan_state IN ('pending', 'clean', 'age_gated', 'flagged', 'human_review', 'blocked'));

-- The pending sweep and the moderation queue both scan by state; before this
-- there was no index for either because nothing was ever pending.
CREATE INDEX IF NOT EXISTS idx_works_scan_state_open
    ON works (scan_state, created_at)
 WHERE deleted_at IS NULL AND scan_state IN ('pending', 'flagged', 'human_review');

-- ── the vision achievements now count Fleets ─────────────────────────────────
-- The ids stay: they are stable keys already referenced by earned rows. Only
-- the text a user reads changes, so it names the thing that still exists.
UPDATE achievements SET name = 'First Fleet',  description = 'Posted your first Fleet'  WHERE id = 'vision_first';
UPDATE achievements SET name = 'Fleet Seer',   description = '10 Fleets posted'         WHERE id = 'vision_seer';
UPDATE achievements SET name = 'Fleet Oracle', description = '50 Fleets posted'         WHERE id = 'vision_oracle';
UPDATE achievements SET name = 'Fleet Quest',  description = '7-day Fleet posting streak' WHERE id = 'vision_quest';
UPDATE achievements SET description = 'A Fleet hit 100 views'   WHERE id = 'vision_sight';
UPDATE achievements SET description = 'A Fleet hit 1,000 views' WHERE id = 'vision_revelation';
