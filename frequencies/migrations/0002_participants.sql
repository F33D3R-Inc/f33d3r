-- Who was in a Frequency, in what role, and what was done to them.
--
-- These rows are durable participation history. They are NOT presence:
-- whether someone is connected right now is a TTL key in Redis, refreshed by
-- heartbeats and swept by the presence loop. A heartbeat never touches
-- PostgreSQL. A row here is written when someone joins, leaves, is removed —
-- the moments that matter after the session is over.

-- ── participation ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS frequency_participants (
    id              BIGSERIAL   PRIMARY KEY,
    frequency_id    UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial            TEXT        NOT NULL CHECK (pial LIKE 'pial:%'),
    -- Effective role while joined: host | cohost | speaker | listener.
    role            TEXT        NOT NULL CHECK (role IN ('host', 'cohost', 'speaker', 'listener')),
    state           TEXT        NOT NULL DEFAULT 'joined' CHECK (state IN ('joined', 'left', 'removed')),
    muted           BOOLEAN     NOT NULL DEFAULT FALSE,
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    left_at         TIMESTAMPTZ,
    removed_at      TIMESTAMPTZ,
    remove_reason   TEXT,
    removed_by      TEXT        CHECK (removed_by IS NULL OR removed_by LIKE 'pial:%')
);

-- A person is joined at most once at a time. Rejoining after leaving opens a
-- new row, so the history keeps every visit.
CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_participants_joined
    ON frequency_participants (frequency_id, pial) WHERE state = 'joined';
CREATE INDEX IF NOT EXISTS idx_frequency_participants_frequency
    ON frequency_participants (frequency_id, state);
CREATE INDEX IF NOT EXISTS idx_frequency_participants_pial
    ON frequency_participants (pial, joined_at DESC);

-- ── delegated roles ──────────────────────────────────────────────────────────
-- A co-host grant outlives a connection: a co-host who drops and rejoins is
-- still a co-host. Speaker is not stored here — it is the outcome of an
-- approved request and lasts for the session.
CREATE TABLE IF NOT EXISTS frequency_roles (
    frequency_id    UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial            TEXT        NOT NULL CHECK (pial LIKE 'pial:%'),
    role            TEXT        NOT NULL CHECK (role IN ('cohost', 'speaker')),
    granted_by      TEXT        NOT NULL CHECK (granted_by LIKE 'pial:%'),
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at      TIMESTAMPTZ,
    revoked_by      TEXT        CHECK (revoked_by IS NULL OR revoked_by LIKE 'pial:%')
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_roles_active
    ON frequency_roles (frequency_id, pial) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_frequency_roles_frequency
    ON frequency_roles (frequency_id) WHERE revoked_at IS NULL;

-- ── Request Mic ──────────────────────────────────────────────────────────────
-- A request carries a reason. That one line is the difference between forty
-- raised hands and a queue a host can actually read.
CREATE TABLE IF NOT EXISTS frequency_speaker_requests (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    frequency_id    UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial            TEXT        NOT NULL CHECK (pial LIKE 'pial:%'),
    reason          TEXT        NOT NULL DEFAULT '' CHECK (char_length(reason) <= 140),
    status          TEXT        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'approved', 'declined', 'withdrawn', 'expired')),
    upvotes         INTEGER     NOT NULL DEFAULT 0 CHECK (upvotes >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ,
    resolved_by     TEXT        CHECK (resolved_by IS NULL OR resolved_by LIKE 'pial:%')
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_requests_pending
    ON frequency_speaker_requests (frequency_id, pial) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_frequency_requests_queue
    ON frequency_speaker_requests (frequency_id, created_at) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS frequency_request_upvotes (
    request_id      UUID        NOT NULL REFERENCES frequency_speaker_requests(id) ON DELETE CASCADE,
    pial            TEXT        NOT NULL CHECK (pial LIKE 'pial:%'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, pial)
);

-- ── moderation ───────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS frequency_moderation_actions (
    id              BIGSERIAL   PRIMARY KEY,
    frequency_id    UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    -- 'service' when the platform acted (moderation termination), else a PIAL.
    actor           TEXT        NOT NULL,
    target          TEXT        CHECK (target IS NULL OR target LIKE 'pial:%'),
    action          TEXT        NOT NULL CHECK (action IN (
                        'mute', 'unmute', 'demote', 'remove', 'block', 'unblock',
                        'cohost_added', 'cohost_removed', 'lock', 'unlock',
                        'requests_closed', 'requests_opened', 'terminate')),
    reason          TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_frequency_moderation_frequency
    ON frequency_moderation_actions (frequency_id, created_at DESC);

-- Blocked from this Frequency: refused at Tune In, for the life of the object.
CREATE TABLE IF NOT EXISTS frequency_blocks (
    frequency_id    UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial            TEXT        NOT NULL CHECK (pial LIKE 'pial:%'),
    blocked_by      TEXT        NOT NULL,
    reason          TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (frequency_id, pial)
);

-- ── invites ──────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS frequency_invites (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    frequency_id    UUID        NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial            TEXT        NOT NULL CHECK (pial LIKE 'pial:%'),
    role            TEXT        NOT NULL CHECK (role IN ('speaker', 'cohost')),
    invited_by      TEXT        NOT NULL CHECK (invited_by LIKE 'pial:%'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    accepted_at     TIMESTAMPTZ,
    declined_at     TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_invites_open
    ON frequency_invites (frequency_id, pial) WHERE accepted_at IS NULL AND declined_at IS NULL;

-- ── idempotency ──────────────────────────────────────────────────────────────
-- A retried mutation returns the answer the first attempt produced rather
-- than producing a second effect. Keyed on the acting identity plus the key
-- the caller sent, so one caller's key cannot replay another's request.
CREATE TABLE IF NOT EXISTS frequency_idempotency (
    actor           TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL CHECK (char_length(idempotency_key) BETWEEN 1 AND 128),
    status          SMALLINT    NOT NULL,
    response        JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (actor, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_frequency_idempotency_age
    ON frequency_idempotency (created_at);
