-- 0030_frequency_notifications.sql
--
-- Who a follower notification may reach is decided per job, and Frequency
-- announcements are limited per person so a busy timeline cannot flood anyone.
ALTER TABLE notification_fanout
    ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS adult BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS frequency_announced (
    account_id UUID        NOT NULL,
    at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_frequency_announced_account
    ON frequency_announced (account_id, at);
