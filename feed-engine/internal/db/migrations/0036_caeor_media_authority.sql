-- 0036_caeor_media_authority.sql
--
-- Caeor becomes the one media-asset authority. It stores every uploaded byte,
-- burns the watermark into every copy a viewer is served, resolves exact-byte
-- duplicates to the original asset (the original uploader keeps the credit),
-- answers which asset a served path belongs to, and withdraws assets. This
-- brain keeps only what is decided about WORKS:
--
--   * which works reference which assets               work_assets
--   * the wall in front of a work                       (the works row itself)
--   * whether the author lets a work be downloaded      works.downloads_allowed
--   * that a deleted work owes Caeor a withdrawal       media_withdrawals
--
-- The local registry (canonical_media) and the raw-hash stores
-- (video_raw_hashes, image_raw_hashes) stop being read or written by this
-- release. They are NOT dropped here: Caeor's import reads them to bring every
-- existing asset, uploader and withdrawal across, and the works that name
-- legacy paths are re-bound to Caeor assets by the media_bind obligation
-- (0037 queues it). Only once both have finished does the staged migration
-- internal/db/migrations_staged/0040_retire_feed_engine_media_registry.sql
-- move into migrations/ and drop them. See that file for the order.

-- ── 1. downloads are a per-work creator setting, allowed by default ───────────
-- Every download is watermarked; a subscriber-only work's download also carries
-- the per-subscriber forensic layer. The author may turn downloads off per work.
ALTER TABLE works ADD COLUMN IF NOT EXISTS downloads_allowed BOOLEAN NOT NULL DEFAULT TRUE;

-- ── 2. a music track is a work ───────────────────────────────────────────────
-- Same pipeline as any work — signed, scanned, fanned out — with its own Facets.
-- Mirrors handler.workKinds. Change one and the other must change with it.
ALTER TABLE works DROP CONSTRAINT IF EXISTS works_kind_vocabulary;
ALTER TABLE works ADD CONSTRAINT works_kind_vocabulary
    CHECK (kind IN ('post', 'reply', 'quote', 'poll', 'video', 'voice', 'thread_post', 'react_video', 'track'));

-- ── 3. work_assets: which works reference which Caeor assets ──────────────────
-- One row per media slot of a work. uploader_pial is Caeor's uploader of the
-- asset, copied at publish: it is what the media gate filters on (only the
-- uploader's own works govern an asset — a work by anybody else that names it is
-- a credited reference and unlocks nothing) and what decides which assets a
-- deleted work withdraws (only its author's own).
--
-- slot     image   one of the work's images (position = its index in media_urls)
--          voice   the work's audio: a voice recording or a music track
--          video   the work's video (HLS master)
--          poster  the poster of the work's video
CREATE TABLE IF NOT EXISTS work_assets (
    work_id       UUID        NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    slot          TEXT        NOT NULL,
    position      INT         NOT NULL DEFAULT 0,
    asset_id      UUID        NOT NULL,
    uploader_pial UUID        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (work_id, slot, position),
    CONSTRAINT work_assets_slot_vocabulary CHECK (slot IN ('image', 'voice', 'video', 'poster')),
    CONSTRAINT work_assets_position_nonnegative CHECK (position >= 0)
);

-- The media gate's question: the works of this uploader that reference this asset.
CREATE INDEX IF NOT EXISTS idx_work_assets_asset_uploader
    ON work_assets (asset_id, uploader_pial);

-- ── 4. media_bind: re-binding a legacy work to Caeor assets ───────────────────
-- A work created before this release names its media by legacy paths and has no
-- work_assets rows. Its media_bind obligation resolves every path through Caeor,
-- rewrites the work to the watermarked objects Caeor serves, records its
-- work_assets and its "Media by" credit. 0037 queues it.
-- Mirrors db.WorkObligation (work_outbox.go). Change one and the other must change with it.
ALTER TABLE work_outbox DROP CONSTRAINT IF EXISTS work_outbox_action_vocabulary;
ALTER TABLE work_outbox ADD CONSTRAINT work_outbox_action_vocabulary CHECK (action IN (
    'content_event',      -- Sitra Achra content.events: the moderation scan
    'follower_fanout',    -- FA Live new_post to followers
    'cited_update',       -- FA Live counts and reply row of the cited work
    'alexandria_catalog', -- Alexandria title
    'link_preview',       -- link_previews cache
    'zior_voice',         -- Zior audio vector of a voice work or a track
    'interest_learning',  -- author's interest vector toward what they cite
    'achievements',       -- post-creation achievements
    'media_bind'          -- re-bind a legacy work's media paths to Caeor assets
));

-- ── 5. media_withdrawals: what deleted works owe Caeor ────────────────────────
-- Deleting a work (by its author or by an admin) WITHDRAWS the author's own
-- assets of that work; it never frees them for re-upload credit. The row is
-- written in the delete's own transaction and drained by a worker that calls
-- Caeor, with backoff and quarantine like work_outbox. There is deliberately no
-- foreign key to works: an admin delete removes the works row in the same
-- transaction, and the debt must outlive it.
--
-- One row per asset: an asset is withdrawn once, whichever work's deletion
-- asked first.
--
--   what is stuck:  SELECT asset_id, work_id, attempts, last_error, quarantined_at
--                     FROM media_withdrawals
--                    WHERE quarantined_at IS NOT NULL AND done_at IS NULL;
--
--   put one back:   UPDATE media_withdrawals
--                      SET quarantined_at = NULL, attempts = 0, next_attempt_at = NOW()
--                    WHERE asset_id = $1;
CREATE TABLE IF NOT EXISTS media_withdrawals (
    asset_id        UUID        PRIMARY KEY,
    work_id         UUID        NOT NULL,
    reason          TEXT        NOT NULL,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    done_at         TIMESTAMPTZ,
    quarantined_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_media_withdrawals_due
    ON media_withdrawals (next_attempt_at, asset_id)
 WHERE done_at IS NULL AND quarantined_at IS NULL;

-- ── 6. work_play_totals: how many times a track has been played ───────────────
-- A play is counted by the server for a signed-in listener; the count is a
-- Fragment pushed to everyone watching the track.
CREATE TABLE IF NOT EXISTS work_play_totals (
    work_id    UUID        PRIMARY KEY REFERENCES works(id) ON DELETE CASCADE,
    plays      BIGINT      NOT NULL DEFAULT 0 CHECK (plays >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
