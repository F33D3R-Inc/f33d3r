-- 0028_notification_work_jobs.sql
--
-- A new work owes notifications to the people it replies to, quotes and mentions.
-- Working out who needs the naming plane, which cannot be part of the transaction
-- that creates the work. So that transaction records the debt here, and a worker
-- pays it. A scheduled work's job is not due until the work is published.
CREATE TABLE IF NOT EXISTS notification_work_jobs (
    work_id         UUID        PRIMARY KEY REFERENCES works(id) ON DELETE CASCADE,
    attempts        INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_notification_work_jobs_due
    ON notification_work_jobs(next_attempt_at);
