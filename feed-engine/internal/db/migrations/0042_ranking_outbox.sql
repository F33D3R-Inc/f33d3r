-- 0042_ranking_outbox.sql
--
-- Ranking signals leave feed-engine through this table.
--
-- AethyrRank owns all ranking state and learns only from Sitra Achra
-- ranking.events. Before this, engagement reached it as an HTTP /feedback call
-- in a goroutine that logged and forgot, and views went to a producer that did
-- the same: a restart, a slow ranker or a broker blip lost the signal.
--
-- Now every signal is a row written in the transaction of the action it
-- describes (a like and its row commit together, or neither does), and one
-- relay (internal/aethyr/relay.go, advisory lock 7331_0042) publishes the rows
-- in id order, keyed by the viewer's PIAL, and marks each only after the broker
-- has confirmed it. One person's signals therefore reach AethyrRank in the
-- order they happened, and an outage holds them rather than losing them.
--
-- event_id is AethyrRank's dedup key: a row published twice (the relay died
-- between the broker's confirmation and the mark) is applied once.
--
--   what is waiting:  SELECT count(*), min(created_at) FROM ranking_outbox
--                      WHERE delivered_at IS NULL AND quarantined_at IS NULL;
--
--   what can never land (too large for the broker), kept for a person:
--                     SELECT id, event_id, quarantine_reason FROM ranking_outbox
--                      WHERE quarantined_at IS NOT NULL;
CREATE TABLE IF NOT EXISTS ranking_outbox (
    id                BIGSERIAL   PRIMARY KEY,
    event_id          UUID        NOT NULL UNIQUE,
    viewer_pial       TEXT        NOT NULL,
    event             JSONB       NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at      TIMESTAMPTZ,
    quarantined_at    TIMESTAMPTZ,
    quarantine_reason TEXT
);

-- The relay's only read: open rows, oldest first.
CREATE INDEX IF NOT EXISTS idx_ranking_outbox_open
    ON ranking_outbox (id)
 WHERE delivered_at IS NULL AND quarantined_at IS NULL;

-- Retention of delivered rows.
CREATE INDEX IF NOT EXISTS idx_ranking_outbox_delivered
    ON ranking_outbox (delivered_at)
 WHERE delivered_at IS NOT NULL;
