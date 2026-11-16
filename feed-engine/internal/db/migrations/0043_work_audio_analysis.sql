-- 0043_work_audio_analysis.sql
--
-- Zior's analysis of an audio work, kept on the work.
--
-- Zior (zior-engine) is the audio-analysis service: it listens to a track or a
-- voice work and answers with its place on the eight Jung axes, its mood, its
-- genre-free descriptor, the listening contexts it suggests, its tempo and its
-- key. Zior keeps nothing: it is stateless, and a restart of it loses nothing.
-- The zior_voice obligation stores the whole answer here, on the work, in one
-- write together with the psych_vector it replaces.
--
-- 0021 gave the old tracks table these columns; 0037 made tracks works without
-- carrying them, so until now everything but the vector was dropped.
--
-- audio_analyzed_at is NULL until Zior has answered. A NULL means "not yet
-- heard", never "heard nothing".
ALTER TABLE works ADD COLUMN IF NOT EXISTS audio_mood         TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS audio_descriptor   TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS audio_context_tags TEXT[];
ALTER TABLE works ADD COLUMN IF NOT EXISTS audio_bpm          REAL;
ALTER TABLE works ADD COLUMN IF NOT EXISTS audio_key          TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS audio_analyzed_at  TIMESTAMPTZ;

ALTER TABLE works DROP CONSTRAINT IF EXISTS works_audio_analysis_whole;
ALTER TABLE works ADD CONSTRAINT works_audio_analysis_whole CHECK (
    (audio_analyzed_at IS NULL
        AND audio_mood IS NULL AND audio_descriptor IS NULL
        AND audio_context_tags IS NULL AND audio_bpm IS NULL AND audio_key IS NULL)
 OR (audio_analyzed_at IS NOT NULL
        AND audio_mood IS NOT NULL AND audio_descriptor IS NOT NULL
        AND audio_context_tags IS NOT NULL AND audio_bpm IS NOT NULL AND audio_key IS NOT NULL)
);
