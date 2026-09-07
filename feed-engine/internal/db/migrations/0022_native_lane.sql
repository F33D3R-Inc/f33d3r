-- 0022_native_lane.sql
--
-- What the native clients (mobile/f33d3r_iOS, the Android JSON lane) do with
-- Fleets and Live that the web surfaces had no columns for.
--
-- Fleets: a poll on a text Fleet, an audience (everyone or followers), whether
-- the author takes replies, and a per-viewer mute of one author's ring.
--
-- Live: the room around a broadcast — hearts, a pinned line, a tip goal and
-- the running tip total, the lane the broadcast was filed under, whether the
-- replay is kept, and the chat line count the end-of-stream summary reports.
-- Chat lines themselves stay in memory by design (handler/live.go liveState):
-- they end with the broadcast. Tips are rows, because money is.

ALTER TABLE fleets ADD COLUMN IF NOT EXISTS poll_options  TEXT[]  NOT NULL DEFAULT '{}';
ALTER TABLE fleets ADD COLUMN IF NOT EXISTS audience      TEXT    NOT NULL DEFAULT 'everyone';
ALTER TABLE fleets ADD COLUMN IF NOT EXISTS allow_replies BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE fleets DROP CONSTRAINT IF EXISTS fleets_audience_vocabulary;
ALTER TABLE fleets ADD CONSTRAINT fleets_audience_vocabulary
    CHECK (audience IN ('everyone', 'followers'));

CREATE TABLE IF NOT EXISTS fleet_poll_votes (
    fleet_id   UUID        NOT NULL REFERENCES fleets(id) ON DELETE CASCADE,
    voter_pial UUID        NOT NULL,
    option_idx INT         NOT NULL CHECK (option_idx >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (fleet_id, voter_pial)
);

CREATE TABLE IF NOT EXISTS fleet_mutes (
    muter_id   UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    muted_id   UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (muter_id, muted_id)
);

ALTER TABLE live_streams ADD COLUMN IF NOT EXISTS audience       TEXT    NOT NULL DEFAULT 'everyone';
ALTER TABLE live_streams ADD COLUMN IF NOT EXISTS lane           TEXT    NOT NULL DEFAULT '';
ALTER TABLE live_streams ADD COLUMN IF NOT EXISTS tip_goal_uaet  BIGINT  NOT NULL DEFAULT 0 CHECK (tip_goal_uaet >= 0);
ALTER TABLE live_streams ADD COLUMN IF NOT EXISTS tip_total_uaet BIGINT  NOT NULL DEFAULT 0 CHECK (tip_total_uaet >= 0);
ALTER TABLE live_streams ADD COLUMN IF NOT EXISTS pinned_body    TEXT    NOT NULL DEFAULT '';
ALTER TABLE live_streams ADD COLUMN IF NOT EXISTS heart_count    INT     NOT NULL DEFAULT 0 CHECK (heart_count >= 0);
ALTER TABLE live_streams ADD COLUMN IF NOT EXISTS chat_lines     INT     NOT NULL DEFAULT 0 CHECK (chat_lines >= 0);
ALTER TABLE live_streams ADD COLUMN IF NOT EXISTS save_replay    BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE live_streams DROP CONSTRAINT IF EXISTS live_streams_audience_vocabulary;
ALTER TABLE live_streams ADD CONSTRAINT live_streams_audience_vocabulary
    CHECK (audience IN ('everyone', 'followers', 'subscribers'));

CREATE TABLE IF NOT EXISTS live_tips (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    stream_id   UUID        NOT NULL REFERENCES live_streams(id) ON DELETE CASCADE,
    from_pial   UUID        NOT NULL,
    amount_uaet BIGINT      NOT NULL CHECK (amount_uaet > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_live_tips_stream ON live_tips(stream_id, created_at DESC);
