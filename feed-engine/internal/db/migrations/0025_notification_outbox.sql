-- 0025_notification_outbox.sql
--
-- Notifications this brain owes to Herald. A row is written here, then a relay
-- publishes it to Sitra Achra and marks it only after the broker confirms, so a
-- Herald or broker outage delays a notification without losing it.
CREATE TABLE IF NOT EXISTS notification_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    event_id        UUID        NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    recipient_account UUID      NOT NULL,
    recipient_pial  TEXT        NOT NULL,
    event           JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ,
    blocked_at      TIMESTAMPTZ,
    blocked_reason  TEXT
);
CREATE INDEX IF NOT EXISTS idx_notification_outbox_pending
    ON notification_outbox(id) WHERE delivered_at IS NULL AND blocked_at IS NULL;
