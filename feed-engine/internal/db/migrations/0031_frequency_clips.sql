-- 0031_frequency_clips.sql
--
-- Clips people made from a replay. Making one spends real encoding time, so this is
-- also what limits how many a person can make.
CREATE TABLE IF NOT EXISTS frequency_clips (
    upload_id    TEXT        PRIMARY KEY,
    frequency_id UUID        NOT NULL,
    account_id   UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_frequency_clips_account
    ON frequency_clips (account_id, created_at);
