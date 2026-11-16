-- 0034_media_uploader_registry.sql
--
-- canonical_media becomes the uploader registry the media gate decides from.
-- Media is governed by the works of the account that uploaded it; a work by
-- anybody else that names the same URL unlocks nothing. For that the registry has
-- to locate every kind of work media, not only images:
--
--   image_url        still image (existing)
--   audio_url        voice recording
--   master_url       single-file video, e.g. a react-video recording (existing)
--   asset_dir        transcoded video: every object under the directory
--   watermarked_url  the transcoder's downloadable watermarked MP4 for asset_dir,
--                    recorded server-side so a download never trusts a client URL
--   raw_sha256       the raw upload the asset was made from
--   upload_id        the upload that registered the asset
--   withdrawn_at     the asset is off the platform; served to nobody

ALTER TABLE canonical_media ADD COLUMN IF NOT EXISTS audio_url       TEXT NOT NULL DEFAULT '';
ALTER TABLE canonical_media ADD COLUMN IF NOT EXISTS asset_dir       TEXT NOT NULL DEFAULT '';
ALTER TABLE canonical_media ADD COLUMN IF NOT EXISTS watermarked_url TEXT NOT NULL DEFAULT '';
ALTER TABLE canonical_media ADD COLUMN IF NOT EXISTS raw_sha256      TEXT NOT NULL DEFAULT '';
ALTER TABLE canonical_media ADD COLUMN IF NOT EXISTS upload_id       TEXT NOT NULL DEFAULT '';
ALTER TABLE canonical_media ADD COLUMN IF NOT EXISTS withdrawn_at    TIMESTAMPTZ;

CREATE UNIQUE INDEX IF NOT EXISTS idx_canon_audio_url ON canonical_media(audio_url) WHERE audio_url <> '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_canon_asset_dir ON canonical_media(asset_dir) WHERE asset_dir <> '';
CREATE INDEX        IF NOT EXISTS idx_canon_raw_sha256 ON canonical_media(raw_sha256) WHERE raw_sha256 <> '';

-- ── Backfill ─────────────────────────────────────────────────────────────────
-- Media uploaded before this migration has no uploader on record except images
-- (written at upload time). The uploader of everything else is taken to be the
-- author of the earliest work that references it — the first reference to an
-- object is its uploader's own post, because nobody else can know the URL before
-- that post exists. For transcoded video the work the raw-hash store links to
-- under the author's own PIAL is preferred over creation order: that link was
-- made by the uploader's own publish.
--
-- Deleted works count: an uploader whose work is deleted is still the uploader,
-- and the gate then finds only deleted works governing the media and refuses it.

-- Video rows that already name an HLS master get the asset directory it sits in.
UPDATE canonical_media cm
   SET asset_dir = d.dir,
       watermarked_url = CASE WHEN d.dir LIKE '/static/media/posts/%'
                              THEN d.dir || '/watermarked.mp4' ELSE '' END
  FROM (
        SELECT DISTINCT ON (dir) id, dir
          FROM (SELECT id, created_at,
                       regexp_replace(master_url, '/(clean/)?master\.m3u8$', '') AS dir
                  FROM canonical_media
                 WHERE asset_dir = '' AND master_url ~ '^/.+/master\.m3u8$') x
         ORDER BY dir, created_at, id
       ) d
 WHERE cm.id = d.id
   AND NOT EXISTS (SELECT 1 FROM canonical_media o WHERE o.asset_dir = d.dir);

-- Transcoded (HLS) video referenced by works.
INSERT INTO canonical_media (creator_user_id, creator_pial_id, asset_dir, watermarked_url, raw_sha256, first_post_id)
SELECT DISTINCT ON (r.dir)
       r.author_id, u.pial_id, r.dir,
       CASE WHEN r.dir LIKE '/static/media/posts/%' THEN r.dir || '/watermarked.mp4' ELSE '' END,
       COALESCE(r.sha256, ''), r.work_id
  FROM (SELECT w.id AS work_id, w.author_id, w.created_at,
               regexp_replace(w.video_master_url, '/(clean/)?master\.m3u8$', '') AS dir,
               v.sha256
          FROM works w
          LEFT JOIN video_raw_hashes v
                 ON v.post_id = w.id::text AND v.uploader_pial = w.author_pial::text
         WHERE w.video_master_url ~ '^/.+/master\.m3u8$') r
  JOIN users u ON u.id = r.author_id
 ORDER BY r.dir, (r.sha256 IS NULL), r.created_at, r.work_id
ON CONFLICT DO NOTHING;

-- Single-file video referenced by works (react-video recordings, raw stores).
INSERT INTO canonical_media (creator_user_id, creator_pial_id, master_url, first_post_id)
SELECT DISTINCT ON (w.video_master_url)
       w.author_id, u.pial_id, w.video_master_url, w.id
  FROM works w
  JOIN users u ON u.id = w.author_id
 WHERE w.video_master_url LIKE '/%' AND w.video_master_url !~ '/master\.m3u8$'
 ORDER BY w.video_master_url, w.created_at, w.id
ON CONFLICT DO NOTHING;

-- Voice recordings referenced by works.
INSERT INTO canonical_media (creator_user_id, creator_pial_id, audio_url, first_post_id)
SELECT DISTINCT ON (w.voice_url)
       w.author_id, u.pial_id, w.voice_url, w.id
  FROM works w
  JOIN users u ON u.id = w.author_id
 WHERE w.voice_url LIKE '/%'
 ORDER BY w.voice_url, w.created_at, w.id
ON CONFLICT DO NOTHING;

-- Images referenced by works and not yet on record. One image asset is several
-- derivative files (-thumb/-feed/-full) of one stem; an asset already on record
-- under any of its derivative names is not registered a second time, or the gate
-- would find two uploaders claiming one object.
WITH refs AS (
    SELECT DISTINCT ON (stem) stem, url, author_id, work_id
      FROM (SELECT regexp_replace(m.url, '(-thumb|-feed|-full)?\.[^./]*$', '') AS stem,
                   m.url, w.author_id, w.id AS work_id, w.created_at
              FROM works w
             CROSS JOIN LATERAL unnest(w.media_urls) AS m(url)
             WHERE m.url LIKE '/%') x
     ORDER BY stem, created_at, work_id
), known AS (
    SELECT DISTINCT regexp_replace(image_url, '(-thumb|-feed|-full)?\.[^./]*$', '') AS stem
      FROM canonical_media
     WHERE image_url <> ''
)
INSERT INTO canonical_media (creator_user_id, creator_pial_id, image_url, first_post_id)
SELECT r.author_id, u.pial_id, r.url, r.work_id
  FROM refs r
  JOIN users u ON u.id = r.author_id
 WHERE NOT EXISTS (SELECT 1 FROM known k WHERE k.stem = r.stem)
ON CONFLICT DO NOTHING;
