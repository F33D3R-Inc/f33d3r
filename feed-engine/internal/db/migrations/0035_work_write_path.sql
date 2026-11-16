-- 0035_work_write_path.sql
--
-- The work write path, rebuilt so that the transaction that creates a work is the
-- acknowledgement of everything the work is owed.
--
-- Before this, the create transaction wrote the works row, its citations and its
-- notification job, and everything else a new work causes — the content scan, the
-- follower fan-out, the cited work's counts, the Alexandria catalog entry, the link
-- preview, the voice analysis, interest learning, achievements — was started as a
-- goroutine after the commit or handed to a producer that logs and forgets. A
-- restart between the commit and those goroutines lost them, and nothing retried.
-- Edition 1 and the content receipt were written outside the transaction and their
-- failures were logged as "non-fatal".
--
-- Six things change here.
--
--   1. work_outbox: every downstream obligation of a work is a row, written in the
--      work's own transaction, drained by a worker with backoff and quarantine.
--   2. works.is_reply: being a reply is a fact the server derives from the signed
--      parent citation, not the client's kind label. Every feed, fan-out and notice
--      gate reads this one column.
--   3. works.content_fingerprint: the same author posting the same thing twice
--      within the dedup window is a Duplicate outcome, decided in the transaction.
--   4. comment_gating: one vocabulary, enforced by CHECK.
--   5. The signed is_repost lane is retired. The reaction repost is the one repost.
--   6. user_profiles.post_count is maintained by trigger, for every path.

-- ── 1. work_outbox ───────────────────────────────────────────────────────────
-- One row per (action, work). dedup_key makes enqueue idempotent, so a replayed
-- create or a re-run release cannot queue the same obligation twice.
--
-- A row is claimed by moving next_attempt_at past a lease and counting the
-- attempt, under FOR UPDATE SKIP LOCKED, so any number of replicas drain the same
-- table without two of them running one row. A crash mid-action leaves the row to
-- be claimed again when its lease runs out. A row that keeps failing backs off
-- exponentially and, at the attempt cap, is quarantined: kept with its error, no
-- longer claimed, and reported until a person puts it back.
--
--   what is stuck:  SELECT id, work_id, action, attempts, last_error, quarantined_at
--                     FROM work_outbox
--                    WHERE quarantined_at IS NOT NULL AND done_at IS NULL
--                    ORDER BY id;
--
--   put one back:   UPDATE work_outbox
--                      SET quarantined_at = NULL, quarantine_reason = NULL,
--                          attempts = 0, next_attempt_at = NOW()
--                    WHERE id = $1;
--
-- ON DELETE CASCADE: a work that is hard-deleted owes nothing. A soft-deleted work
-- keeps its rows; the worker re-reads deleted_at before every action and closes
-- the row without acting.
CREATE TABLE IF NOT EXISTS work_outbox (
    id                BIGSERIAL   PRIMARY KEY,
    work_id           UUID        NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    action            TEXT        NOT NULL,
    dedup_key         TEXT        NOT NULL UNIQUE,
    attempts          INT         NOT NULL DEFAULT 0,
    last_error        TEXT,
    next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    done_at           TIMESTAMPTZ,
    quarantined_at    TIMESTAMPTZ,
    quarantine_reason TEXT,
    -- Mirrors db.WorkObligation (work_outbox.go). Change one and the other must change with it.
    CONSTRAINT work_outbox_action_vocabulary CHECK (action IN (
        'content_event',      -- Sitra Achra content.events: the moderation scan
        'follower_fanout',    -- FA Live new_post to followers
        'cited_update',       -- FA Live counts and reply row of the cited work
        'alexandria_catalog', -- Alexandria title
        'link_preview',       -- link_previews cache
        'zior_voice',         -- Zior audio vector of a voice work
        'interest_learning',  -- author's interest vector toward what they cite
        'achievements'        -- post-creation achievements
    ))
);

