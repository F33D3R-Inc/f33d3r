-- F33D3R iOS dev server — schema.
--
-- This is a SQLite transcription of the subset of feed-engine's Postgres
-- schema (internal/db/migrations/0001_baseline.sql and later) that the iOS
-- contract reads and writes. Table and column names are kept identical to the
-- real ones on purpose: the handlers in internal/api are written against these
-- names so they can be lifted into feed-engine/internal/api with the SQL
-- dialect swapped and nothing else.
--
-- Dialect notes:
--   * UUIDs are TEXT.
--   * Timestamps are TEXT in a fixed-width RFC 3339 UTC layout
--     (2006-01-02T15:04:05.000000000Z) so string comparison is time order.
--   * Postgres TEXT[] columns are JSON arrays in TEXT.
--   * BOOLEAN is INTEGER 0/1.
--
-- Two tables stand in for other brains, because this server runs alone:
--   * pial_signing_keys  — Elohim Veni's key authority.
--   * ledger_entries     — Ain Soph's ledger.

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- ── Identity spine ────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS pial_roots (
    pial_id       TEXT PRIMARY KEY,
    public_key    TEXT    NOT NULL DEFAULT '',
    role          TEXT    NOT NULL DEFAULT 'user',      -- user | admin | founder
    xp            INTEGER NOT NULL DEFAULT 0,
    is_verified   INTEGER NOT NULL DEFAULT 0,
    is_adult      INTEGER NOT NULL DEFAULT 1,
    is_minor      INTEGER NOT NULL DEFAULT 0,
    age_verified  INTEGER NOT NULL DEFAULT 0,
    kyc_tier      TEXT    NOT NULL DEFAULT 'none',   -- none | basic | soft | full
    kyc_submitted_at TEXT,
    created_at    TEXT    NOT NULL,
    deleted_at    TEXT,
    is_tombstoned INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS users (
    id         TEXT PRIMARY KEY,
    handle     TEXT NOT NULL UNIQUE,
    pial_id    TEXT NOT NULL REFERENCES pial_roots(pial_id),
    email_hash TEXT NOT NULL DEFAULT '',
    tier       TEXT NOT NULL DEFAULT 'free',            -- free | subscriber | creator
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS user_credentials (
    user_id          TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    password_hash    TEXT    NOT NULL,
    totp_enabled     INTEGER NOT NULL DEFAULT 0,
    backup_codes_set INTEGER NOT NULL DEFAULT 1,
    updated_at       TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS user_profiles (
    user_id              TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    display_name         TEXT    NOT NULL DEFAULT '',
    bio                  TEXT    NOT NULL DEFAULT '',
    pronouns             TEXT    NOT NULL DEFAULT '',
    location             TEXT    NOT NULL DEFAULT '',
    website              TEXT    NOT NULL DEFAULT '',
    avatar_url           TEXT    NOT NULL DEFAULT '',
    header_url           TEXT    NOT NULL DEFAULT '',
    theme_id             TEXT    NOT NULL DEFAULT 'void',
    accent_hex           TEXT    NOT NULL DEFAULT '',
    official_type        TEXT    NOT NULL DEFAULT '',   -- '' | government | business
    is_creator           INTEGER NOT NULL DEFAULT 0,
    is_adult_creator     INTEGER NOT NULL DEFAULT 0,
    is_private           INTEGER NOT NULL DEFAULT 0,
    content_setting      TEXT    NOT NULL DEFAULT 'default', -- safe_mode | default | adult_enabled
    show_sensitive       INTEGER NOT NULL DEFAULT 0,
    celebrations_enabled INTEGER NOT NULL DEFAULT 1,
    follower_count       INTEGER NOT NULL DEFAULT 0,
    following_count      INTEGER NOT NULL DEFAULT 0,
    post_count           INTEGER NOT NULL DEFAULT 0,
    pinned_work_id       TEXT,
    birthday_md_visibility   TEXT NOT NULL DEFAULT 'everyone',  -- everyone | followers | mutual_followers | only_me
    birthday_year_visibility TEXT NOT NULL DEFAULT 'only_me',
    country_code         TEXT,                               -- ISO-3166 alpha-2, NULL when unset
    social_links         TEXT    NOT NULL DEFAULT '{}',     -- JSON object, platform → URL
    external_tip_links   TEXT    NOT NULL DEFAULT '{}',     -- JSON object, rail → handle or address
    updated_at           TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS user_sessions (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash        TEXT NOT NULL UNIQUE,
    device_id         TEXT NOT NULL DEFAULT '',
    device_name       TEXT NOT NULL DEFAULT '',
    ip_address        TEXT NOT NULL DEFAULT '',
    pial_id           TEXT,
    active_account_id TEXT,
    created_at        TEXT NOT NULL,
    expires_at        TEXT NOT NULL,
    last_seen_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON user_sessions(user_id);

-- ── Social graph ──────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS follows (
    follower_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    following_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (follower_id, following_id)
);
CREATE INDEX IF NOT EXISTS idx_follows_following ON follows(following_id);

CREATE TABLE IF NOT EXISTS user_mutes (
    muter_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    muted_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (muter_id, muted_id)
);

CREATE TABLE IF NOT EXISTS blocks (
    blocker_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    blocked_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (blocker_id, blocked_id)
);

CREATE TABLE IF NOT EXISTS subscriptions (
    subscriber_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    creator_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status        TEXT NOT NULL DEFAULT 'active',
    created_at    TEXT NOT NULL,
    PRIMARY KEY (subscriber_id, creator_id)
);

-- ── Works ─────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS works (
    id                    TEXT PRIMARY KEY,
    cid                   TEXT    NOT NULL UNIQUE,
    author_id             TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    author_pial           TEXT    NOT NULL,
    body                  TEXT    NOT NULL DEFAULT '',
    kind                  TEXT    NOT NULL DEFAULT 'post'
        CHECK (kind IN ('post','reply','quote','poll','video','voice','thread_post','react_video')),
    media_urls            TEXT    NOT NULL DEFAULT '[]',
    is_nsfw               INTEGER NOT NULL DEFAULT 0,
    is_gore               INTEGER NOT NULL DEFAULT 0,
    is_blocked            INTEGER NOT NULL DEFAULT 0,
    expires_at            TEXT,
    created_at            TEXT    NOT NULL,
    deleted_at            TEXT,
    is_repost             INTEGER NOT NULL DEFAULT 0,
    repost_source_id      TEXT REFERENCES works(id) ON DELETE SET NULL,
    is_sensitive          INTEGER NOT NULL DEFAULT 0,
    subscriber_only       INTEGER NOT NULL DEFAULT 0,
    comment_gating        TEXT    NOT NULL DEFAULT 'open',
    scheduled_at          TEXT,
    tags                  TEXT    NOT NULL DEFAULT '[]',
    content_type          TEXT    NOT NULL DEFAULT 'text',
    poll_options          TEXT,
    poll_ends_at          TEXT,
    voice_url             TEXT,
    voice_duration_secs   REAL,
    video_master_url      TEXT,
    video_watermarked_url TEXT,
    video_poster_url      TEXT,
    video_duration_secs   REAL,
    video_width           INTEGER,
    video_height          INTEGER,
    react_layout          TEXT,
    lineage_pial          TEXT,
    lineage_handle        TEXT,
    is_edited             INTEGER NOT NULL DEFAULT 0,
    edited_at             TEXT,
    view_count            INTEGER NOT NULL DEFAULT 0,
    scan_state            TEXT    NOT NULL DEFAULT 'clean',
    score_band            TEXT    NOT NULL DEFAULT 'steady',
    price_uaet            INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_works_author_id  ON works(author_id);
CREATE INDEX IF NOT EXISTS idx_works_created_at ON works(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_works_kind       ON works(kind);

CREATE TABLE IF NOT EXISTS editions (
    id             TEXT PRIMARY KEY,
    work_id        TEXT    NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    cid            TEXT    NOT NULL UNIQUE,
    body           TEXT    NOT NULL DEFAULT '',
    edition_number INTEGER NOT NULL DEFAULT 1,
    created_at     TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS work_citations (
    id            TEXT PRIMARY KEY,
    work_id       TEXT NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    target_id     TEXT NOT NULL,
    citation_type TEXT NOT NULL,                       -- reply | quote
    created_at    TEXT NOT NULL,
    UNIQUE (work_id, target_id, citation_type)
);
CREATE INDEX IF NOT EXISTS idx_work_citations_target ON work_citations(target_id, citation_type);

CREATE TABLE IF NOT EXISTS work_reactions (
    work_id       TEXT NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    reactor_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reaction_type TEXT NOT NULL,                       -- like | repost | bookmark | dislike
    created_at    TEXT NOT NULL,
    PRIMARY KEY (work_id, reactor_id, reaction_type)
);
CREATE INDEX IF NOT EXISTS idx_work_reactions_reactor ON work_reactions(reactor_id, reaction_type, created_at DESC);

CREATE TABLE IF NOT EXISTS work_poll_votes (
    work_id    TEXT    NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    voter_id   TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    option_idx INTEGER NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (work_id, voter_id)
);

CREATE TABLE IF NOT EXISTS reports (
    id          TEXT PRIMARY KEY,
    reporter_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    work_id     TEXT,
    target_user TEXT,
    reason      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

-- ── Notifications ─────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS notifications (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type        TEXT NOT NULL,      -- like | repost | follow | reply | quote | mention | tip | subscribe | thread_reply
    actor_id    TEXT REFERENCES users(id) ON DELETE SET NULL,
    target_id   TEXT NOT NULL DEFAULT '',
    target_type TEXT NOT NULL DEFAULT 'work',
    payload     TEXT NOT NULL DEFAULT '{}',
    is_read     INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notif_user ON notifications(user_id, created_at DESC);

-- ── Stand-in: Elohim Veni key authority ───────────────────────────────────────

CREATE TABLE IF NOT EXISTS pial_signing_keys (
    pial_id        TEXT PRIMARY KEY,
    public_key_b64 TEXT NOT NULL,
    algorithm      TEXT NOT NULL DEFAULT 'ECDSA-P256',
    registered_at  TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

-- ── Stand-in: Ain Soph ledger ─────────────────────────────────────────────────
-- Amounts are µAET (1 AET = 1,000,000 µAET), signed from the row owner's point
-- of view: a credit is positive, a debit negative.

CREATE TABLE IF NOT EXISTS ledger_entries (
    id                TEXT PRIMARY KEY,
    pial_id           TEXT    NOT NULL,
    kind              TEXT    NOT NULL,   -- tip_received | tip_sent | subscription | unlock | sale | payout | airdrop
    amount_uaet       INTEGER NOT NULL,
    counterparty_pial TEXT,
    work_id           TEXT,
    stream_id         TEXT,
    status            TEXT    NOT NULL DEFAULT 'settled', -- settled | pending
    created_at        TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ledger_pial ON ledger_entries(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_work ON ledger_entries(work_id) WHERE work_id IS NOT NULL;

-- A work bought outright. One row per buyer per work is what makes the
-- purchase idempotent: a retry after a timeout finds the row and charges
-- nothing. The money itself is in ledger_entries (an `unlock` debit on the
-- buyer, a `sale` credit on the author, same work_id); this table is the
-- entitlement the read side hydrates `work_purchased_by_viewer` from.
CREATE TABLE IF NOT EXISTS work_purchases (
    work_id     TEXT    NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    buyer_id    TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    buyer_pial  TEXT    NOT NULL,
    author_pial TEXT    NOT NULL,
    amount_uaet INTEGER NOT NULL,
    created_at  TEXT    NOT NULL,
    PRIMARY KEY (work_id, buyer_id)
);
CREATE INDEX IF NOT EXISTS idx_work_purchases_buyer ON work_purchases(buyer_id, created_at DESC);

-- ── Visions: 24-hour ephemeral posts (feed-engine migration 0008) ─────────────

CREATE TABLE IF NOT EXISTS visions (
    id                  TEXT PRIMARY KEY,
    author_id           TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    author_pial         TEXT    NOT NULL,
    content_type        TEXT    NOT NULL DEFAULT 'text' CHECK (content_type IN ('text','image','video','share')),
    body                TEXT    NOT NULL DEFAULT '',
    artboard_background TEXT    NOT NULL DEFAULT 'void',
    artboard_typeface   TEXT    NOT NULL DEFAULT 'grotesk',
    artboard_type_scale TEXT    NOT NULL DEFAULT 'auto' CHECK (artboard_type_scale IN ('auto','s','m','l','xl')),
    artboard_align      TEXT    NOT NULL DEFAULT 'center' CHECK (artboard_align IN ('left','center','right')),
    media_urls          TEXT    NOT NULL DEFAULT '[]',
    shared_work_id      TEXT REFERENCES works(id) ON DELETE SET NULL,
    scan_state          TEXT    NOT NULL DEFAULT 'clean',
    is_nsfw             INTEGER NOT NULL DEFAULT 0,
    is_blocked          INTEGER NOT NULL DEFAULT 0,
    audience            TEXT    NOT NULL DEFAULT 'everyone' CHECK (audience IN ('everyone','subscribers')),
    allow_replies       INTEGER NOT NULL DEFAULT 1,
    poll_options        TEXT,
    created_at          TEXT    NOT NULL,
    expires_at          TEXT    NOT NULL,
    deleted_at          TEXT,
    metadata            TEXT    NOT NULL DEFAULT '{}',
    CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS idx_visions_author_live ON visions(author_id, expires_at, created_at);
CREATE INDEX IF NOT EXISTS idx_visions_expiry ON visions(expires_at);

CREATE TABLE IF NOT EXISTS vision_views (
    vision_id    TEXT NOT NULL REFERENCES visions(id) ON DELETE CASCADE,
    viewer_pial TEXT NOT NULL,
    viewed_at   TEXT NOT NULL,
    PRIMARY KEY (vision_id, viewer_pial)
);

CREATE TABLE IF NOT EXISTS vision_poll_votes (
    vision_id   TEXT    NOT NULL REFERENCES visions(id) ON DELETE CASCADE,
    voter_id   TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    option_idx INTEGER NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (vision_id, voter_id)
);

-- A reply to a vision is private: it goes to the author and nowhere else. In
-- production it lands in Vovin as a DM with the vision quoted; here it is a row
-- the author reads through their inbox.
CREATE TABLE IF NOT EXISTS vision_replies (
    id         TEXT PRIMARY KEY,
    vision_id   TEXT NOT NULL REFERENCES visions(id) ON DELETE CASCADE,
    from_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    to_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS vision_mutes (
    muter_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    muted_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (muter_id, muted_id)
);

-- ── Live (feed-engine migration 0009) ────────────────────────────────────────

CREATE TABLE IF NOT EXISTS live_streams (
    id               TEXT PRIMARY KEY,
    author_id        TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    author_pial      TEXT    NOT NULL,
    title            TEXT    NOT NULL DEFAULT '',
    description      TEXT    NOT NULL DEFAULT '',
    status           TEXT    NOT NULL DEFAULT 'idle' CHECK (status IN ('idle','live','ended')),
    started_at       TEXT,
    ended_at         TEXT,
    viewer_count     INTEGER NOT NULL DEFAULT 0,
    peak_viewers     INTEGER NOT NULL DEFAULT 0,
    poster_url       TEXT    NOT NULL DEFAULT '',
    playlist_url     TEXT    NOT NULL DEFAULT '',
    is_nsfw          INTEGER NOT NULL DEFAULT 0,
    is_blocked       INTEGER NOT NULL DEFAULT 0,
    scan_state       TEXT    NOT NULL DEFAULT 'clean',
    audience         TEXT    NOT NULL DEFAULT 'everyone' CHECK (audience IN ('everyone','subscribers')),
    lane             TEXT    NOT NULL DEFAULT '',
    tip_goal_uaet    INTEGER NOT NULL DEFAULT 0,
    notify_followers INTEGER NOT NULL DEFAULT 1,
    save_replay      INTEGER NOT NULL DEFAULT 1,
    pinned_body      TEXT    NOT NULL DEFAULT '',
    heart_count      INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_live_streams_one_open_per_author ON live_streams(author_id) WHERE status IN ('idle','live');
CREATE INDEX IF NOT EXISTS idx_live_streams_active ON live_streams(started_at DESC) WHERE status = 'live';

-- Chat is persisted here so a viewer who joins late gets the backlog and the
-- broadcaster's end-of-stream summary can count it. Kinds: chat | tip | system.
CREATE TABLE IF NOT EXISTS live_chat (
    id         TEXT PRIMARY KEY,
    stream_id  TEXT NOT NULL REFERENCES live_streams(id) ON DELETE CASCADE,
    user_id    TEXT REFERENCES users(id) ON DELETE SET NULL,
    kind       TEXT NOT NULL DEFAULT 'chat',
    body       TEXT NOT NULL DEFAULT '',
    amount_uaet INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_live_chat_stream ON live_chat(stream_id, created_at);

-- ── Frequencies: live audio rooms (Auralis migrations 0001, 0002) ────────────
--
-- A SQLite transcription of frequencies/migrations/0001_frequencies.sql and
-- 0002_participants.sql, column for column, so these queries lift into the
-- real brain unchanged. Two deliberate differences, both dialect and neither
-- meaning:
--
--   * The identity columns hold this server's bare PIAL uuids, the way
--     live_streams.author_pial does, rather than the naming plane's
--     `pial:<uuid>` form. The brain's LIKE 'pial:%' checks are therefore not
--     transcribed; every other CHECK is.
--   * Presence is a `joined` participant row, not a Redis TTL key. This
--     server has no Redis, and a room's counts must come from rows.

CREATE TABLE IF NOT EXISTS frequencies (
    id                      TEXT    PRIMARY KEY,
    -- Optimistic concurrency, bumped by every mutation, exactly as the brain
    -- does it. Nothing here races today (one SQLite writer), but the column
    -- is the contract's and the client may one day send it back.
    version                 INTEGER NOT NULL DEFAULT 1,
    host_pial               TEXT    NOT NULL,
    title                   TEXT    NOT NULL,
    description             TEXT    NOT NULL DEFAULT '',
    state                   TEXT    NOT NULL CHECK (state IN (
                                'draft', 'scheduled', 'starting', 'live', 'ending', 'ended',
                                'processing_replay', 'archived',
                                'cancelled', 'failed', 'moderation_terminated')),
    visibility              TEXT    NOT NULL DEFAULT 'public'
                                    CHECK (visibility IN ('public', 'followers', 'subscribers', 'private')),
    language                TEXT    NOT NULL DEFAULT 'en',
    adult_content           INTEGER NOT NULL DEFAULT 0,
    speaker_verity_min_tier INTEGER NOT NULL DEFAULT 0 CHECK (speaker_verity_min_tier BETWEEN 0 AND 3),
    scheduled_at            TEXT,
    started_at              TEXT,
    ended_at                TEXT,
    end_reason              TEXT,
    recording_enabled       INTEGER NOT NULL DEFAULT 0,
    replay_status           TEXT    NOT NULL DEFAULT 'none'
                                    CHECK (replay_status IN ('none', 'processing', 'ready', 'failed')),
    max_speakers            INTEGER NOT NULL DEFAULT 10   CHECK (max_speakers BETWEEN 1 AND 50),
    max_listeners           INTEGER NOT NULL DEFAULT 1000 CHECK (max_listeners BETWEEN 1 AND 100000),
    requests_open           INTEGER NOT NULL DEFAULT 1,
    locked                  INTEGER NOT NULL DEFAULT 0,
    media_node              TEXT,
    created_at              TEXT    NOT NULL,
    updated_at              TEXT    NOT NULL
);

-- One open Frequency per host. Scheduled ones may stack up; live ones may not.
CREATE UNIQUE INDEX IF NOT EXISTS uq_frequencies_one_open_per_host
    ON frequencies (host_pial) WHERE state IN ('starting', 'live', 'ending');
CREATE INDEX IF NOT EXISTS idx_frequencies_live
    ON frequencies (started_at DESC) WHERE state = 'live';
CREATE INDEX IF NOT EXISTS idx_frequencies_scheduled
    ON frequencies (scheduled_at ASC) WHERE state = 'scheduled';
CREATE INDEX IF NOT EXISTS idx_frequencies_ended
    ON frequencies (ended_at DESC) WHERE state IN ('ended', 'processing_replay', 'archived');
CREATE INDEX IF NOT EXISTS idx_frequencies_host
    ON frequencies (host_pial, created_at DESC);

-- Participation history. A `joined` row is presence here.
CREATE TABLE IF NOT EXISTS frequency_participants (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    frequency_id  TEXT    NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial          TEXT    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('host', 'cohost', 'speaker', 'listener')),
    state         TEXT    NOT NULL DEFAULT 'joined' CHECK (state IN ('joined', 'left', 'removed')),
    muted         INTEGER NOT NULL DEFAULT 0,
    joined_at     TEXT    NOT NULL,
    left_at       TEXT,
    removed_at    TEXT,
    remove_reason TEXT,
    removed_by    TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_participants_joined
    ON frequency_participants (frequency_id, pial) WHERE state = 'joined';
CREATE INDEX IF NOT EXISTS idx_frequency_participants_frequency
    ON frequency_participants (frequency_id, state);
CREATE INDEX IF NOT EXISTS idx_frequency_participants_pial
    ON frequency_participants (pial, joined_at DESC);

-- Delegated roles. A co-host grant outlives a connection.
CREATE TABLE IF NOT EXISTS frequency_roles (
    frequency_id TEXT NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial         TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('cohost', 'speaker')),
    granted_by   TEXT NOT NULL,
    granted_at   TEXT NOT NULL,
    revoked_at   TEXT,
    revoked_by   TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_roles_active
    ON frequency_roles (frequency_id, pial) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_frequency_roles_frequency
    ON frequency_roles (frequency_id) WHERE revoked_at IS NULL;

-- Request Mic. The reason is the difference between forty raised hands and a
-- queue a host can read.
CREATE TABLE IF NOT EXISTS frequency_speaker_requests (
    id           TEXT    PRIMARY KEY,
    frequency_id TEXT    NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial         TEXT    NOT NULL,
    reason       TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'approved', 'declined', 'withdrawn', 'expired')),
    upvotes      INTEGER NOT NULL DEFAULT 0 CHECK (upvotes >= 0),
    created_at   TEXT    NOT NULL,
    resolved_at  TEXT,
    resolved_by  TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_frequency_requests_pending
    ON frequency_speaker_requests (frequency_id, pial) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_frequency_requests_queue
    ON frequency_speaker_requests (frequency_id, created_at) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS frequency_request_upvotes (
    request_id TEXT NOT NULL REFERENCES frequency_speaker_requests(id) ON DELETE CASCADE,
    pial       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (request_id, pial)
);

CREATE TABLE IF NOT EXISTS frequency_moderation_actions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    frequency_id TEXT    NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    actor        TEXT    NOT NULL,
    target       TEXT,
    action       TEXT    NOT NULL CHECK (action IN (
                     'mute', 'unmute', 'demote', 'remove', 'block', 'unblock',
                     'cohost_added', 'cohost_removed', 'lock', 'unlock',
                     'requests_closed', 'requests_opened', 'terminate')),
    reason       TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_frequency_moderation_frequency
    ON frequency_moderation_actions (frequency_id, created_at DESC);

-- Blocked from this Frequency: refused at Tune In, for the life of the object.
CREATE TABLE IF NOT EXISTS frequency_blocks (
    frequency_id TEXT NOT NULL REFERENCES frequencies(id) ON DELETE CASCADE,
    pial         TEXT NOT NULL,
    blocked_by   TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    PRIMARY KEY (frequency_id, pial)
);
