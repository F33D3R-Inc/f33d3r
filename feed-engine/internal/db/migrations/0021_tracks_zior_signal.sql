-- 0021_tracks_zior_signal.sql
--
-- A music track gets its place on the Jung axes.
--
-- Migration 0020 gave works a psych_vector, computed from the work's text by
-- internal/jung.MapWork. A track in tracks is not a work and has no text
-- worth mapping: the audio is the whole of it. Zior (zior-engine) listens to
-- the file at upload and produces the same 8-axis vector —
--
--   [persona, shadow, agency, integration, attachment, disruption, tension, release]
--
-- — plus the mood, the genre-free descriptor and the listening contexts it
-- infers. Those land here, on the track's own row, keyed by the track id
-- Zior stored the signal under, so /signal/{id} and /similar/{id} on Zior
-- and this row describe the same thing.
--
-- psych_vector is NULL until Zior has answered; a NULL means "not yet heard",
-- never a default vector. zior_analyzed_at says when the answer arrived.

ALTER TABLE tracks ADD COLUMN IF NOT EXISTS psych_vector     REAL[];
ALTER TABLE tracks ADD COLUMN IF NOT EXISTS mood             TEXT        NOT NULL DEFAULT '';
ALTER TABLE tracks ADD COLUMN IF NOT EXISTS descriptor       TEXT        NOT NULL DEFAULT '';
ALTER TABLE tracks ADD COLUMN IF NOT EXISTS context_tags     TEXT[]      NOT NULL DEFAULT '{}';
ALTER TABLE tracks ADD COLUMN IF NOT EXISTS zior_analyzed_at TIMESTAMPTZ;