-- The drain's only query: open, not quarantined, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_work_outbox_due
    ON work_outbox (next_attempt_at, id)
 WHERE done_at IS NULL AND quarantined_at IS NULL;

-- What is quarantined, read on every report.
CREATE INDEX IF NOT EXISTS idx_work_outbox_quarantined
    ON work_outbox (id)
 WHERE quarantined_at IS NOT NULL AND done_at IS NULL;

-- Retention of finished rows.
CREATE INDEX IF NOT EXISTS idx_work_outbox_done
    ON work_outbox (done_at)
 WHERE done_at IS NOT NULL;

-- ── 2. works.is_reply ────────────────────────────────────────────────────────
-- Written once, in the create transaction, from the parent citation the author
-- signed. It is a column rather than a NOT EXISTS over work_citations because a
-- citation is removed with its target (fk_work_citations_target ON DELETE
-- CASCADE): a reply whose parent is hard-deleted is still a reply, and must not
-- surface in feeds as a post.
--
-- Backfill: every work with a reply citation, and every legacy kind='reply' row
-- whose parent never resolved (the old path stored those as orphans).
ALTER TABLE works ADD COLUMN IF NOT EXISTS is_reply BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE works w
   SET is_reply = TRUE
 WHERE w.kind = 'reply'
    OR EXISTS (SELECT 1 FROM work_citations c
                WHERE c.work_id = w.id AND c.citation_type = 'reply');

-- Rate limit, dedup window and the profile timeline all read one author's works
-- newest first.
CREATE INDEX IF NOT EXISTS idx_works_author_created
    ON works (author_id, created_at DESC);

-- ── 3. works.content_fingerprint ─────────────────────────────────────────────
-- sha256 of the normalized content (worktext.Fingerprint): body, kind, parent,
-- quote, media, poll. NULL on works created before this migration, which therefore
-- never count as a duplicate of anything.
ALTER TABLE works ADD COLUMN IF NOT EXISTS content_fingerprint TEXT;

CREATE INDEX IF NOT EXISTS idx_works_author_fingerprint
    ON works (author_id, content_fingerprint, created_at DESC)
 WHERE content_fingerprint IS NOT NULL;

-- ── 4. comment_gating: one vocabulary ────────────────────────────────────────
-- The spelling is the clients'. comment_gating is inside the signed payload
-- (workCanonicalPayload), so the value a row is created with is the value its
-- author signed; storing 'open' for a signed 'everyone' made every row disagree
-- with its own signature. The value set is the union of what clients sign
-- (everyone|followers|circle|none) and what the setter could store (verified):
-- every one of them is a choice an author made, and dropping one would rewrite it.
--
-- 'open' and the empty string were the setter's and the column default's spelling
-- of 'everyone'. Anything else outside the vocabulary was never enforced, so the
-- behaviour every such work actually had was 'everyone', and that is what it keeps.
UPDATE works
   SET comment_gating = 'everyone'
 WHERE comment_gating IS NULL
    OR comment_gating NOT IN ('everyone', 'followers', 'circle', 'verified', 'none');

ALTER TABLE works ALTER COLUMN comment_gating SET DEFAULT 'everyone';

-- Mirrors db.CommentGatings (works.go). Change one and the other must change with it.
ALTER TABLE works DROP CONSTRAINT IF EXISTS works_comment_gating_vocabulary;
ALTER TABLE works ADD CONSTRAINT works_comment_gating_vocabulary
    CHECK (comment_gating IN ('everyone', 'followers', 'circle', 'verified', 'none'));

