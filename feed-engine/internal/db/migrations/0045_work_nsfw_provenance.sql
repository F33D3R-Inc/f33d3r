-- 0045_work_nsfw_provenance.sql
--
-- Only 18+ material is hidden. A work is 18+ when its creator marked it so, or
-- when Abraxas age-gated it for nudity. Two facts are needed to keep those
-- apart, and the table held one:
--
--   creator_marked_nsfw — the creator's own 18+ mark, written once at insert.
--     Before this, an age-gate overwrote is_nsfw with TRUE, so the creator's
--     choice was lost and a work could never be returned to it. is_nsfw is now
--     derived on every scan-state change: creator_marked_nsfw OR age_gated
--     (SetPostScanState).
--
--   scan_set_by — whether automation or a person put the work in its current
--     scan state. Automation may revise what automation decided (a re-scan
--     under a changed policy); a person's decision is never overwritten by it
--     (applyWorkVerdict). Before this, automation could only ever move a
--     pending work, so a policy fix could not reach anything already judged.

ALTER TABLE works ADD COLUMN IF NOT EXISTS creator_marked_nsfw BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE works ADD COLUMN IF NOT EXISTS scan_set_by TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE works DROP CONSTRAINT IF EXISTS works_scan_set_by_vocabulary;
ALTER TABLE works ADD CONSTRAINT works_scan_set_by_vocabulary CHECK (scan_set_by IN ('auto', 'person'));

-- Who set each work's current state: the latest moderation-log row says. A
-- person is any actor other than abraxas, or an Abraxas review decision.
UPDATE works w
   SET scan_set_by = 'person'
  FROM (SELECT DISTINCT ON (post_id) post_id, actor, reason
          FROM content_moderation_log
         ORDER BY post_id, created_at DESC) last
 WHERE last.post_id = w.id::text
   AND (last.actor <> 'abraxas' OR last.reason LIKE 'abraxas review decision%');

-- The creator's mark, for works that existed before it was recorded. A work's
-- is_nsfw is the creator's mark unless automation age-gated it, in which case
-- the mark was overwritten and cannot be read back:
--   * a work with media keeps is_nsfw as its mark — it may well be 18+ media,
--     and hiding a work its creator marked is the side to err on;
--   * a work with no media cannot be nude media, so an automated age-gate on
--     it came from the retired rule that gated every post of an adult creator,
--     and its mark is FALSE. A re-scan (abraxas-shield rescan) then judges it
--     under the current policy.
UPDATE works
   SET creator_marked_nsfw = is_nsfw
 WHERE NOT (scan_state = 'age_gated'
            AND scan_set_by = 'auto'
            AND cardinality(media_urls) = 0
            AND video_master_url IS NULL
            AND voice_url IS NULL);
