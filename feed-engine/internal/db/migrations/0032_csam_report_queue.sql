-- 0032_csam_report_queue.sql
--
-- law_enforcement_queue existed with nothing writing to it. These are what a person
-- needs in order to file a CyberTipline report, and to show it was filed. Like the
-- table itself, nothing here has a foreign key, so no account deletion can reach it.
ALTER TABLE law_enforcement_queue
    ADD COLUMN IF NOT EXISTS account_handles  TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS surface          TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS hash_type        TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS content_sha256   TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS account_action   TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS session_evidence JSONB       NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS status           TEXT        NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS safety_notified  BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS resolved_by      TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS resolved_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resolution_note  TEXT        NOT NULL DEFAULT '';

-- The same content from the same person stays one open report however often it is retried.
CREATE UNIQUE INDEX IF NOT EXISTS uq_law_queue_open
    ON law_enforcement_queue (pial_id, content_sha256, surface) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_law_queue_status
    ON law_enforcement_queue (status, reported_at);

-- Admin bans and suspensions wrote capability names in lowercase, while every check
-- reads the uppercase constants, so none of them ever took effect. Apply them under the
-- names that are checked. A suspension whose time has passed is over and is not revived.
INSERT INTO pial_capabilities (pial_id, capability, state, expires_at, reason, granted_by, updated_at)
SELECT c.pial_id,
       UPPER(c.capability),
       CASE WHEN ts.enforcement_state = 'suspended' THEN 'cooldown' ELSE c.state END,
       CASE WHEN ts.enforcement_state = 'suspended' THEN ts.cooldown_until ELSE c.expires_at END,
       c.reason, c.granted_by, NOW()
FROM pial_capabilities c
LEFT JOIN trust_scores ts ON ts.pial_id = c.pial_id
WHERE c.capability IN ('posting', 'messaging')
  AND NOT (ts.enforcement_state = 'suspended' AND ts.cooldown_until IS NOT NULL AND ts.cooldown_until <= NOW())
ON CONFLICT (pial_id, capability) DO UPDATE
  SET state = EXCLUDED.state, expires_at = EXCLUDED.expires_at, reason = EXCLUDED.reason,
      granted_by = EXCLUDED.granted_by, updated_at = NOW();

DELETE FROM pial_capabilities WHERE capability IN ('posting', 'messaging', 'tipping', 'subscribing', 'live_streaming');
