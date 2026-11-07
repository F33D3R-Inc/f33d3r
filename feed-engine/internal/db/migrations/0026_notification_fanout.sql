-- 0026_notification_fanout.sql
--
-- A notification owed to every follower of an account. The job records how far it
-- has got in the same transaction as each batch, so a crash resumes where it stopped.
CREATE TABLE IF NOT EXISTS notification_fanout (
    id           BIGSERIAL   PRIMARY KEY,
    actor_id     UUID        NOT NULL,
    actor_handle TEXT        NOT NULL,
    kind         TEXT        NOT NULL,
    target_id    TEXT        NOT NULL,
    target_type  TEXT        NOT NULL,
    preview      TEXT        NOT NULL DEFAULT '',
    last_follower UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    done_at      TIMESTAMPTZ,
    UNIQUE (kind, target_id)
);
CREATE INDEX IF NOT EXISTS idx_notification_fanout_open
    ON notification_fanout(id) WHERE done_at IS NULL;

