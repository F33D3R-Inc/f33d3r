-- 0024_vision_achievement_names.sql
--
-- 0010_work_kind_vocabulary.sql overwrote these six achievements' display
-- text with Fleet branding ("First Fleet", "Fleet Seer", ...) despite their
-- ids already being vision_*. This restores 0001_baseline.sql's original
-- wording, undoing that drift now that Fleet is Vision everywhere else.

UPDATE achievements SET name = 'The Awakening', description = 'Posted your first Vision' WHERE id = 'vision_first';
UPDATE achievements SET name = 'Seer',          description = '10 Visions posted'         WHERE id = 'vision_seer';
UPDATE achievements SET name = 'The Oracle',    description = '50 Visions posted'         WHERE id = 'vision_oracle';
UPDATE achievements SET name = 'Vision Quest',  description = '7-day Vision posting streak' WHERE id = 'vision_quest';
UPDATE achievements SET description = 'A Vision hit 100 views'   WHERE id = 'vision_sight';
UPDATE achievements SET description = 'A Vision hit 1,000 views' WHERE id = 'vision_revelation';
