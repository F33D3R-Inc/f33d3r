-- ── Abraxas becomes the integrity authority ──────────────────────────────────
-- feed-engine no longer judges content itself. Abraxas publishes every verdict,
-- ownership finding, banned match and review decision to Sitra Achra topic
-- safety.events, and feed-engine applies them (internal/sitra/safety.go,
-- internal/handler/safety_events.go). This migration gives that consumer what
-- the identity consumer has: an ordering ledger and a dead-letter table.

-- ── safety_subject_seq ───────────────────────────────────────────────────────
-- The last Abraxas sequence applied per subject. An event whose seq is not
-- above the recorded one has already been applied (a redelivery) and is
-- skipped, so a later decision is never overwritten by an earlier event.
CREATE TABLE IF NOT EXISTS safety_subject_seq (
    subject_kind  TEXT        NOT NULL,
    subject_id    TEXT        NOT NULL,
    last_seq      BIGINT      NOT NULL,
    last_event_id TEXT        NOT NULL,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subject_kind, subject_id)
);

-- ── safety_event_deadletter ──────────────────────────────────────────────────
-- A safety.events record that can never be applied is kept here with its
-- coordinates and the reason, and only then committed past.
CREATE TABLE IF NOT EXISTS safety_event_deadletter (
    id          BIGSERIAL   PRIMARY KEY,
    topic       TEXT        NOT NULL,
    partition   INT         NOT NULL,
    "offset"    BIGINT      NOT NULL,
    record      BYTEA       NOT NULL,
    reason      TEXT        NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (topic, partition, "offset")
);

-- ── law_enforcement_queue.safety_notified ────────────────────────────────────
-- Recorded whether Zodacare confirmed a CSAM account action. Zodacare is
-- retired: the ban or hold is queued in identity_commands on the report's own
-- commit and delivered to elohim-veni by the identity relay, whose delivery
-- state is the record. The column would only ever read false.
ALTER TABLE law_enforcement_queue DROP COLUMN IF EXISTS safety_notified;

-- ── Pending works wait for their verdict ─────────────────────────────────────
-- A work whose scan is pending is visible to its author only, and is not
-- fanned out until its verdict arrives (ClaimWorkOutbox). This index serves
-- that wait.
CREATE INDEX IF NOT EXISTS idx_works_pending_scan
    ON works (id) WHERE scan_state = 'pending' AND deleted_at IS NULL;

-- ── Every pending work is owed a verdict ─────────────────────────────────────
-- From this migration on a pending work is hidden from everyone but its author
-- until Abraxas answers. A work that is pending now may have been announced to
-- a scanner that never answered (content-scan's callback is gone), so its scan
-- obligation is owed again: queued where it is missing, reopened where it was
-- closed or quarantined. The event_id is stable per work, so Abraxas recognises
-- a work it has already seen.
INSERT INTO work_outbox (work_id, action, dedup_key)
SELECT w.id, 'content_event', 'content_event:' || w.id::text
  FROM works w
 WHERE w.scan_state = 'pending' AND w.deleted_at IS NULL
ON CONFLICT (dedup_key) DO UPDATE
   SET done_at = NULL, quarantined_at = NULL, quarantine_reason = NULL,
       attempts = 0, last_error = NULL, next_attempt_at = NOW()
 WHERE work_outbox.done_at IS NOT NULL OR work_outbox.quarantined_at IS NOT NULL;
