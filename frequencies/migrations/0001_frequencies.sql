-- The Frequency: F33D3R's live-audio social object.
--
-- A Frequency is durable. The WebRTC session that carries its audio is
-- temporary and lives in Redis and in the owning media node's memory; this row
-- outlives it. Everything a person can point at — a card in the feed, a
-- scheduled reminder, a replay — points at this id.
--
-- Identity columns hold NAMES in the naming plane's vocabulary
-- (`pial:<uuid>`), never handles and never another brain's row ids. Manhattan
-- migration 0004 makes `pial:` a derived namespace, so whichever brain meets a
-- person first may register them; the trigger in 0004_manhattan_outbox.sql
-- does that for every identity written here.

CREATE TABLE IF NOT EXISTS frequencies (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Optimistic concurrency. Every mutation is
    --   UPDATE ... WHERE id = $1 AND version = $2
    -- and bumps this; zero rows affected means somebody else moved first and
    -- the caller re-reads before deciding again. Two co-host promotions, two
    -- approvals, an end racing a join — none of them can interleave.
    version                 BIGINT      NOT NULL DEFAULT 1,

    host_pial               TEXT        NOT NULL CHECK (host_pial LIKE 'pial:%'),

    title                   TEXT        NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
    description             TEXT        NOT NULL DEFAULT '' CHECK (char_length(description) <= 600),

    state                   TEXT        NOT NULL CHECK (state IN (
                                'draft', 'scheduled', 'starting', 'live', 'ending', 'ended',
                                'processing_replay', 'archived',
                                'cancelled', 'failed', 'moderation_terminated')),

    visibility              TEXT        NOT NULL DEFAULT 'public'
                                        CHECK (visibility IN ('public', 'followers', 'subscribers', 'private')),
    language                TEXT        NOT NULL DEFAULT 'en' CHECK (char_length(language) BETWEEN 2 AND 16),
    adult_content           BOOLEAN     NOT NULL DEFAULT FALSE,
    -- Minimum Verity attestation tier a listener needs before Request Mic is
    -- accepted. 0 means anyone who can listen may ask.
    speaker_verity_min_tier SMALLINT    NOT NULL DEFAULT 0 CHECK (speaker_verity_min_tier BETWEEN 0 AND 3),

    scheduled_at            TIMESTAMPTZ,
    started_at              TIMESTAMPTZ,
    ended_at                TIMESTAMPTZ,
    -- Why it ended: host_ended, host_lost, moderation, failed, cancelled.
    end_reason              TEXT,

    recording_enabled       BOOLEAN     NOT NULL DEFAULT FALSE,
    replay_status           TEXT        NOT NULL DEFAULT 'none'
                                        CHECK (replay_status IN ('none', 'processing', 'ready', 'failed')),

    max_speakers            INTEGER     NOT NULL DEFAULT 10   CHECK (max_speakers BETWEEN 1 AND 50),
    max_listeners           INTEGER     NOT NULL DEFAULT 1000 CHECK (max_listeners BETWEEN 1 AND 100000),
    requests_open           BOOLEAN     NOT NULL DEFAULT TRUE,
    locked                  BOOLEAN     NOT NULL DEFAULT FALSE,

    -- The media node that owns this Frequency's live session. Authoritative
    -- ownership is the Redis lease (freq:<id>:lease); this is the last known
    -- holder, kept so a restart can tell which sessions it was serving.
    media_node              TEXT,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One open Frequency per host. Scheduled ones may stack up; live ones may not.
CREATE UNIQUE INDEX IF NOT EXISTS uq_frequencies_one_open_per_host
    ON frequencies (host_pial)
 WHERE state IN ('starting', 'live', 'ending');

-- Discovery reads: what is live, what is coming, what just ended.
CREATE INDEX IF NOT EXISTS idx_frequencies_live
    ON frequencies (started_at DESC) WHERE state = 'live';
CREATE INDEX IF NOT EXISTS idx_frequencies_scheduled
    ON frequencies (scheduled_at ASC) WHERE state = 'scheduled';
CREATE INDEX IF NOT EXISTS idx_frequencies_ended
    ON frequencies (ended_at DESC) WHERE state IN ('ended', 'processing_replay', 'archived');
CREATE INDEX IF NOT EXISTS idx_frequencies_host
    ON frequencies (host_pial, created_at DESC);
