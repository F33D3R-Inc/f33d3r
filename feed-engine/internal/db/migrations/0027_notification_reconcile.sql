-- 0027_notification_reconcile.sql
--
-- Tracks the check that every notification delivered to Sitra Achra was stored by
-- Herald. A row Herald never stored is published again, a limited number of times.
ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS reconciled_at TIMESTAMPTZ;
ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS reconcile_attempts INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_notification_outbox_reconcile
    ON notification_outbox(delivered_at) WHERE delivered_at IS NOT NULL AND reconciled_at IS NULL;
