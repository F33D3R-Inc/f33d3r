-- 0020_work_psych_vector.sql
--
-- The Jung layer gets a place to live.
--
-- user_profiles.interest_vector (0001) has always been described as an
-- 8-dimensional psychological vector, and AethyrRank has always scored
-- alignment as the cosine between it and a work's topic_vector. Until now
-- neither side of that cosine was a psychological vector: the interest vector
-- was an average of hashes of author ids, and the topic vector was a hash of
-- the author id. Two hashes agree about nothing, so the score meant nothing,
-- and the feeds were served chronologically with the ranker never called.
--
-- This migration gives a work its own vector on the eight axes Zior defines
-- (zior-engine/src/jung/axes.rs, mirrored in internal/jung):
--
--   [persona, shadow, agency, integration, attachment, disruption, tension, release]
--
-- computed from the work itself — its words, its structure, its kind and
-- media, its tags, its adult flags — by internal/jung.MapWork. The same eight
-- axes carry a person's interest vector, which from now on is an online
-- average of the vectors of the works they engage with, so the cosine between
-- the two is a real statement about compatibility.
--
-- psych_vector is NULL for a work that predates this migration or was inserted
-- by a path that did not compute it. A NULL is not a default vector: the feed
-- computes the vector when it first serves such a work and writes it back, so
-- the column converges without a backfill job walking every row on boot.
--
-- Engagement counts are NOT denormalised here. Migration 0004 settled that a
-- work's tallies are counted from work_reactions and work_citations at read
-- time, and the feed already carries those counts on every candidate; a second
-- copy under a different name would be exactly the drifting counter 0004
-- removed. The ranker's engagement signals are built from the counts the work
-- select already returns.

ALTER TABLE works ADD COLUMN IF NOT EXISTS psych_vector REAL[];

-- The lazy backfill finds its work by this predicate and nothing else scans
-- on it, so the index is partial: it only ever holds the rows still waiting.
CREATE INDEX IF NOT EXISTS idx_works_psych_vector_missing
  ON works(created_at DESC)
  WHERE psych_vector IS NULL AND deleted_at IS NULL;

-- When the interest vector last moved. The value itself is written by every
-- reaction; this says how current it is, so a stale vector can be told apart
-- from a fresh one without comparing floats.
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS interest_updated_at TIMESTAMPTZ;
