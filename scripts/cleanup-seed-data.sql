-- F33D3R seed data cleanup
-- Removes fake posts, fake follows, fake metrics from seed accounts
-- Safe to run on prod: only touches data from admin/guest/creator/artist accounts
-- and resets inflated post_metrics to real counts

BEGIN;

-- 1. Delete posts from pure seed accounts + 2 seeded dev posts
--    CASCADE removes their post_likes, post_reposts, post_metrics, bookmarks
DELETE FROM posts
WHERE author_id IN (SELECT id FROM users WHERE handle IN ('admin','guest','creator','artist'))
   OR (author_id = (SELECT id FROM users WHERE handle='dev')
       AND created_at = '2026-05-07 04:00:09.775823+00');

-- 2. Delete fake follow graph involving seed accounts
DELETE FROM follows
WHERE follower_id IN (SELECT id FROM users WHERE handle IN ('admin','guest','creator','artist'))
   OR following_id IN (SELECT id FROM users WHERE handle IN ('admin','guest','creator','artist'));

-- 3. Delete notifications to seed accounts
DELETE FROM notifications
WHERE user_id IN (SELECT id FROM users WHERE handle IN ('admin','guest','creator','artist'));

-- 4. Reset ALL post_metrics to actual table counts (wipes seeded inflation)
UPDATE post_metrics pm SET
  likes       = (SELECT COUNT(*) FROM post_likes   WHERE post_id = pm.post_id),
  reposts     = (SELECT COUNT(*) FROM post_reposts WHERE post_id = pm.post_id),
  comments    = (SELECT COUNT(*) FROM posts        WHERE parent_id = pm.post_id),
  impressions = 0,
  saves       = (SELECT COUNT(*) FROM bookmarks    WHERE post_id = pm.post_id);

-- 5. Clean up orphaned metric rows
DELETE FROM post_metrics WHERE post_id NOT IN (SELECT id FROM posts);

COMMIT;

-- Verify
SELECT 'posts' AS table_name, COUNT(*) AS remaining FROM posts
UNION ALL SELECT 'follows',      COUNT(*) FROM follows
UNION ALL SELECT 'post_likes',   COUNT(*) FROM post_likes
UNION ALL SELECT 'post_reposts', COUNT(*) FROM post_reposts
UNION ALL SELECT 'notifications',COUNT(*) FROM notifications;
