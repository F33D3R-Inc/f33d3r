-- 0002_additive_baseline.sql
-- The additive statements that used to run as a Go string slice after the DDL blob.
-- Folded into the ordered plane verbatim so there is exactly one place schema changes live.

ALTER TABLE works ADD COLUMN IF NOT EXISTS translated_body TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS translated_lang TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS is_edited BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE works ADD COLUMN IF NOT EXISTS edited_at TIMESTAMPTZ;
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS pinned_work_id UUID REFERENCES works(id) ON DELETE SET NULL;

-- Facet Atlas — admin review status per facet (approved | needs_work | unreviewed).
CREATE TABLE IF NOT EXISTS facet_reviews (
    name        TEXT PRIMARY KEY,
    status      TEXT NOT NULL,
    note        TEXT,
    reviewed_by TEXT,
    reviewed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Moderation dedup — collapse any pre-existing duplicate pending reports (same reporter on
-- the same content) down to the earliest row, then add a partial-unique index so a reporter
-- can hold only one open report per item. This kills the "triple" cards at the source.
UPDATE content_reports cr
   SET status = 'resolved', resolved_by = 'system:dedup', resolved_at = NOW()
 WHERE status = 'pending'
   AND id <> (
       SELECT c2.id FROM content_reports c2
        WHERE c2.reporter_id = cr.reporter_id
          AND c2.content_id  = cr.content_id
          AND c2.status      = 'pending'
        ORDER BY c2.created_at ASC, c2.id ASC
        LIMIT 1);

CREATE UNIQUE INDEX IF NOT EXISTS uq_reports_pending_per_reporter
    ON content_reports(reporter_id, content_id) WHERE status = 'pending';
