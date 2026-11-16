-- 0041_retire_alexandria_catalog.sql
--
-- Alexandria is retired. It was a second catalog of works: every title it held
-- was a copy of a feed-engine work, written by the alexandria_catalog
-- obligation, and nothing on the platform ever read one back. Works are
-- feed-engine's (Manhattan's authority map gives the `work` kind to this
-- brain), so the copy is gone rather than moved, and so is the obligation that
-- kept writing it.
--
-- Every alexandria_catalog row goes. A pending or quarantined one is a debt to a
-- service that no longer exists and can never be paid; a finished one is the
-- receipt for a copy that no longer exists. Deleting them is what lets the
-- vocabulary below be enforced on every row, old ones included, instead of
-- being declared NOT VALID and trusted.
--
-- cmd/retire-alexandria verifies f33d3r_library against f33d3r_feed and
-- retires the naming-plane entries of any title that was never a feed-engine
-- work. It is the only thing that reads f33d3r_library; this migration reads
-- nothing outside f33d3r_feed.

DELETE FROM work_outbox WHERE action = 'alexandria_catalog';

-- Mirrors db.WorkObligation (work_outbox.go). Change one and the other must change with it.
ALTER TABLE work_outbox DROP CONSTRAINT IF EXISTS work_outbox_action_vocabulary;
ALTER TABLE work_outbox ADD CONSTRAINT work_outbox_action_vocabulary CHECK (action IN (
    'content_event',      -- Sitra Achra content.events: the moderation scan
    'follower_fanout',    -- FA Live new_post to followers
    'cited_update',       -- FA Live counts and reply row of the cited work
    'link_preview',       -- link_previews cache
    'zior_voice',         -- Zior audio vector of a voice work or a track
    'interest_learning',  -- author's interest vector toward what they cite
    'achievements',       -- post-creation achievements
    'media_bind'          -- re-bind a legacy work's media paths to Caeor assets
));
