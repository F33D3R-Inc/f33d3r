-- migrate-prod-video-urls.sql
--
-- One-time migration: repoint existing prod video_master_url paths from the
-- old single-pass structure to the new two-pass clean/watermarked structure.
--
-- Old path: /static/media/posts/{id}/master.m3u8
-- New path: /static/media/posts/{id}/clean/master.m3u8
--
-- Run ONCE on prod DB before or immediately after deploying the two-pass
-- transcoding build. Idempotent: the WHERE clause skips already-migrated rows.
--
-- Usage (on prod server):
--   docker compose -f docker-compose.prod.yml --env-file .env.prod exec -T postgres \
--     psql -U f33d3r -d f33d3r_feed -f /path/to/migrate-prod-video-urls.sql
-- Or paste the UPDATE directly into psql.

BEGIN;

-- Show what will be updated before committing
SELECT
  id,
  video_master_url AS old_url,
  regexp_replace(video_master_url, '/master\.m3u8$', '/clean/master.m3u8') AS new_url
FROM works
WHERE video_master_url IS NOT NULL
  AND video_master_url NOT LIKE '%/clean/%'
  AND video_master_url NOT LIKE '%/watermarked/%';

-- Rewrite master_url to clean/ subdirectory
UPDATE works
SET video_master_url = regexp_replace(video_master_url, '/master\.m3u8$', '/clean/master.m3u8')
WHERE video_master_url IS NOT NULL
  AND video_master_url NOT LIKE '%/clean/%'
  AND video_master_url NOT LIKE '%/watermarked/%';

-- Report row count
SELECT CONCAT('Rows updated: ', COUNT(*)) AS result
FROM works
WHERE video_master_url LIKE '%/clean/master.m3u8';

COMMIT;
