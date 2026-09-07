-- Frequency Replay metadata. Recording is Phase 5; the table exists now so the
-- schema every other row references is settled before the media plane is.
--
-- Recording happens server-side from the SFU's media tap. No host browser
-- produces the canonical recording. The object itself lives in Caeor / MinIO
-- under object_key; this row is what a Frequency knows about its replay.

CREATE TABLE IF NOT EXISTS frequency_recordings (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    frequency_id        UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    object_key          TEXT,
    codec               TEXT        NOT NULL DEFAULT 'opus',
    sample_rate         INTEGER     NOT NULL DEFAULT 48000,
    channels            SMALLINT    NOT NULL DEFAULT 1,
    duration_ms         BIGINT,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at            TIMESTAMPTZ,
    processing_state    TEXT        NOT NULL DEFAULT 'recording'
                                    CHECK (processing_state IN ('recording', 'uploading', 'processing', 'ready', 'failed')),
    failure_reason      TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_frequency_recordings_frequency
    ON frequency_recordings (frequency_id, started_at DESC);
