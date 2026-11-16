-- retire_linucb_checkpoints.sql
--
-- STAGED. This file is deliberately outside internal/db/migrations/, so it is
-- not embedded and cannot run on boot. It takes the next free number when it is
-- promoted into migrations/.
--
-- linucb_checkpoints was AethyrRank's LinUCB state, written into feed-engine's
-- database by a process that did not own it. AethyrRank now owns its state in
-- its own database, f33d3r_rank (aethyrrank-engine/migrations/0001_rank_state.sql),
-- and nothing in feed-engine reads or writes this table.
--
-- ── The order ────────────────────────────────────────────────────────────────
--
--   1. The AethyrRank that keeps its state in f33d3r_rank is deployed and has
--      booted once (it creates and migrates f33d3r_rank). The old AethyrRank,
--      which wrote this table, is stopped.
--
--   2. The import has run and verified, exiting 0:
--
--        docker compose run --rm \
--          -e FEED_DATABASE_URL=postgres://…/f33d3r_feed \
--          aethyrrank-engine ./aethyrrank-engine import-feed-checkpoints
--
--      It records every model it verified in f33d3r_rank in
--      linucb_checkpoints_exported, here. A row it could not decode is listed
--      and the import exits non-zero: repair or delete that row and run it again.
--
--   3. Promote this file into migrations/ under the next free number.
--
-- ── The guard ────────────────────────────────────────────────────────────────
-- Refuses to drop anything while a checkpoint exists that the import has not
-- verified in f33d3r_rank. A migration that fails is fatal at boot, so a
-- premature promotion stops the deploy instead of losing learned state.

DO $$
BEGIN
    IF to_regclass('public.linucb_checkpoints') IS NOT NULL THEN
        IF to_regclass('public.linucb_checkpoints_exported') IS NULL THEN
            RAISE EXCEPTION 'linucb_checkpoints has not been imported into f33d3r_rank: run aethyrrank-engine import-feed-checkpoints first';
        END IF;
        IF EXISTS (
            SELECT 1 FROM linucb_checkpoints c
             WHERE NOT EXISTS (SELECT 1 FROM linucb_checkpoints_exported e
                                WHERE e.model_key = c.surface)
        ) THEN
            RAISE EXCEPTION 'linucb_checkpoints holds models the import has not verified in f33d3r_rank: run aethyrrank-engine import-feed-checkpoints until it exits 0';
        END IF;
    END IF;
END $$;

DROP TABLE IF EXISTS linucb_checkpoints;
DROP TABLE IF EXISTS linucb_checkpoints_exported;
