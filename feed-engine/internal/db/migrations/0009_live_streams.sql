-- 0010_live_streams.sql
--
-- The live lane. Stream lifecycle, ingest credentials, viewer and scan samples.
-- Video never touches this database: only the state the server is authoritative for.

-- ── live_streams ─────────────────────────────────────────────────────────────
-- One row per broadcast. Status is server-owned: idle -> live -> ended, and no
-- other transition is legal. scan_state starts at 'pending_scan' — a live stream
-- has produced no frames yet, so nothing about it can honestly be called clean.
CREATE TABLE IF NOT EXISTS live_streams (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    author_pial   UUID        NOT NULL,
    title         TEXT        NOT NULL DEFAULT '',
    description   TEXT        NOT NULL DEFAULT '',
    status        TEXT        NOT NULL DEFAULT 'idle'
                              CHECK (status IN ('idle', 'live', 'ended')),
    started_at    TIMESTAMPTZ,
    ended_at      TIMESTAMPTZ,
    viewer_count  INT         NOT NULL DEFAULT 0 CHECK (viewer_count >= 0),
    peak_viewers  INT         NOT NULL DEFAULT 0 CHECK (peak_viewers >= 0),
    poster_url    TEXT        NOT NULL DEFAULT '',
    playlist_url  TEXT        NOT NULL DEFAULT '',
    is_nsfw       BOOLEAN     NOT NULL DEFAULT FALSE,
    is_blocked    BOOLEAN     NOT NULL DEFAULT FALSE,
    scan_state    TEXT        NOT NULL DEFAULT 'pending_scan'
                              CHECK (scan_state IN ('pending_scan', 'clean', 'age_gated',
                                                    'human_review', 'flagged', 'blocked')),
    -- Source geometry as the media server actually observed it. The ABR ladder is
    -- derived from these, never from what the broadcaster claimed.
    source_width  INT         NOT NULL DEFAULT 0,
    source_height INT         NOT NULL DEFAULT 0,
    ingest_proto  TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- An author holds at most one open stream row; ended rows are unconstrained history.
CREATE UNIQUE INDEX IF NOT EXISTS idx_live_streams_one_open_per_author
    ON live_streams(author_id) WHERE status IN ('idle', 'live');

CREATE INDEX IF NOT EXISTS idx_live_streams_active
    ON live_streams(started_at DESC) WHERE status = 'live';

CREATE INDEX IF NOT EXISTS idx_live_streams_author
    ON live_streams(author_id, created_at DESC);

-- ── live_ingest_keys ─────────────────────────────────────────────────────────
-- The publish secret, stored as SHA-256 only. The plaintext key is returned once
-- at mint or rotation and is never recoverable from this table.
CREATE TABLE IF NOT EXISTS live_ingest_keys (
    stream_id  UUID        PRIMARY KEY REFERENCES live_streams(id) ON DELETE CASCADE,
    key_hash   TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at TIMESTAMPTZ,
    last_used  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_live_ingest_keys_hash
    ON live_ingest_keys(key_hash);

-- ── live_viewer_samples ──────────────────────────────────────────────────────
-- Time series behind peak_viewers. Written by the server, never by a client.
CREATE TABLE IF NOT EXISTS live_viewer_samples (
    id           BIGSERIAL   PRIMARY KEY,
    stream_id    UUID        NOT NULL REFERENCES live_streams(id) ON DELETE CASCADE,
    viewer_count INT         NOT NULL CHECK (viewer_count >= 0),
    sampled_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_live_viewer_samples_stream
    ON live_viewer_samples(stream_id, sampled_at DESC);

-- ── live_scan_samples ────────────────────────────────────────────────────────
-- One row per keyframe pulled out of the running ladder and put through the
-- content-safety path. This is the evidence trail behind live_streams.scan_state.
CREATE TABLE IF NOT EXISTS live_scan_samples (
    id         BIGSERIAL   PRIMARY KEY,
    stream_id  UUID        NOT NULL REFERENCES live_streams(id) ON DELETE CASCADE,
    frame_name TEXT        NOT NULL,
    sha256     TEXT        NOT NULL DEFAULT '',
    verdict    TEXT        NOT NULL DEFAULT 'pending_scan',
    risk_level TEXT        NOT NULL DEFAULT '',
    signals    TEXT[]      NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (stream_id, frame_name)
);

CREATE INDEX IF NOT EXISTS idx_live_scan_samples_stream
    ON live_scan_samples(stream_id, created_at DESC);