-- ── 5. retire the signed is_repost lane ──────────────────────────────────────
-- A repost is a reaction (work_reactions.reaction_type = 'repost'): it is what the
-- following feed unions, what the counts count, and what can be undone. A signed
-- is_repost work was a second model of the same act that no client creates and no
-- Facet renders — it drew as an empty post. Existing rows are carried into the one
-- model before the columns go:
--
--   * a shell (no words, no media, no poll) of a work that still exists becomes the
--     author's repost reaction on that work; every shell is then removed, because
--     a shell says nothing of its own and drew only as an empty card;
--   * a repost that carries the author's own words is what the product calls a
--     quote: it gains the quote citation and keeps everything it said;
--   * a repost with words whose source is gone is a post — it keeps what it said.
INSERT INTO work_reactions (work_id, reactor_id, reaction_type, created_at)
SELECT w.repost_source_id, w.author_id, 'repost', w.created_at
  FROM works w
  JOIN works src ON src.id = w.repost_source_id
 WHERE w.is_repost
   AND w.deleted_at IS NULL
   AND w.repost_source_id <> w.id
   AND btrim(w.body) = ''
   AND cardinality(w.media_urls) = 0
   AND w.voice_url IS NULL
   AND w.video_master_url IS NULL
   AND w.poll_options IS NULL
ON CONFLICT (work_id, reactor_id, reaction_type) DO NOTHING;

DELETE FROM works w
 WHERE w.is_repost
   AND btrim(w.body) = ''
   AND cardinality(w.media_urls) = 0
   AND w.voice_url IS NULL
   AND w.video_master_url IS NULL
   AND w.poll_options IS NULL;

INSERT INTO work_citations (work_id, target_id, citation_type)
SELECT w.id, w.repost_source_id, 'quote'
  FROM works w
  JOIN works src ON src.id = w.repost_source_id
 WHERE w.is_repost
   AND w.repost_source_id <> w.id
ON CONFLICT (work_id, target_id, citation_type) DO NOTHING;

UPDATE works w
   SET kind = 'quote'
  FROM works src
 WHERE src.id = w.repost_source_id
   AND w.is_repost
   AND w.repost_source_id <> w.id
   AND w.kind = 'post';

ALTER TABLE works DROP COLUMN IF EXISTS is_repost;
ALTER TABLE works DROP COLUMN IF EXISTS repost_source_id;

-- ── 6. user_profiles.post_count by trigger ───────────────────────────────────
-- post_count was raised only by the scheduled-release cron, so every immediate
-- work was missing from it and no delete ever lowered it. It counts a person's
-- published works: not deleted, not waiting on a schedule. A trigger cannot be
-- bypassed — it fires for the create path, the release, a soft delete, an admin
-- hard delete and an account erasure cascade alike — so nothing in application
-- code maintains this number any more.
CREATE OR REPLACE FUNCTION works_post_count() RETURNS TRIGGER AS $$
DECLARE
    was_counted BOOLEAN := FALSE;
    is_counted  BOOLEAN := FALSE;
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        was_counted := OLD.deleted_at IS NULL AND OLD.scheduled_at IS NULL;
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        is_counted := NEW.deleted_at IS NULL AND NEW.scheduled_at IS NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND was_counted = is_counted AND OLD.author_id = NEW.author_id THEN
        RETURN NULL;
    END IF;
    IF was_counted THEN
        UPDATE user_profiles SET post_count = GREATEST(0, COALESCE(post_count, 0) - 1)
         WHERE user_id = OLD.author_id;
    END IF;
    IF is_counted THEN
        UPDATE user_profiles SET post_count = COALESCE(post_count, 0) + 1
         WHERE user_id = NEW.author_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_works_post_count ON works;
CREATE TRIGGER trg_works_post_count
    AFTER INSERT OR DELETE OR UPDATE OF deleted_at, scheduled_at, author_id ON works
    FOR EACH ROW EXECUTE FUNCTION works_post_count();

-- One-time reconciliation of the drift the trigger now makes impossible.
UPDATE user_profiles p
   SET post_count = (SELECT COUNT(*) FROM works w
                      WHERE w.author_id = p.user_id
                        AND w.deleted_at IS NULL
                        AND w.scheduled_at IS NULL);
