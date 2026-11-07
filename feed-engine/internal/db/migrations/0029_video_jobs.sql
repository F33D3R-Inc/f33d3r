-- 0029_video_jobs.sql
--
-- A video being processed. The row outlives a restart of either side, so its owner
-- always hears how it ended.
CREATE TABLE IF NOT EXISTS video_jobs (
    upload_id  TEXT        PRIMARY KEY,
    account_id UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pial_id    TEXT        NOT NULL,
    filename   TEXT        NOT NULL DEFAULT '',
    job_id     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
