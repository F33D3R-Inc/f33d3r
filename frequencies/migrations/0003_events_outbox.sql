-- The Frequency event log, which is also the Sitra Achra outbox.
--
-- Every state mutation inserts its event here in the same transaction as the
-- row it changed. Either the Frequency went live AND frequency.started is
-- queued, or neither happened. src/events/drain.rs publishes rows in `seq`
-- order and marks them; a publish that fails is retried from where it stopped,
-- so a broker outage, a SIGKILL or a rolling deploy loses nothing.
--
-- This is the same argument the Manhattan outbox makes, applied to the event
-- fabric: fire-and-forget produce is a produce that a restart silently loses,
-- and a consumer that never heard frequency.ended keeps rendering a live card.
--
-- Ordering: consumers get events for one Frequency in `seq` order because the
-- drain publishes them in that order with frequency_id as the partition key.
-- Idempotency: event_id is the consumer's dedup key (events/taxonomy.md).

CREATE TABLE IF NOT EXISTS frequency_events (
    event_id            UUID        PRIMARY KEY,
    seq                 BIGSERIAL   NOT NULL UNIQUE,
    frequency_id        UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    event_type          TEXT        NOT NULL CHECK (event_type LIKE 'frequency.%'),
    schema_version      TEXT        NOT NULL DEFAULT '1.0',
    -- The actor, as a bare PIAL uuid: this is the envelope field
    -- events/taxonomy.md calls pial_id and the partition-ordering key every
    -- consumer already understands. 'service' when the platform acted.
    pial_id             TEXT        NOT NULL,
    correlation_id      TEXT        NOT NULL DEFAULT '',
    payload             JSONB       NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    published_at        TIMESTAMPTZ,
    publish_attempts    INTEGER     NOT NULL DEFAULT 0,
    last_error          TEXT
);

-- The drain's query: unpublished, oldest first.
CREATE INDEX IF NOT EXISTS idx_frequency_events_unpublished
    ON frequency_events (seq) WHERE published_at IS NULL;
-- Per-object history, for /v1/frequencies/{id}/events and for reconciliation.
CREATE INDEX IF NOT EXISTS idx_frequency_events_frequency
    ON frequency_events (frequency_id, seq);
