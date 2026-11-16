-- 0001_rank_state.sql
--
-- f33d3r_rank: every piece of ranking state AethyrRank owns. Before this the
-- LinUCB checkpoints lived in feed-engine's database (f33d3r_feed), written by
-- a process that did not own that schema, and the bandit learned through an
-- HTTP /feedback call that dropped whatever failed.
--
-- The intake is ranking.events on Sitra Achra. One consumer instance owns it
-- (pg advisory lock 7331_0041). Each batch it applies commits, in ONE
-- transaction: the models it changed, the reactions it learned or took back,
-- the event ids it applied, and the next offset of every partition it read.
-- On boot the consumer restores the models and resumes at those offsets, so the
-- stored state is always exactly "every event before these offsets, once".

-- One LinUCB model per (viewer or NEXUS shard, surface).
-- a_matrix: column-major dim×dim, b_vector: dim.
CREATE TABLE IF NOT EXISTS linucb_checkpoints (
    model_key    TEXT        PRIMARY KEY,
    dim          INT         NOT NULL CHECK (dim > 0),
    a_matrix     JSONB       NOT NULL,
    b_vector     JSONB       NOT NULL,
    update_count BIGINT      NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT linucb_checkpoints_shape CHECK (
        jsonb_typeof(a_matrix) = 'array' AND jsonb_array_length(a_matrix) = dim * dim
        AND jsonb_typeof(b_vector) = 'array' AND jsonb_array_length(b_vector) = dim)
);

-- A reaction that can be taken back (like, repost, bookmark, dislike, follow),
-- with the exact update it applied. The undo reverses precisely that update,
-- and a repeat of a reaction that already stands teaches nothing twice.
-- target_id is the work, or the account for a follow.
CREATE TABLE IF NOT EXISTS linucb_reactions (
    viewer_account TEXT             NOT NULL,
    target_id      TEXT             NOT NULL,
    signal         TEXT             NOT NULL
        CHECK (signal IN ('like', 'repost', 'bookmark', 'dislike', 'follow')),
    model_key      TEXT             NOT NULL,
    features       JSONB            NOT NULL,
    reward         DOUBLE PRECISION NOT NULL,
    applied_at     TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    PRIMARY KEY (viewer_account, target_id, signal)
);

-- Every event applied, by id. A producer that delivers the same event twice
-- (an outbox row produced, then the process died before marking it) is applied
-- once. Kept 30 days, far past the topic's 7-day retention and any outbox
-- redelivery an operator would make.
CREATE TABLE IF NOT EXISTS ranking_events_applied (
    event_id   UUID        PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ranking_events_applied_at
    ON ranking_events_applied (applied_at);

-- Where the consumer resumes: the next offset to read, per partition.
CREATE TABLE IF NOT EXISTS ranking_consumer_offsets (
    topic       TEXT        NOT NULL,
    partition   INT         NOT NULL,
    next_offset BIGINT      NOT NULL CHECK (next_offset >= 0),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (topic, partition)
);
