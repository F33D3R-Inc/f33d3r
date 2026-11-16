-- 0041_retire_feed_engine_media_registry.sql
--
-- STAGED. This file is deliberately outside internal/db/migrations/, so it is
-- not embedded and cannot run on boot. It drops data that other processes must
-- have finished reading first, and a forward-only migration cannot be taken
-- back once it has dropped a table.
--
-- ── The order ────────────────────────────────────────────────────────────────
--
--   1. Caeor (Team M-A) is deployed and its import has run to completion: every
--      canonical_media row, every raw hash and every legacy file is a Caeor
--      asset with its original uploader and its withdrawal state, and every
--      legacy path resolves through GET /v1/assets/resolve?path=. Imported
--      legacy media keeps its existing path and is servable only once
--      `caeor remark` has watermarked it in place: run the re-mark here, or
--      legacy media is served to nobody.
--
--   2. feed-engine is deployed with 0036 and 0037. 0037 queues media_bind for
--      every work that names local media; the work outbox binds each one to its
--      Caeor assets. A bind Caeor cannot answer backs off and, after
--      WorkOutboxMaxAttempts, is quarantined. If step 1 had not finished, put
--      the quarantined binds back once it has:
--
--        UPDATE work_outbox
--           SET quarantined_at = NULL, quarantine_reason = NULL,
--               attempts = 0, next_attempt_at = NOW()
--         WHERE action = 'media_bind' AND done_at IS NULL;
--
--   3. Verify, and resolve by hand whatever is listed:
--
--        -- binds not yet done
--        SELECT work_id, attempts, last_error FROM work_outbox
--         WHERE action = 'media_bind' AND done_at IS NULL;
--
--        -- live works that carry local media but no media references
--        -- (imported media keeps its paths, so a path's shape says nothing;
--        -- a bound work has work_assets rows)
--        SELECT id FROM works w
--         WHERE w.deleted_at IS NULL
--           AND NOT EXISTS (SELECT 1 FROM work_assets wa WHERE wa.work_id = w.id)
--           AND (   EXISTS (SELECT 1 FROM unnest(w.media_urls) m(url) WHERE m.url LIKE '/%')
--                OR COALESCE(w.voice_url, '')        LIKE '/%'
--                OR COALESCE(w.video_master_url, '') LIKE '/%'
--                OR COALESCE(w.video_poster_url, '') LIKE '/%');
--
--        -- tracks that did not become works (their author has no PIAL)
--        SELECT t.id FROM tracks t WHERE NOT EXISTS (SELECT 1 FROM works w WHERE w.id = t.id);
--
--   4. Abraxas's import (abraxas-shield/src/import.rs) has copied
--      banned_content_hashes into f33d3r_integrity's banned_hashes. The
--      banned registry is Abraxas's; feed-engine neither reads nor writes the
--      local table (its admin route and cmd/banned-hashes call POST /v1/banned).
--      Verify the copy before moving this file in: every row here must be in
--      banned_hashes, matched by its value.
--
--   5. Move this file into internal/db/migrations/, renumbered above the highest
--      version applied at that time (above Team I-B's staged
--      0040_retire_identity_tables.sql if that has moved in first), and deploy.
--      The guard below repeats step 3 (step 4 is verified by hand, in Abraxas) and refuses to drop anything while any of
--      it still lists a row, so moving the file early fails the boot instead of
--      losing data.

DO $$
DECLARE
    open_binds    BIGINT;
    unbound_works BIGINT;
    stray_tracks  BIGINT;
BEGIN
    SELECT COUNT(*) INTO open_binds
      FROM work_outbox
     WHERE action = 'media_bind' AND done_at IS NULL;

    SELECT COUNT(*) INTO unbound_works
      FROM works w
     WHERE w.deleted_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM work_assets wa WHERE wa.work_id = w.id)
       AND (   EXISTS (SELECT 1 FROM unnest(w.media_urls) m(url) WHERE m.url LIKE '/%')
            OR COALESCE(w.voice_url, '')        LIKE '/%'
            OR COALESCE(w.video_master_url, '') LIKE '/%'
            OR COALESCE(w.video_poster_url, '') LIKE '/%');

    SELECT COUNT(*) INTO stray_tracks
      FROM tracks t
     WHERE NOT EXISTS (SELECT 1 FROM works w WHERE w.id = t.id);

    IF open_binds > 0 OR unbound_works > 0 OR stray_tracks > 0 THEN
        RAISE EXCEPTION
            'refusing to retire the local media registry: % media_bind obligation(s) open, '
            '% live work(s) with local media and no media references, % track(s) not carried into works '
            '— see the order at the top of this migration',
            open_binds, unbound_works, stray_tracks;
    END IF;
END
$$;

-- ── the local media registry: Caeor owns uploaders, duplicates, withdrawals ──
DROP TABLE IF EXISTS canonical_media;
DROP TABLE IF EXISTS video_raw_hashes;
DROP TABLE IF EXISTS image_raw_hashes;

-- ── the banned registry: Abraxas owns it ─────────────────────────────────────
DROP TABLE IF EXISTS banned_content_hashes;

-- ── the tracks lane: every track is a work ───────────────────────────────────
DROP TABLE IF EXISTS track_likes;
DROP TABLE IF EXISTS tracks;

-- ── the client-named downloadable file ───────────────────────────────────────
-- works.video_watermarked_url held a value the client sent at publish. A
-- download is Caeor's, by asset, behind the download route; nothing reads it.
ALTER TABLE works DROP COLUMN IF EXISTS video_watermarked_url;
