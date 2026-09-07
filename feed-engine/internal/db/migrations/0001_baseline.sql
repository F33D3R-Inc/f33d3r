-- 0001_baseline.sql
-- Baseline schema, extracted verbatim from the pre-versioning migrate.go DDL blob.
-- Every statement here is idempotent (IF NOT EXISTS / IF EXISTS), so this file applies
-- cleanly to a fresh database and to any database that already ran the old boot replay.
-- FORWARD-ONLY: never edit this file after it has been applied anywhere. Its checksum is
-- recorded in schema_migrations and a drift is a hard boot failure, not a warning.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS users (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    handle     TEXT        NOT NULL UNIQUE,
    email_hash TEXT        NOT NULL DEFAULT '',
    tier       TEXT        NOT NULL DEFAULT 'free',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_profiles (
    user_id         UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    display_name    TEXT        NOT NULL DEFAULT '',
    bio             TEXT        NOT NULL DEFAULT '',
    pronouns        TEXT        NOT NULL DEFAULT '',
    location        TEXT        NOT NULL DEFAULT '',
    website         TEXT        NOT NULL DEFAULT '',
    avatar_url      TEXT        NOT NULL DEFAULT '',
    avatar_animated BOOLEAN     NOT NULL DEFAULT FALSE,
    header_url      TEXT        NOT NULL DEFAULT '',
    theme_id        TEXT        NOT NULL DEFAULT 'void',
    accent_hex      TEXT        NOT NULL DEFAULT '',
    jung_archetype  TEXT        NOT NULL DEFAULT '',
    pinned_track_id TEXT        NOT NULL DEFAULT '',
    social_links    JSONB       NOT NULL DEFAULT '{}',
    is_creator      BOOLEAN     NOT NULL DEFAULT FALSE,
    follower_count  INT         NOT NULL DEFAULT 0,
    following_count INT         NOT NULL DEFAULT 0,
    post_count      INT         NOT NULL DEFAULT 0,
    content_setting TEXT        NOT NULL DEFAULT 'default',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS posts (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id    UUID        REFERENCES posts(id) ON DELETE CASCADE,
    is_reply     BOOLEAN     NOT NULL DEFAULT FALSE,
    body         TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'text',
    media_urls   TEXT[]      NOT NULL DEFAULT '{}',
    tags         TEXT[]      NOT NULL DEFAULT '{}',
    is_edited    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Add new columns to existing posts table (idempotent)
ALTER TABLE posts ADD COLUMN IF NOT EXISTS parent_id    UUID REFERENCES posts(id) ON DELETE CASCADE;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS is_reply     BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS media_urls   TEXT[] NOT NULL DEFAULT '{}';

-- Quote reposts: store reference to original quoted post
ALTER TABLE posts ADD COLUMN IF NOT EXISTS quoted_post_id UUID REFERENCES posts(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_posts_quoted ON posts(quoted_post_id);

-- Sprint 0 / S0.6: HLS video assets. master.m3u8 + poster + duration. NULL when not a video.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_master_url     TEXT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_poster_url     TEXT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_duration_secs  REAL;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_width          INT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_height         INT;

-- Voice posts: audio-only posts with a waveform player.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS voice_url            TEXT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS voice_duration_secs  REAL;

CREATE INDEX IF NOT EXISTS idx_posts_created    ON posts(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_posts_author     ON posts(author_id);
CREATE INDEX IF NOT EXISTS idx_posts_type       ON posts(content_type);
CREATE INDEX IF NOT EXISTS idx_posts_parent     ON posts(parent_id);
CREATE INDEX IF NOT EXISTS idx_posts_fts        ON posts USING gin(to_tsvector('english', body));

-- Add tier column to users if missing
ALTER TABLE users ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'free';

-- Add content_setting column to user_profiles if missing
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS content_setting TEXT NOT NULL DEFAULT 'default';
-- 18+ "show sensitive content" (gore/IsGore) preference — independent of content_setting (porn axis)
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS show_sensitive  BOOLEAN NOT NULL DEFAULT FALSE;
-- Seasonal celebration themes (Pride/July 4/Halloween/Christmas), admin-scheduled
-- in the celebrations table. ON for everyone by default; this column is the
-- per-user OPT-OUT (FALSE = this user hides them).
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS celebrations_enabled BOOLEAN NOT NULL DEFAULT TRUE;
-- Correct the default on DBs where the column already exists (was added opt-in/FALSE).
ALTER TABLE user_profiles ALTER COLUMN celebrations_enabled SET DEFAULT TRUE;
-- Account privacy: TRUE = profile/posts hidden from public (logged-out) lookup,
-- visible only to approved followers. Default public, matching X.com behaviour.
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS is_private      BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS official_type   TEXT NOT NULL DEFAULT '';
-- official_type values: '' (none) | 'civic' | 'enterprise' | 'infrastructure' | 'ai'
-- Rename legacy values to canonical taxonomy names (idempotent)
UPDATE user_profiles SET official_type = 'civic'      WHERE official_type = 'government';
UPDATE user_profiles SET official_type = 'enterprise'  WHERE official_type = 'business';

-- Remove old updated_at from user_profiles if it has created_at (schema drift fix)
ALTER TABLE user_profiles DROP COLUMN IF EXISTS created_at;

CREATE TABLE IF NOT EXISTS post_metrics (
    post_id     UUID   PRIMARY KEY REFERENCES posts(id) ON DELETE CASCADE,
    likes       INT    NOT NULL DEFAULT 0,
    reposts     INT    NOT NULL DEFAULT 0,
    comments    INT    NOT NULL DEFAULT 0,
    saves       INT    NOT NULL DEFAULT 0,
    impressions INT    NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE post_metrics ADD COLUMN IF NOT EXISTS dislikes INT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS user_dislikes (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    post_id    UUID NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, post_id)
);
CREATE INDEX IF NOT EXISTS idx_user_dislikes_post ON user_dislikes(post_id);

CREATE TABLE IF NOT EXISTS follows (
    follower_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    following_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(follower_id, following_id)
);
CREATE INDEX IF NOT EXISTS idx_follows_following ON follows(following_id);
CREATE INDEX IF NOT EXISTS idx_follows_follower  ON follows(follower_id);

CREATE TABLE IF NOT EXISTS bookmarks (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    post_id    UUID NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(user_id, post_id)
);
CREATE INDEX IF NOT EXISTS idx_bookmarks_post ON bookmarks(post_id);

CREATE TABLE IF NOT EXISTS post_likes (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    post_id    UUID NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(user_id, post_id)
);
CREATE INDEX IF NOT EXISTS idx_post_likes_post ON post_likes(post_id);

-- post_reposts: migrate old schema (user_id PK) to new schema (reposter_id + surrogate PK)
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'post_reposts' AND column_name = 'user_id'
    ) AND NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'post_reposts' AND column_name = 'reposter_id'
    ) THEN
        DROP TABLE post_reposts;
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS post_reposts (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    post_id       UUID        NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    reposter_id   UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(post_id, reposter_id)
);
CREATE INDEX IF NOT EXISTS idx_reposts_reposter ON post_reposts(reposter_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_reposts_post     ON post_reposts(post_id);

CREATE TABLE IF NOT EXISTS feed_sessions (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    surface    TEXT        NOT NULL DEFAULT 'feed',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tracks (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title         TEXT        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    audio_url     TEXT        NOT NULL,
    cover_url     TEXT        NOT NULL DEFAULT '',
    duration_secs INT         NOT NULL DEFAULT 0,
    genre         TEXT        NOT NULL DEFAULT '',
    tags          TEXT[]      NOT NULL DEFAULT '{}',
    price_cents   INT         NOT NULL DEFAULT 0,
    is_free       BOOLEAN     NOT NULL DEFAULT TRUE,
    play_count    INT         NOT NULL DEFAULT 0,
    like_count    INT         NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_tracks_author  ON tracks(author_id);
CREATE INDEX IF NOT EXISTS idx_tracks_created ON tracks(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tracks_genre   ON tracks(genre);

CREATE TABLE IF NOT EXISTS track_likes (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    track_id   UUID NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(user_id, track_id)
);

-- Persistent audit trail of every event forwarded to AethyrRank.
-- Drives real velocity signals and offline analysis of ranking quality.
CREATE TABLE IF NOT EXISTS feedback_events (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     TEXT        NOT NULL,
    session_id  TEXT        NOT NULL,
    surface     TEXT        NOT NULL DEFAULT 'feed',
    content_id  TEXT        NOT NULL,
    event_type  TEXT        NOT NULL,
    position    INT         NOT NULL DEFAULT 0,
    dwell_ms    BIGINT      NOT NULL DEFAULT 0,
    is_explore  BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_feedback_content   ON feedback_events(content_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_feedback_user      ON feedback_events(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_feedback_event     ON feedback_events(event_type, created_at DESC);

-- LinUCB model checkpoints written by aethyrrank-engine.
-- Stored here so both services share the same Postgres instance.
CREATE TABLE IF NOT EXISTS linucb_checkpoints (
    surface      TEXT        PRIMARY KEY,
    a_matrix     JSONB       NOT NULL,
    b_vector     JSONB       NOT NULL,
    update_count BIGINT      NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Realm / XP progression
ALTER TABLE users ADD COLUMN IF NOT EXISTS realm        INT     NOT NULL DEFAULT 1;
ALTER TABLE users ADD COLUMN IF NOT EXISTS xp           BIGINT  NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS unread_count INT     NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS role         TEXT    NOT NULL DEFAULT 'user';
-- Account deactivation (written by DeactivateUser, read by trending/creator queries).
-- These columns were referenced in code but never created — add them idempotently.
ALTER TABLE users ADD COLUMN IF NOT EXISTS deactivated_at     TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS deactivated_reason TEXT NOT NULL DEFAULT '';

-- ── PIAL: Persistent Identity + Access Layer ──────────────────────────────────
-- Root identity anchor — minimal, immutable, never carries psychology or ranking.
-- Accounts are ephemeral shells. PIAL is the persistent soul.
CREATE TABLE IF NOT EXISTS pial_roots (
    pial_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    public_key    TEXT        NOT NULL DEFAULT '',
    state_hash    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ,
    is_tombstoned BOOLEAN     NOT NULL DEFAULT FALSE
);

-- Account → PIAL resolution (many accounts, one PIAL root)
CREATE TABLE IF NOT EXISTS pial_account_bindings (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id    UUID        NOT NULL REFERENCES pial_roots(pial_id),
    account_id UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    bound_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status     TEXT        NOT NULL DEFAULT 'active',
    is_primary BOOLEAN     NOT NULL DEFAULT TRUE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_pial_acct_active
    ON pial_account_bindings(account_id) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_pial_acct_pial ON pial_account_bindings(pial_id);

-- Capability layer — dynamic permissions derived from PIAL state, never from psychology
CREATE TABLE IF NOT EXISTS pial_capabilities (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id    UUID        NOT NULL REFERENCES pial_roots(pial_id),
    capability TEXT        NOT NULL,
    state      TEXT        NOT NULL DEFAULT 'granted',
    expires_at TIMESTAMPTZ,
    reason     TEXT        NOT NULL DEFAULT '',
    granted_by TEXT        NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_pial_cap_unique ON pial_capabilities(pial_id, capability);

-- Event ledger — append-only, never update or delete rows
CREATE TABLE IF NOT EXISTS pial_event_ledger (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id    UUID        NOT NULL REFERENCES pial_roots(pial_id),
    account_id UUID        REFERENCES users(id),
    event_type TEXT        NOT NULL,
    payload    JSONB       NOT NULL DEFAULT '{}',
    source     TEXT        NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_pial_ledger ON pial_event_ledger(pial_id, created_at DESC);

-- Link users to their PIAL root (convenience denorm — bindings table is authoritative)
ALTER TABLE users ADD COLUMN IF NOT EXISTS pial_id UUID REFERENCES pial_roots(pial_id);
CREATE INDEX IF NOT EXISTS idx_users_pial ON users(pial_id) WHERE pial_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS xp_events (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason     TEXT        NOT NULL,
    xp_delta   INT         NOT NULL,
    content_id TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_xp_events_user ON xp_events(user_id, created_at DESC);
-- Rename event_type → reason for tables created before this fix
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='xp_events' AND column_name='event_type') THEN
    ALTER TABLE xp_events RENAME COLUMN event_type TO reason;
  END IF;
END $$;

-- ── Elohim Veni: Security Brain ──────────────────────────────────────────────
-- These tables live here temporarily (shared Postgres, dev mode).
-- In production, Elohim Veni keeps its own database (moderation decisions + audit log),
-- but the PIAL record (feed-engine) is the authority; Elohim Veni writes capability
-- changes to it. Cross-service reads use signed capability tokens minted from PIAL.

-- User-submitted content reports — feeds the moderation queue.
CREATE TABLE IF NOT EXISTS content_reports (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    reporter_id  UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content_id   TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'post',
    reason       TEXT        NOT NULL,
    detail       TEXT        NOT NULL DEFAULT '',
    status       TEXT        NOT NULL DEFAULT 'pending',
    resolved_by  TEXT        NOT NULL DEFAULT '',
    resolved_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_reports_status  ON content_reports(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_reports_content ON content_reports(content_id);

-- Per-PIAL trust state — managed by Elohim Veni, read by all brains.
-- trust_level: 0-100. nsfw_tier: 0=none, 1=consume, 2=limited_post, 3=full_post, 4=creator.
-- enforcement_state: none | warning | restricted | suspended | terminated.
CREATE TABLE IF NOT EXISTS trust_scores (
    pial_id           UUID        PRIMARY KEY REFERENCES pial_roots(pial_id),
    trust_level       INT         NOT NULL DEFAULT 50,
    nsfw_tier         INT         NOT NULL DEFAULT 0,
    enforcement_state TEXT        NOT NULL DEFAULT 'none',
    violation_count   INT         NOT NULL DEFAULT 0,
    last_violation_at TIMESTAMPTZ,
    cooldown_until    TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Hashed identity signals for evasion resistance.
-- Raw values are NEVER stored — only SHA-256(signal + per-pial nonce).
-- Used for ban-evasion detection; false-positive reviewed before action.
CREATE TABLE IF NOT EXISTS identity_signals (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id     UUID        NOT NULL REFERENCES pial_roots(pial_id),
    signal_type TEXT        NOT NULL,
    signal_hash TEXT        NOT NULL,
    confidence  FLOAT       NOT NULL DEFAULT 0.5,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_signals_hash ON identity_signals(signal_type, signal_hash);
CREATE INDEX IF NOT EXISTS idx_signals_pial ON identity_signals(pial_id);

-- Immutable enforcement action log.
-- Every moderation decision is recorded here with its evidence and actor.
CREATE TABLE IF NOT EXISTS enforcement_actions (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id     UUID        NOT NULL REFERENCES pial_roots(pial_id),
    action_type TEXT        NOT NULL,
    reason      TEXT        NOT NULL DEFAULT '',
    evidence    JSONB       NOT NULL DEFAULT '{}',
    actor       TEXT        NOT NULL DEFAULT 'system',
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_enforcement_pial ON enforcement_actions(pial_id, created_at DESC);

-- Comment audience control — who can reply to a post
-- Values: 'open' (everyone) | 'followers' | 'verified' | 'none'
ALTER TABLE posts ADD COLUMN IF NOT EXISTS comment_gating TEXT NOT NULL DEFAULT 'open';

-- ── Auth: password credentials + passkey + TOTP ───────────────────────────────
CREATE TABLE IF NOT EXISTS user_credentials (
    user_id       UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    password_hash TEXT        NOT NULL DEFAULT '',
    totp_secret   TEXT        NOT NULL DEFAULT '',
    totp_enabled  BOOLEAN     NOT NULL DEFAULT FALSE,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Auth: persistent device sessions (replaces bare handle cookie) ────────────
CREATE TABLE IF NOT EXISTS user_sessions (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash    TEXT        NOT NULL UNIQUE,
    device_id     TEXT        NOT NULL DEFAULT '',
    device_name   TEXT        NOT NULL DEFAULT '',
    ip_address    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '30 days',
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sessions_token ON user_sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_user  ON user_sessions(user_id);

-- ── Onboarding: user roles, age verification, content preferences ─────────────
-- role_type: user | creator | official | minor_user | minor_creator
-- creator_type: musician | streamer | artist | podcaster | athlete | journalist | other
-- onboard_step tracks multi-step progress (0 = not started, 99 = complete)
-- role / is_adult / is_minor / is_age_verified moved to PIAL (pial_roots) — the single
-- system of record. Only creator_type / is_id_verified / content_prefs / onboard_* remain.
CREATE TABLE IF NOT EXISTS user_roles (
    user_id          UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    creator_type     TEXT        NOT NULL DEFAULT '',
    is_id_verified   BOOLEAN     NOT NULL DEFAULT FALSE,
    content_prefs    JSONB       NOT NULL DEFAULT '{}',
    onboard_step     INT         NOT NULL DEFAULT 0,
    onboard_done     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Backfill existing users with default role row
INSERT INTO user_roles (user_id, onboard_done)
SELECT id, TRUE FROM users
ON CONFLICT (user_id) DO NOTHING;

-- ── Notifications (real, DB-backed) ──────────────────────────────────────────
-- type: like | follow | reply | mention | message | system | tip | milestone
CREATE TABLE IF NOT EXISTS notifications (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type        TEXT        NOT NULL,
    actor_id    UUID        REFERENCES users(id) ON DELETE SET NULL,
    target_id   TEXT        NOT NULL DEFAULT '',
    target_type TEXT        NOT NULL DEFAULT 'post',
    payload     JSONB       NOT NULL DEFAULT '{}',
    is_read     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_notif_user   ON notifications(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_notif_unread ON notifications(user_id, is_read) WHERE is_read = FALSE;

-- NSFW tier bootstrap — initialise trust_scores row for new PIALs.
-- Trigger keeps trust_scores in sync whenever a new pial_root is created.
CREATE OR REPLACE FUNCTION pial_init_trust()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO trust_scores (pial_id) VALUES (NEW.pial_id) ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS trg_pial_init_trust ON pial_roots;
CREATE TRIGGER trg_pial_init_trust
    AFTER INSERT ON pial_roots
    FOR EACH ROW EXECUTE FUNCTION pial_init_trust();

-- Adult content
ALTER TABLE posts ADD COLUMN IF NOT EXISTS is_nsfw BOOLEAN NOT NULL DEFAULT FALSE;

-- Achievements
CREATE TABLE IF NOT EXISTS achievements (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL,
    icon        TEXT NOT NULL DEFAULT '🏅',
    tier        TEXT NOT NULL DEFAULT 'bronze'
);
CREATE TABLE IF NOT EXISTS user_achievements (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    achievement_id TEXT NOT NULL REFERENCES achievements(id),
    earned_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, achievement_id)
);
CREATE INDEX IF NOT EXISTS idx_user_ach ON user_achievements(user_id, earned_at DESC);

INSERT INTO achievements (id, name, description, icon, tier) VALUES
    ('first_post',      'First Steps',        'Published your first post',                               '✍️',  'bronze'),
    ('post_10',         'Content Creator',    '10 posts published',                                      '📝',  'bronze'),
    ('post_100',        'Content Machine',    '100 posts published',                                     '🔥',  'silver'),
    ('first_follower',  'Social Spark',       'Got your first follower',                                 '⚡',  'bronze'),
    ('followers_10',    'Rising Star',        '10 followers',                                            '⭐',  'bronze'),
    ('followers_50',    'Established',        '50 followers',                                            '🌟',  'silver'),
    ('followers_100',   'Influencer',         '100 followers',                                           '💫',  'gold'),
    ('first_like',      'Crowd Pleaser',      'Received your first like',                                '❤️',  'bronze'),
    ('liked_100',       'Going Viral',        'A post received 100 likes',                               '🚀',  'silver'),
    ('first_message',   'Connected',          'Sent your first message',                                 '💬',  'bronze'),
    ('creator_on',      'Creator Certified',  'Enabled creator monetization',                            '✦',   'gold'),
    ('first_tip',       'Tipped',             'Received your first tip',                                 '💰',  'silver'),
    ('kyc_tier1',       'Verified Human',     'Completed Tier 1 verification',                           '✓',   'bronze'),
    ('kyc_tier2',       'ID Verified',        'Completed government ID verification',                    '🛡️',  'silver'),
    ('kyc_tier3',       'Payout Unlocked',    'Completed full KYC — payout enabled',                    '💳',  'gold'),
    ('realm_adept',     'Adept',              'Reached Realm 4',                                         '🔮',  'gold'),
    ('realm_guardian',  'Guardian',           'Reached Realm 5',                                         '⚔️',  'platinum'),
    ('adult_creator',   '2257 Compliant',     'Adult content creator — 2257 records on file',            '🔞',  'gold'),
    ('clean_slate',     'Clean Slate',        '30 days with zero violations',                            '🌿',  'silver'),
    ('vision_first',      'The Awakening',  'Posted your first Vision',                              '👁',  'bronze'),
    ('vision_seer',       'Seer',           '10 Visions posted',                                     '🔮',  'bronze'),
    ('vision_oracle',     'The Oracle',     '50 Visions posted',                                     '✦',   'silver'),
    ('vision_quest',      'Vision Quest',   '7-day Vision posting streak',                           '🌟',  'gold'),
    ('vision_sight',      'The Sight',      'A Vision hit 100 views',                                '⚡',  'silver'),
    ('vision_revelation', 'Revelation',     'A Vision hit 1,000 views',                              '🚀',  'gold')
ON CONFLICT (id) DO NOTHING;

-- Creator subscription plans (mirrors Thessalon for feed-engine reads)
CREATE TABLE IF NOT EXISTS creator_plans (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    price_aet       INT  NOT NULL DEFAULT 0,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_plans_creator ON creator_plans(creator_id, active);

-- Subscriptions local table (mirrors Thessalon for feed-engine access)
CREATE TABLE IF NOT EXISTS subscriptions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscriber_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    creator_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan_id         TEXT NOT NULL DEFAULT '',
    price_aet       INT  NOT NULL DEFAULT 0,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '30 days',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status          TEXT NOT NULL DEFAULT 'active',
    UNIQUE(subscriber_id, creator_id)
);
CREATE INDEX IF NOT EXISTS idx_sub_creator ON subscriptions(creator_id, status);
CREATE INDEX IF NOT EXISTS idx_sub_viewer  ON subscriptions(subscriber_id, status);
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ;

-- ── Compliance tables ──────────────────────────────────────────────────────────

-- DMCA takedown requests
CREATE TABLE IF NOT EXISTS dmca_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id       TEXT NOT NULL UNIQUE,
    claimant_name   TEXT NOT NULL,
    claimant_email  TEXT NOT NULL,
    infringing_url  TEXT NOT NULL,
    original_url    TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending', -- pending | actioned | dismissed
    resolved_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_dmca_status ON dmca_requests(status, created_at DESC);

-- GDPR/CCPA data deletion requests
CREATE TABLE IF NOT EXISTS data_deletion_requests (
    user_id      UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    pial_id      TEXT NOT NULL DEFAULT '',
    requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    status       TEXT NOT NULL DEFAULT 'pending' -- pending | processing | complete | retained
);

-- CSAM scan audit log (every upload scanned, result logged)
CREATE TABLE IF NOT EXISTS csam_scan_log (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    media_url    TEXT NOT NULL,
    pial_id      TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT 'image',
    result       TEXT NOT NULL DEFAULT 'clean', -- clean | flagged | error
    score        REAL NOT NULL DEFAULT 0,
    scanned_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_csam_flagged ON csam_scan_log(result, scanned_at DESC) WHERE result != 'clean';

-- Law enforcement referral queue (CSAM reports to NCMEC)
CREATE TABLE IF NOT EXISTS law_enforcement_queue (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id      TEXT NOT NULL,
    media_url    TEXT NOT NULL,
    report_type  TEXT NOT NULL DEFAULT 'csam',
    reported_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ncmec_ref    TEXT DEFAULT ''  -- NCMEC CyberTipline reference number once submitted
);

-- User blocks — bidirectional: blocked content is invisible in both directions.
CREATE TABLE IF NOT EXISTS blocks (
    blocker_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    blocked_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (blocker_id, blocked_id)
);
CREATE INDEX IF NOT EXISTS idx_blocks_blocked ON blocks(blocked_id);

-- Email on users table (for password reset; optional, user-supplied)
ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS role  TEXT NOT NULL DEFAULT 'user';
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email) WHERE email != '';

-- Password reset tokens (one-time use, 1 hour TTL)
CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token      TEXT        NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_prt_token ON password_reset_tokens(token) WHERE used_at IS NULL;

-- Backup codes: sole account-recovery mechanism. Email is never stored or used.
CREATE TABLE IF NOT EXISTS backup_codes (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT        NOT NULL,
    used       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_backup_codes_user ON backup_codes(user_id) WHERE NOT used;

-- Canonical media entities — media exists independently of posts
-- Every video uploaded becomes a persistent canonical_media record tied to its PIAL creator.
CREATE TABLE IF NOT EXISTS canonical_media (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_user_id  UUID        REFERENCES users(id) ON DELETE SET NULL,
    creator_pial_id  UUID        REFERENCES pial_roots(pial_id) ON DELETE SET NULL,
    creator_handle   TEXT        NOT NULL DEFAULT '',
    master_url       TEXT        NOT NULL DEFAULT '',
    poster_url       TEXT        NOT NULL DEFAULT '',
    duration_secs    FLOAT       NOT NULL DEFAULT 0,
    width            INT         NOT NULL DEFAULT 0,
    height           INT         NOT NULL DEFAULT 0,
    rights_status    TEXT        NOT NULL DEFAULT 'open',
    total_views      BIGINT      NOT NULL DEFAULT 0,
    first_post_id    UUID,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_canon_creator ON canonical_media(creator_user_id);
CREATE INDEX IF NOT EXISTS idx_canon_pial    ON canonical_media(creator_pial_id);

ALTER TABLE posts ADD COLUMN IF NOT EXISTS canonical_media_id UUID REFERENCES canonical_media(id) ON DELETE SET NULL;

-- Prompt 2: post gate columns + reply preview avatars
ALTER TABLE posts ADD COLUMN IF NOT EXISTS is_sensitive           BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS subscriber_only        BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS latest_replier_handles TEXT[]  NOT NULL DEFAULT '{}';
ALTER TABLE posts ADD COLUMN IF NOT EXISTS latest_replier_avatars TEXT[]  NOT NULL DEFAULT '{}';

-- DOB-based age enforcement. date_of_birth is collected at onboarding and used
-- to set is_minor / is_adult deterministically server-side.
ALTER TABLE user_roles ADD COLUMN IF NOT EXISTS date_of_birth DATE;

-- Birthday visibility prefs only — the DOB itself lives solely on pial_roots.date_of_birth.
-- month+day shown publicly, year always private (per the visibility columns below).
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS show_birthday          BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS birthday_md_visibility   TEXT NOT NULL DEFAULT 'everyone';
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS birthday_year_visibility TEXT NOT NULL DEFAULT 'only_me';

-- Content lineage from content-scan dedup engine.
-- lineage_pial + lineage_handle are set when a video is detected as a duplicate
-- of an earlier upload. They identify the ORIGINAL uploader, not this post's author.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS lineage_pial   TEXT NOT NULL DEFAULT '';
ALTER TABLE posts ADD COLUMN IF NOT EXISTS lineage_handle TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_posts_lineage ON posts(lineage_handle) WHERE lineage_handle != '';

-- Astraon analytics stub: view_time_seconds tracks total video watch seconds per post.
-- Incremented by the feed-engine media-view event handler. Migrates to Astraon on go-live.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS view_time_seconds BIGINT NOT NULL DEFAULT 0;

-- ── Polls ─────────────────────────────────────────────────────────────────────
-- poll_options: array of option labels on the post itself (NULL when not a poll)
ALTER TABLE posts ADD COLUMN IF NOT EXISTS poll_options  TEXT[];
ALTER TABLE posts ADD COLUMN IF NOT EXISTS poll_ends_at  TIMESTAMPTZ;

-- poll_votes: one row per (voter, post), option_idx references poll_options array index
CREATE TABLE IF NOT EXISTS poll_votes (
    post_id    UUID NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    voter_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    option_idx INT  NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (post_id, voter_id)
);
CREATE INDEX IF NOT EXISTS idx_poll_votes_post ON poll_votes(post_id);

-- Backfill capabilities for any existing PIAL roots that have none.
-- Safe to run multiple times (ON CONFLICT DO NOTHING).
INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
SELECT pr.pial_id, cap.capability, 'granted', 'system_backfill'
FROM pial_roots pr
CROSS JOIN (
    VALUES
    ('POSTING'), ('REALM_PROGRESSION'), ('NEW_ACCOUNT_TRUST'),
    ('MESSAGING'), ('MUSIC_UPLOAD'), ('MEDIA_UPLOAD'), ('ADULT_CONTENT'),
    ('TIPPING'), ('LIVE_STREAMING'), ('API_ACCESS')
) AS cap(capability)
WHERE NOT EXISTS (
    SELECT 1 FROM pial_capabilities pc WHERE pc.pial_id = pr.pial_id
)
ON CONFLICT (pial_id, capability) DO NOTHING;

-- ── Vault recovery ────────────────────────────────────────────────────────────
-- Stores a server-encrypted private key blob for cross-device recovery.
-- Server never holds the plaintext — blob is wrapped with PBKDF2(recovery_pin).
CREATE TABLE IF NOT EXISTS vault_recovery (
    user_id    UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    blob_b64   TEXT NOT NULL,
    salt_b64   TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Feed surfaces ─────────────────────────────────────────────────────────────
-- Named interest surfaces. Phase 1: tag/content_type bridge routing.
-- Phase 2: Zior writes post_surface_scores on post.created.
CREATE TABLE IF NOT EXISTS feed_surfaces (
    id           TEXT PRIMARY KEY,
    label        TEXT        NOT NULL,
    emoji        TEXT        NOT NULL DEFAULT '',
    surface_type TEXT        NOT NULL DEFAULT 'interest',
    tags         TEXT[]      NOT NULL DEFAULT '{}',
    content_type TEXT        NOT NULL DEFAULT '',
    sort_order   INT         NOT NULL DEFAULT 0,
    is_active    BOOLEAN     NOT NULL DEFAULT TRUE
);

-- User pinned surface tabs (server-side, survives device changes).
CREATE TABLE IF NOT EXISTS user_surface_pins (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    surface_id TEXT NOT NULL REFERENCES feed_surfaces(id) ON DELETE CASCADE,
    sort_order INT  NOT NULL DEFAULT 0,
    pinned_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, surface_id)
);
CREATE INDEX IF NOT EXISTS idx_user_surface_pins ON user_surface_pins(user_id, sort_order);

-- Seed 12 interest surfaces. Safe to re-run (ON CONFLICT DO NOTHING).
INSERT INTO feed_surfaces (id, label, emoji, tags, content_type, sort_order) VALUES
('music',   'Music',   '🎵', '{}',                                             'audio', 10),
('video',   'Video',   '🎬', '{}',                                             'video', 20),
('art',     'Art',     '🎨', '{"art","digitalart","illustration","design"}',    '',      30),
('gaming',  'Gaming',  '🎮', '{"gaming","games","gamer","esports"}',           '',      40),
('tech',    'Tech',    '💻', '{"tech","coding","ai","programming","dev"}',      '',      50),
('fashion', 'Fashion', '👗', '{"fashion","style","ootd","streetwear"}',         '',      60),
('sports',  'Sports',  '⚽', '{"sports","football","basketball","fitness"}',    '',      70),
('food',    'Food',    '🍕', '{"food","cooking","recipe","foodie"}',            '',      80),
('crypto',  'Crypto',  '🪙', '{"crypto","web3","blockchain","nft","defi"}',     '',      90),
('film',    'Film',    '🎥', '{"film","movies","cinema","tv","series"}',         '',     100),
('books',   'Books',   '📚', '{"books","reading","literature","author"}',        '',     110),
('travel',  'Travel',  '✈️', '{"travel","wanderlust","adventure","explore"}',   '',     120)
ON CONFLICT (id) DO NOTHING;

-- AethyrRank gap: graph-derived interest vectors.
-- Stored as a REAL[] (8-dimensional normalised float vector). NULL = not yet computed.
-- Updated online after each like/save event (learning rate 0.08 exponential blend).
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS interest_vector REAL[] DEFAULT NULL;

-- ── Trust & Safety: Content Moderation States ─────────────────────────────────
-- scan_state lifecycle: pending_scan → clean | age_gated | flagged | human_review | blocked
-- pending_scan: post created, scan not yet complete — not shown in public feeds
-- clean:        scan cleared — normal visibility rules apply
-- age_gated:    nudity/adult content detected — content_gate overlay required
-- flagged:      borderline — still visible with content_gate, elevated in review queue
-- human_review: auto-escalated — hidden from public feed, awaiting moderator decision
-- blocked:      confirmed violation — post hard-removed from all feeds
-- Existing posts default to 'clean' (they were already public).
-- New posts are explicitly set to 'pending_scan' at INSERT time by the handler.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS scan_state TEXT NOT NULL DEFAULT 'clean';
CREATE INDEX IF NOT EXISTS idx_posts_scan_state ON posts(scan_state) WHERE scan_state IN ('pending_scan','human_review','flagged');

-- Scan result record per post — stores raw signals from content-scan.
-- One row per post. Updated in-place if re-scanned.
CREATE TABLE IF NOT EXISTS content_scan_results (
    post_id          TEXT        PRIMARY KEY,
    scan_version     TEXT        NOT NULL DEFAULT '2.0',
    nudity_score     FLOAT       NOT NULL DEFAULT 0.0,
    gore_score       FLOAT       NOT NULL DEFAULT 0.0,
    clickbait_score  FLOAT       NOT NULL DEFAULT 0.0,
    ocr_text         TEXT        NOT NULL DEFAULT '',
    hate_signals     TEXT[]      NOT NULL DEFAULT '{}',
    risk_level       TEXT        NOT NULL DEFAULT 'unknown',
    recommendation   TEXT        NOT NULL DEFAULT 'pending',
    signals          TEXT[]      NOT NULL DEFAULT '{}',
    is_duplicate     BOOLEAN     NOT NULL DEFAULT FALSE,
    duplicate_type   TEXT        NOT NULL DEFAULT '',
    original_post_id TEXT        NOT NULL DEFAULT '',
    scanned_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_by      TEXT        NOT NULL DEFAULT '',
    reviewed_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_scan_results_risk      ON content_scan_results(risk_level);
CREATE INDEX IF NOT EXISTS idx_scan_results_scanned   ON content_scan_results(scanned_at DESC);

-- Banned content hash registry — exact and near-duplicate matching.
-- SHA-256 of audio stream or file; phash of video/image frames.
-- Entries are added by moderators after confirming a violation.
-- content-scan checks incoming uploads against this table before storing.
CREATE TABLE IF NOT EXISTS banned_content_hashes (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    hash_type    TEXT        NOT NULL,  -- 'sha256' | 'phash' | 'audio_fp'
    hash_value   TEXT        NOT NULL,
    category     TEXT        NOT NULL,  -- 'csam' | 'gore' | 'hate' | 'spam'
    added_by     TEXT        NOT NULL DEFAULT 'system',
    added_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    note         TEXT        NOT NULL DEFAULT '',
    UNIQUE(hash_type, hash_value)
);
CREATE INDEX IF NOT EXISTS idx_banned_hashes_lookup ON banned_content_hashes(hash_type, hash_value);

-- Moderation action log for content (separate from user enforcement_actions).
-- Immutable — append only. Records every state transition on a post.
CREATE TABLE IF NOT EXISTS content_moderation_log (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    post_id    TEXT        NOT NULL,
    from_state TEXT        NOT NULL DEFAULT '',
    to_state   TEXT        NOT NULL,
    reason     TEXT        NOT NULL DEFAULT '',
    actor      TEXT        NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_content_mod_log_post ON content_moderation_log(post_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_content_mod_log_time ON content_moderation_log(created_at DESC);

-- scan_version 2.1: audio transcript column added to content_scan_results.
ALTER TABLE content_scan_results ADD COLUMN IF NOT EXISTS transcript TEXT NOT NULL DEFAULT '';

-- ── Identity / Role cleanup ────────────────────────────────────────────────────
-- Rename official_type values to user-facing labels (civic→government, enterprise→business).
-- Idempotent: only updates rows that still hold the old values.
UPDATE user_profiles SET official_type = 'government' WHERE official_type = 'civic';
UPDATE user_profiles SET official_type = 'business'   WHERE official_type = 'enterprise';

-- creator was incorrectly stored as users.role; it is a perm (is_creator boolean).
-- Migrate those rows: role → 'user', is_creator → TRUE.
UPDATE user_profiles SET is_creator = TRUE
WHERE user_id IN (SELECT id FROM users WHERE role = 'creator');
UPDATE users SET role = 'user' WHERE role = 'creator';

-- mobile_feed_view preference: "standard" (default X-style cards) or "reels" (TikTok-style snap).
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS mobile_feed_view TEXT NOT NULL DEFAULT 'standard';

-- ── Link preview cache (Open Graph metadata) ──────────────────────────────────
-- Keyed by URL. TTL enforced at app layer (7 days).
CREATE TABLE IF NOT EXISTS link_previews (
    url         TEXT        PRIMARY KEY,
    title       TEXT        NOT NULL DEFAULT '',
    description TEXT        NOT NULL DEFAULT '',
    image_url   TEXT        NOT NULL DEFAULT '',
    site_name   TEXT        NOT NULL DEFAULT '',
    fetched_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Maps a post to its link preview. At most one preview per post (first URL wins).
CREATE TABLE IF NOT EXISTS post_link_previews (
    post_id    UUID PRIMARY KEY REFERENCES posts(id) ON DELETE CASCADE,
    url        TEXT NOT NULL REFERENCES link_previews(url)
);
CREATE INDEX IF NOT EXISTS idx_post_link_previews_url ON post_link_previews(url);

-- Pinned post per user profile (one post pinned at a time).
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS pinned_post_id UUID REFERENCES posts(id) ON DELETE SET NULL;

-- Mutes: hide another user's posts without blocking.
CREATE TABLE IF NOT EXISTS user_mutes (
    muter_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    muted_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (muter_id, muted_id)
);
CREATE INDEX IF NOT EXISTS idx_user_mutes_muter ON user_mutes(muter_id);

-- Scheduled posts: scheduled_at = NULL means publish immediately.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS scheduled_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_posts_scheduled ON posts(scheduled_at) WHERE scheduled_at IS NOT NULL;

-- TOTP 2FA columns (already exist in user_credentials via earlier migration, safe to re-run).
ALTER TABLE user_credentials ADD COLUMN IF NOT EXISTS totp_secret  TEXT    NOT NULL DEFAULT '';
ALTER TABLE user_credentials ADD COLUMN IF NOT EXISTS totp_enabled BOOLEAN NOT NULL DEFAULT FALSE;

-- Topic / hashtag subscriptions: follow a hashtag to see it in a Topics feed surface.
CREATE TABLE IF NOT EXISTS user_topic_subscriptions (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tag        TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, tag)
);
CREATE INDEX IF NOT EXISTS idx_user_topic_subs ON user_topic_subscriptions(user_id);

-- Lists: curated timelines of users.
CREATE TABLE IF NOT EXISTS user_lists (
    id          UUID NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    owner_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_public   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_user_lists_owner ON user_lists(owner_id);

CREATE TABLE IF NOT EXISTS list_members (
    list_id  UUID NOT NULL REFERENCES user_lists(id) ON DELETE CASCADE,
    user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    added_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (list_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_list_members_list ON list_members(list_id);

-- ── Cashtags — stock ticker embeds attached to posts ─────────────────────────
-- Stores a price snapshot at post creation time so the card is historically
-- accurate. live_price is fetched on demand by the client.
CREATE TABLE IF NOT EXISTS post_cashtags (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    post_id     UUID        NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    ticker      TEXT        NOT NULL,
    company_name TEXT       NOT NULL DEFAULT '',
    exchange    TEXT        NOT NULL DEFAULT '',
    price_at_post NUMERIC(18,4) NOT NULL DEFAULT 0,
    change_pct  NUMERIC(8,4)  NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_post_cashtags_post ON post_cashtags(post_id);
CREATE INDEX IF NOT EXISTS idx_post_cashtags_ticker ON post_cashtags(ticker);

-- ── Org memberships ───────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS org_memberships (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_user_id  UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status       TEXT        NOT NULL DEFAULT 'pending_employee',
    initiated_by TEXT        NOT NULL DEFAULT 'employee',
    is_primary   BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, org_user_id)
);
CREATE INDEX IF NOT EXISTS idx_org_memberships_user   ON org_memberships(user_id);
CREATE INDEX IF NOT EXISTS idx_org_memberships_org    ON org_memberships(org_user_id);
CREATE INDEX IF NOT EXISTS idx_org_memberships_primary ON org_memberships(user_id) WHERE is_primary = TRUE AND status = 'approved';

-- ── Org verification applications ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS org_verification_applications (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_type     TEXT        NOT NULL,
    org_name     TEXT        NOT NULL DEFAULT '',
    org_website  TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    evidence_url TEXT        NOT NULL DEFAULT '',
    status       TEXT        NOT NULL DEFAULT 'pending',
    admin_notes  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_at  TIMESTAMPTZ,
    reviewed_by  UUID        REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS idx_org_verifications_status ON org_verification_applications(status, created_at);

-- ── Security & threat detection tables ────────────────────────────────────────
CREATE TABLE IF NOT EXISTS security_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type   TEXT NOT NULL,
    severity     TEXT NOT NULL DEFAULT 'low',
    user_id      UUID REFERENCES users(id) ON DELETE SET NULL,
    ip_address   TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT '',
    path         TEXT NOT NULL DEFAULT '',
    details      JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_security_events_type     ON security_events(event_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_security_events_ip       ON security_events(ip_address, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_security_events_severity ON security_events(severity, created_at DESC);

CREATE TABLE IF NOT EXISTS blocked_ips (
    ip_address   TEXT PRIMARY KEY,
    reason       TEXT NOT NULL DEFAULT '',
    blocked_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    auto_blocked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS admin_audit_log (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id   TEXT NOT NULL DEFAULT '',
    details     JSONB NOT NULL DEFAULT '{}',
    ip_address  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_admin_audit_admin ON admin_audit_log(admin_id, created_at DESC);

-- Device fingerprinting — one row per (user, device_hash).
-- device_hash = sha256(user_agent). Fires a new_device_login security event on first login
-- from an unseen device, enabling account takeover detection.
CREATE TABLE IF NOT EXISTS user_devices (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_hash TEXT        NOT NULL,
    ip_address  TEXT        NOT NULL DEFAULT '',
    user_agent  TEXT        NOT NULL DEFAULT '',
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, device_hash)
);
CREATE INDEX IF NOT EXISTS idx_user_devices_user ON user_devices(user_id);

-- Long-form articles — creator-authored Markdown posts with title, cover, slug.
-- status: draft | published | archived
-- slug is globally unique; on conflict a numeric suffix is appended server-side.
CREATE TABLE IF NOT EXISTS articles (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug         TEXT        NOT NULL UNIQUE,
    title        TEXT        NOT NULL DEFAULT '',
    body         TEXT        NOT NULL DEFAULT '',
    body_html    TEXT        NOT NULL DEFAULT '',
    excerpt      TEXT        NOT NULL DEFAULT '',
    cover_url    TEXT        NOT NULL DEFAULT '',
    status       TEXT        NOT NULL DEFAULT 'draft',
    published_at TIMESTAMPTZ,
    view_count   BIGINT      NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_articles_author    ON articles(author_id, status);
CREATE INDEX IF NOT EXISTS idx_articles_published ON articles(published_at DESC) WHERE status = 'published';

-- Post translation cache — server-side translated text keyed per post.
-- translated_lang is the ISO 639-1 code detected by the translation API.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS translated_body TEXT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS translated_lang TEXT;


-- Adult Creator account type.
-- is_adult_creator: role is active (Verity age verification passed).
-- adult_creator_pending: user selected Adult Creator at onboarding but
--   has not completed Verity verification yet.
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS is_adult_creator      BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS adult_creator_pending BOOLEAN NOT NULL DEFAULT FALSE;

-- dm_filter_mode controls whose DMs go to requests vs main inbox.
-- 'all' = anyone (default, preserves existing behaviour)
-- 'followers' = non-followed senders are filtered to the requests tab
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS dm_filter_mode TEXT NOT NULL DEFAULT 'all';

-- Leaderboard: ISO 3166-1 alpha-2 country code, set by user in profile settings.
-- Used to scope the daily achievements leaderboard to the viewer's country.
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS country_code VARCHAR(2) DEFAULT NULL;
CREATE INDEX IF NOT EXISTS idx_user_profiles_country_code ON user_profiles(country_code) WHERE country_code IS NOT NULL;

-- Image canonical ownership: image_url maps a Caeor asset URL to its original uploader.
-- Enables lineage attribution when a different user uploads the same image later.
-- master_url is video-only; image_url is the separate column for static images.
ALTER TABLE canonical_media ADD COLUMN IF NOT EXISTS image_url TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_canon_image_url ON canonical_media(image_url) WHERE image_url != '';

-- Unique constraint on canonical_media(master_url): first uploader owns the canonical
-- video asset. Subsequent identical uploads get ON CONFLICT DO NOTHING + redirected.
CREATE UNIQUE INDEX IF NOT EXISTS idx_canon_master_url
    ON canonical_media(master_url) WHERE master_url != '';

-- Durable media fingerprint store — content-scan writes here instead of SQLite so
-- fingerprints survive container restarts. One row per unique SHA-256 content hash.
CREATE TABLE IF NOT EXISTS media_fingerprints (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id          TEXT        NOT NULL DEFAULT '',
    post_id           TEXT        NOT NULL DEFAULT '',
    uploader_pial     TEXT        NOT NULL DEFAULT '',
    uploader_handle   TEXT        NOT NULL DEFAULT '',
    sha256            TEXT        NOT NULL DEFAULT '',
    audio_fingerprint TEXT        NOT NULL DEFAULT '',
    perceptual_hash   TEXT        NOT NULL DEFAULT '',
    is_nsfw           BOOLEAN     NOT NULL DEFAULT FALSE,
    image_url         TEXT        NOT NULL DEFAULT '',
    scanned_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_media_fp_sha256 ON media_fingerprints(sha256)           WHERE sha256           != '';
CREATE INDEX        IF NOT EXISTS idx_media_fp_asset  ON media_fingerprints(asset_id)          WHERE asset_id         != '';
CREATE INDEX        IF NOT EXISTS idx_media_fp_post   ON media_fingerprints(post_id)           WHERE post_id          != '';
CREATE INDEX        IF NOT EXISTS idx_media_fp_phash  ON media_fingerprints(perceptual_hash)   WHERE perceptual_hash  != '';

-- Pre-upload video dedup gate. Populated at TUS assembly time with the raw-file SHA-256
-- before any transcoding begins. The client hashes the file locally and queries this
-- table before sending a single byte — exactly matching how image dedup works.
CREATE TABLE IF NOT EXISTS video_raw_hashes (
    sha256           TEXT        PRIMARY KEY,
    uploader_pial    TEXT        NOT NULL DEFAULT '',
    uploader_handle  TEXT        NOT NULL DEFAULT '',
    post_id          TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Articles extended fields
ALTER TABLE articles ADD COLUMN IF NOT EXISTS publication_id UUID;
ALTER TABLE articles ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE articles ADD COLUMN IF NOT EXISTS is_featured BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE articles ADD COLUMN IF NOT EXISTS featured_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_articles_pub ON articles(publication_id) WHERE publication_id IS NOT NULL;

-- Article versioning
CREATE TABLE IF NOT EXISTS article_versions (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    article_id   UUID        NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    author_id    UUID        NOT NULL REFERENCES users(id),
    version_num  INT         NOT NULL DEFAULT 1,
    title        TEXT        NOT NULL DEFAULT '',
    body         TEXT        NOT NULL DEFAULT '',
    body_html    TEXT        NOT NULL DEFAULT '',
    excerpt      TEXT        NOT NULL DEFAULT '',
    cover_url    TEXT        NOT NULL DEFAULT '',
    reason       TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_article_versions_article ON article_versions(article_id, created_at DESC);

-- Publications
CREATE TABLE IF NOT EXISTS publications (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    handle       TEXT        NOT NULL UNIQUE,
    name         TEXT        NOT NULL DEFAULT '',
    bio          TEXT        NOT NULL DEFAULT '',
    avatar_url   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_publications_owner ON publications(owner_id);

CREATE TABLE IF NOT EXISTS publication_members (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    publication_id  UUID        NOT NULL REFERENCES publications(id) ON DELETE CASCADE,
    user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role            TEXT        NOT NULL DEFAULT 'contributor',
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(publication_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_pub_members_pub ON publication_members(publication_id);

-- Article social signals
CREATE TABLE IF NOT EXISTS article_likes (
    article_id   UUID        NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (article_id, user_id)
);
CREATE TABLE IF NOT EXISTS article_bookmarks (
    article_id   UUID        NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    user_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (article_id, user_id)
);

-- FTS index for articles
CREATE INDEX IF NOT EXISTS idx_articles_fts ON articles USING gin(to_tsvector('english', title || ' ' || body));

-- Pre-upload image dedup gate. Populated after a successful Caeor upload with the raw-file
-- SHA-256 so future browser uploads of the same file are blocked before a byte is sent.
CREATE TABLE IF NOT EXISTS image_raw_hashes (
    sha256           TEXT        PRIMARY KEY,
    uploader_pial    TEXT        NOT NULL DEFAULT '',
    uploader_handle  TEXT        NOT NULL DEFAULT '',
    media_url        TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- External tip payment links. Stores normalized usernames only (no $ @ or URL prefixes).
-- Keys: cashapp | venmo | paypal | kofi | buymeacoffee. Empty string = not set.
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS external_tip_links JSONB NOT NULL DEFAULT '{}';

-- ── PIAL consolidation: human-level identity attributes ───────────────────────
-- PIAL is the only auth identity. Birthday, KYC tier, and operational status live
-- here so every brain can call Nantar once and get the full picture.
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS date_of_birth   DATE;
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS kyc_tier        TEXT NOT NULL DEFAULT 'none';
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS kyc_verified_at TIMESTAMPTZ;
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS age_verified    BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS status          TEXT NOT NULL DEFAULT 'active';
-- PIAL is the single system of record for identity/authz. role lives here (was users.role +
-- user_roles.role_type); perms live in pial_capabilities. dob_locked is set TRUE once age is
-- verified by eKYC/admin so a verified adult can no longer rewrite their DOB to evade the age gate.
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS role            TEXT NOT NULL DEFAULT 'user';
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS dob_locked      BOOLEAN NOT NULL DEFAULT FALSE;
-- is_adult / is_minor stored on PIAL (set from DOB) so the many SQL readers can join PIAL
-- directly; replaces the dropped user_roles.is_adult / is_minor.
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS is_adult        BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE pial_roots ADD COLUMN IF NOT EXISTS is_minor        BOOLEAN NOT NULL DEFAULT FALSE;
-- Backfill is_adult/is_minor from DOB where known, else from the old user_roles flags.
UPDATE pial_roots pr SET
  is_adult = CASE WHEN pr.date_of_birth IS NOT NULL
                  THEN (date_part('year', age(pr.date_of_birth)) >= 18) ELSE pr.is_adult END,
  is_minor = CASE WHEN pr.date_of_birth IS NOT NULL
                  THEN (date_part('year', age(pr.date_of_birth)) < 18)  ELSE pr.is_minor END;
-- The one-time seed of is_adult/is_minor/role/age_verified from the old user_roles /
-- user_profiles columns (for accounts without a DOB) lives in scripts/fold-auth-to-pial.sql —
-- run it once per environment before these columns are dropped below.

-- Merge is_tombstoned boolean into status enum (is_tombstoned kept for backward compat reads)
UPDATE pial_roots SET status = 'tombstoned' WHERE is_tombstoned = TRUE AND status = 'active';

-- Backfill PIAL DOB from the onboarding audit source (user_roles.date_of_birth)
-- for accounts created before pial_roots.date_of_birth existed.
UPDATE pial_roots pr
SET date_of_birth = ur.date_of_birth
FROM pial_account_bindings pab
JOIN user_roles ur ON ur.user_id = pab.account_id
WHERE pab.pial_id = pr.pial_id
  AND ur.date_of_birth IS NOT NULL
  AND pr.date_of_birth IS NULL;

-- Anyone with a birthday is at minimum basic KYC
UPDATE pial_roots
SET kyc_tier = 'basic', age_verified = TRUE
WHERE date_of_birth IS NOT NULL AND kyc_tier = 'none';

-- DOB consolidation: pial_roots.date_of_birth is the SOLE source of truth.
-- Rescue any legacy self-edited birthday that predates PIAL custody into PIAL,
-- then drop the redundant mirror column for good. No code reads or writes it.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_name = 'user_profiles' AND column_name = 'birthday') THEN
    UPDATE pial_roots pr
    SET date_of_birth = up.birthday
    FROM pial_account_bindings pab
    JOIN user_profiles up ON up.user_id = pab.account_id
    WHERE pab.pial_id = pr.pial_id
      AND up.birthday IS NOT NULL
      AND pr.date_of_birth IS NULL
      AND pr.dob_locked = FALSE;
    ALTER TABLE user_profiles DROP COLUMN birthday;
  END IF;
END $$;

-- ── Session → PIAL: sessions bind to PIAL, not just account ──────────────────
-- active_account_id replaces user_id as the mutable account pointer.
-- user_id kept for backward compat during migration window.
ALTER TABLE user_sessions ADD COLUMN IF NOT EXISTS pial_id           UUID REFERENCES pial_roots(pial_id);
ALTER TABLE user_sessions ADD COLUMN IF NOT EXISTS active_account_id UUID REFERENCES users(id);

-- Backfill existing sessions from pial_account_bindings
UPDATE user_sessions us
SET pial_id           = pab.pial_id,
    active_account_id = us.user_id
FROM pial_account_bindings pab
WHERE pab.account_id = us.user_id
  AND pab.status = 'active'
  AND us.pial_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_sessions_pial ON user_sessions(pial_id) WHERE pial_id IS NOT NULL;

-- ── Posts: reposts as first-class post events ─────────────────────────────────
-- is_repost=TRUE rows have no body; repost_source_id points to the original.
-- deleted_at is the soft-delete marker for un-repost and future post deletion.
-- All feed queries filter deleted_at IS NULL via blockedFilter.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS is_repost        BOOLEAN   NOT NULL DEFAULT FALSE;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS repost_source_id UUID      REFERENCES posts(id) ON DELETE SET NULL;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS deleted_at       TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_posts_repost      ON posts(repost_source_id) WHERE is_repost = TRUE;
CREATE INDEX IF NOT EXISTS idx_posts_deleted     ON posts(deleted_at)       WHERE deleted_at IS NOT NULL;

-- ── Backfill existing post_reposts → first-class post rows ──────────────────
-- post_reposts is now a legacy table. Existing repost records become is_repost=TRUE
-- post rows so EnrichPostsWithInteractions and GetUserReposts work correctly.
-- old created_at is preserved so they appear in historical feed position (not current).
INSERT INTO posts (author_id, body, content_type, is_repost, repost_source_id, scan_state, created_at)
SELECT pr.reposter_id, '', 'repost', TRUE, pr.post_id, 'clean', pr.created_at
FROM post_reposts pr
WHERE NOT EXISTS (
    SELECT 1 FROM posts p
    WHERE p.is_repost = TRUE AND p.author_id = pr.reposter_id AND p.repost_source_id = pr.post_id
);

INSERT INTO post_metrics (post_id)
SELECT id FROM posts WHERE is_repost = TRUE
ON CONFLICT DO NOTHING;

-- ── Content receipts: permanent provenance anchored to PIAL ──────────────────
CREATE TABLE IF NOT EXISTS content_receipts (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id          UUID        NOT NULL REFERENCES pial_roots(pial_id),
    post_id          UUID        REFERENCES posts(id) ON DELETE SET NULL,
    asset_id         UUID,
    minted_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    body_hash        TEXT        NOT NULL DEFAULT '',
    kyc_tier_at_mint TEXT        NOT NULL DEFAULT 'none'
);
CREATE INDEX IF NOT EXISTS idx_receipts_pial ON content_receipts(pial_id, minted_at DESC);
CREATE INDEX IF NOT EXISTS idx_receipts_post ON content_receipts(post_id);

-- ── Abraxas Shield config: admin-tunable thresholds and keyword lists ─────────
-- Single-row config table (id=1 always). Abraxas Shield polls this on startup
-- and every 5 minutes. All threshold values are 0.0–1.0.
CREATE TABLE IF NOT EXISTS abraxas_config (
    id                          INT         PRIMARY KEY DEFAULT 1,
    nudity_block_threshold      FLOAT8      NOT NULL DEFAULT 0.70,
    nudity_review_threshold     FLOAT8      NOT NULL DEFAULT 0.45,
    clickbait_block_threshold   FLOAT8      NOT NULL DEFAULT 0.70,
    clickbait_review_threshold  FLOAT8      NOT NULL DEFAULT 0.55,
    gore_block_threshold        FLOAT8      NOT NULL DEFAULT 0.80,
    gore_review_threshold       FLOAT8      NOT NULL DEFAULT 0.50,
    custom_hate_keywords        TEXT[]      NOT NULL DEFAULT '{}',
    custom_violence_keywords    TEXT[]      NOT NULL DEFAULT '{}',
    custom_spam_keywords        TEXT[]      NOT NULL DEFAULT '{}',
    enable_bot_detection        BOOLEAN     NOT NULL DEFAULT TRUE,
    enable_spam_detection       BOOLEAN     NOT NULL DEFAULT TRUE,
    auto_approve_unknown        BOOLEAN     NOT NULL DEFAULT FALSE,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by                  TEXT        NOT NULL DEFAULT 'system'
);
INSERT INTO abraxas_config (id) VALUES (1) ON CONFLICT DO NOTHING;

-- ── Celebrations: admin-scheduled seasonal themes ────────────────────────────
-- One row per theme. The active row (enabled AND today within [start_date,end_date],
-- narrowest range wins — see db.GetActiveCelebration) tags the shell as
-- body.celebrate-<theme>; the like-button effect + flair CSS live in
-- static/css/celebrations.css. ON for everyone by default; users opt out via
-- user_profiles.celebrations_enabled. Admin edits dates + on/off from the
-- Celebrations admin panel. A single day = start_date == end_date.
CREATE TABLE IF NOT EXISTS celebrations (
    theme       TEXT        PRIMARY KEY,
    name        TEXT        NOT NULL,
    start_date  DATE        NOT NULL,
    end_date    DATE        NOT NULL,
    enabled     BOOLEAN     NOT NULL DEFAULT FALSE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by  TEXT        NOT NULL DEFAULT 'system'
);
-- Seed the curated catalog (admin tweaks dates/on-off; dates are re-scheduled per year).
INSERT INTO celebrations (theme, name, start_date, end_date, enabled) VALUES
    ('pride',     'Pride',            '2026-06-01', '2026-06-30', TRUE),
    ('july4',     'Independence Day', '2026-07-04', '2026-07-04', TRUE),
    ('halloween', 'Halloween',        '2026-10-01', '2026-10-31', TRUE),
    ('christmas', 'Christmas',        '2026-12-01', '2026-12-26', TRUE)
ON CONFLICT (theme) DO NOTHING;

-- ── Abraxas feedback loop: admin moderation decisions → signal training ───────
-- Each row records which signals triggered the flag and how the admin ruled.
-- Abraxas Shield queries this table to suppress high false-positive signals.
CREATE TABLE IF NOT EXISTS scan_feedback (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    post_id TEXT NOT NULL,
    signals TEXT[] NOT NULL DEFAULT '{}',
    admin_decision TEXT NOT NULL,   -- 'approve' | 'age_gate' | 'block'
    actioned_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_scan_feedback_signals ON scan_feedback USING GIN(signals);
CREATE INDEX IF NOT EXISTS idx_scan_feedback_created ON scan_feedback(created_at DESC);

-- Visions: ephemeral posts (camera-only, 24h TTL)
ALTER TABLE posts ADD COLUMN IF NOT EXISTS kind       TEXT NOT NULL DEFAULT 'post';
ALTER TABLE posts ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_posts_kind    ON posts(kind);
CREATE INDEX IF NOT EXISTS idx_posts_expires ON posts(expires_at) WHERE expires_at IS NOT NULL;
-- Brand rename: fleet → vision (idempotent, updates any existing rows)
UPDATE posts SET kind = 'vision' WHERE kind = 'fleet';

-- ── Phase 1: content-addressed event-chain schema ────────────────────────────
-- Replaces the monolithic posts table with a clean separation of concerns.
-- posts table is NOT dropped here — handlers still reference it.
-- Cutover (DROP TABLE posts) happens in a later phase once all queries migrate.

-- Gore surface + block gate on existing posts table
ALTER TABLE posts ADD COLUMN IF NOT EXISTS is_gore    BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS is_blocked BOOLEAN NOT NULL DEFAULT FALSE;

-- works: content-addressed authored work (replaces posts)
CREATE TABLE IF NOT EXISTS works (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  cid         TEXT        UNIQUE NOT NULL,
  author_id   UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  author_pial UUID        NOT NULL,
  body        TEXT        NOT NULL DEFAULT '',
  kind        TEXT        NOT NULL DEFAULT 'post',
  media_urls  TEXT[]      NOT NULL DEFAULT '{}',
  is_nsfw     BOOLEAN     NOT NULL DEFAULT FALSE,
  is_gore     BOOLEAN     NOT NULL DEFAULT FALSE,
  is_blocked  BOOLEAN     NOT NULL DEFAULT FALSE,
  expires_at  TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  deleted_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_works_author_id  ON works(author_id);
CREATE INDEX IF NOT EXISTS idx_works_created_at ON works(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_works_kind       ON works(kind);
CREATE INDEX IF NOT EXISTS idx_works_is_nsfw    ON works(is_nsfw)  WHERE is_nsfw = TRUE;
CREATE INDEX IF NOT EXISTS idx_works_is_gore    ON works(is_gore)  WHERE is_gore = TRUE;

-- ── works: extend with all columns migrated from posts ───────────────────────
-- Content classification
ALTER TABLE works ADD COLUMN IF NOT EXISTS is_repost        BOOLEAN     NOT NULL DEFAULT FALSE;
ALTER TABLE works ADD COLUMN IF NOT EXISTS repost_source_id UUID        REFERENCES works(id) ON DELETE SET NULL;
ALTER TABLE works ADD COLUMN IF NOT EXISTS is_sensitive      BOOLEAN     NOT NULL DEFAULT FALSE;
ALTER TABLE works ADD COLUMN IF NOT EXISTS subscriber_only   BOOLEAN     NOT NULL DEFAULT FALSE;
ALTER TABLE works ADD COLUMN IF NOT EXISTS comment_gating    TEXT        NOT NULL DEFAULT 'open';
ALTER TABLE works ADD COLUMN IF NOT EXISTS scheduled_at      TIMESTAMPTZ;

-- Taxonomy
ALTER TABLE works ADD COLUMN IF NOT EXISTS tags             TEXT[]      NOT NULL DEFAULT '{}';
ALTER TABLE works ADD COLUMN IF NOT EXISTS content_type     TEXT        NOT NULL DEFAULT 'text';

-- Poll fields (text[] to match posts.poll_options, JSONB vote tallies stored separately in poll_votes)
ALTER TABLE works ADD COLUMN IF NOT EXISTS poll_options     TEXT[];
ALTER TABLE works ADD COLUMN IF NOT EXISTS poll_ends_at     TIMESTAMPTZ;

-- Voice
ALTER TABLE works ADD COLUMN IF NOT EXISTS voice_url            TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS voice_duration_secs  REAL;

-- Video (HLS)
ALTER TABLE works ADD COLUMN IF NOT EXISTS video_master_url     TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS video_poster_url     TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS video_duration_secs  REAL;
ALTER TABLE works ADD COLUMN IF NOT EXISTS video_width          INTEGER;
ALTER TABLE works ADD COLUMN IF NOT EXISTS video_height         INTEGER;

-- React With Video: layout the reaction is arranged in at view time
-- (presenter|pip|split|card_over|media_only|green_screen). Empty for non-RWV works.
ALTER TABLE works ADD COLUMN IF NOT EXISTS react_layout         TEXT;

-- Lineage (immediate parent attribution for thread/reply display)
ALTER TABLE works ADD COLUMN IF NOT EXISTS lineage_pial   TEXT;
ALTER TABLE works ADD COLUMN IF NOT EXISTS lineage_handle TEXT;

-- Legacy link (original posts.id for migrated content — idempotent lookup)
ALTER TABLE works ADD COLUMN IF NOT EXISTS legacy_post_id UUID;

-- Denormalised counters (updated by triggers or batch; avoids re-counting every page load)
ALTER TABLE works ADD COLUMN IF NOT EXISTS dislike_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE works ADD COLUMN IF NOT EXISTS quote_count   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE works ADD COLUMN IF NOT EXISTS reply_count   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE works ADD COLUMN IF NOT EXISTS view_count    INTEGER NOT NULL DEFAULT 0;

-- Scan / moderation state
ALTER TABLE works ADD COLUMN IF NOT EXISTS scan_state  TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE works ADD COLUMN IF NOT EXISTS score_band  TEXT NOT NULL DEFAULT 'steady';

-- Two-file video architecture: clean URL for platform playback, watermarked URL for download/theft protection.
-- video_master_url     = clean HLS (no burn) — what the in-app player loads.
-- video_watermarked_url = moving-watermark HLS — what escapes the platform carries.
ALTER TABLE works ADD COLUMN IF NOT EXISTS video_watermarked_url TEXT;

-- Additional indexes for the new columns
CREATE INDEX IF NOT EXISTS idx_works_scheduled      ON works(scheduled_at)      WHERE scheduled_at IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_works_legacy_post    ON works(legacy_post_id)    WHERE legacy_post_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_works_repost_source  ON works(repost_source_id)  WHERE repost_source_id IS NOT NULL;

-- editions: immutable edit history per work
CREATE TABLE IF NOT EXISTS editions (
  id             UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
  work_id        UUID    NOT NULL REFERENCES works(id) ON DELETE CASCADE,
  cid            TEXT    UNIQUE NOT NULL,
  body           TEXT    NOT NULL DEFAULT '',
  edition_number INTEGER NOT NULL DEFAULT 1,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_editions_work_id ON editions(work_id);

-- work_citations: reply-to and quote relationships
CREATE TABLE IF NOT EXISTS work_citations (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  work_id       UUID NOT NULL REFERENCES works(id) ON DELETE CASCADE,
  target_id     UUID NOT NULL,
  citation_type TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_work_citations_work_id   ON work_citations(work_id);
CREATE INDEX IF NOT EXISTS idx_work_citations_target_id ON work_citations(target_id);

-- work_reactions: likes, reposts, bookmarks
CREATE TABLE IF NOT EXISTS work_reactions (
  work_id       UUID NOT NULL REFERENCES works(id) ON DELETE CASCADE,
  reactor_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  reaction_type TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (work_id, reactor_id, reaction_type)
);
CREATE INDEX IF NOT EXISTS idx_work_reactions_work_id ON work_reactions(work_id);
CREATE INDEX IF NOT EXISTS idx_work_reactions_reactor ON work_reactions(reactor_id);

-- work_poll_votes: one ballot per (work, voter). This is the ONLY source of truth
-- for poll tallies — counts are aggregated with COUNT(*) at read time, never
-- denormalised onto works, so there is exactly one write path (a row here) and
-- no counter to drift. The primary key enforces one vote per PIAL persona per
-- poll; the option index addresses works.poll_options by position.
--
-- Distinct from the legacy poll_votes table, which is foreign-keyed to posts(id)
-- and therefore cannot hold a ballot for a work. Polls live on works.
CREATE TABLE IF NOT EXISTS work_poll_votes (
  work_id    UUID NOT NULL REFERENCES works(id) ON DELETE CASCADE,
  voter_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  option_idx INT  NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (work_id, voter_id)
);
CREATE INDEX IF NOT EXISTS idx_work_poll_votes_tally ON work_poll_votes(work_id, option_idx);
CREATE INDEX IF NOT EXISTS idx_work_poll_votes_voter ON work_poll_votes(voter_id);

-- work_scores: Abraxas scoring signals (does NOT gate visibility)
CREATE TABLE IF NOT EXISTS work_scores (
  work_id    UUID             NOT NULL REFERENCES works(id) ON DELETE CASCADE,
  signal     TEXT             NOT NULL,
  score      DOUBLE PRECISION NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
  PRIMARY KEY (work_id, signal)
);

-- pial_signing_keys: Elohim-veni (f33d3r_security) is the durable authority; reads go there
-- (see handler.fetchSigningPubkey). The local table below is a write-through mirror only and
-- MUST NOT be dropped on boot — a DROP here wipes every key on restart and breaks work
-- signature verification (the signing-key lookup returns empty, so signed works are rejected).

-- pial_ecdh_keys: ECDH-P256 public keys for marketplace content decryption
-- Browser generates per-PIAL ECDH keypair; public key registered here.
-- Themis fetches this key to re-wrap CEKs after payment confirmation.
CREATE TABLE IF NOT EXISTS pial_ecdh_keys (
  pial_id        UUID        PRIMARY KEY,
  public_key_b64 TEXT        NOT NULL,
  registered_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- pial_signing_keys: ECDSA-P256 (SPKI) signing public keys for Malkuth/Themis signature
-- verification. The PIAL record (feed-engine) is authoritative; Elohim-veni's key service
-- is a best-effort mirror. Client generates the keypair; private key never leaves the device.
CREATE TABLE IF NOT EXISTS pial_signing_keys (
  pial_id        UUID        PRIMARY KEY,
  public_key_b64 TEXT        NOT NULL,
  algorithm      TEXT        NOT NULL DEFAULT 'ECDSA-P256',
  registered_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS upload_batches (
    batch_id    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pial_id     UUID,
    total_count INT         NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_upload_batches_user ON upload_batches(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS upload_batch_slots (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id      UUID        NOT NULL REFERENCES upload_batches(batch_id) ON DELETE CASCADE,
    tus_upload_id TEXT        NOT NULL,
    filename      TEXT        NOT NULL DEFAULT '',
    status        TEXT        NOT NULL DEFAULT 'queued',
    master_url    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_upload_batch_slots_batch ON upload_batch_slots(batch_id);
CREATE INDEX IF NOT EXISTS idx_upload_batch_slots_tus   ON upload_batch_slots(tus_upload_id);

ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS is_founding_creator BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS founding_creator_at TIMESTAMPTZ;
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS referral_code TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS creator_referrals (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    referrer_id    UUID        NOT NULL REFERENCES users(id),
    referred_id    UUID        NOT NULL REFERENCES users(id),
    referral_code  TEXT        NOT NULL,
    completed_at   TIMESTAMPTZ,
    reward_granted BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(referred_id)
);
CREATE INDEX IF NOT EXISTS idx_referrals_referrer ON creator_referrals(referrer_id);
CREATE INDEX IF NOT EXISTS idx_referrals_code     ON creator_referrals(referral_code);

-- ════════════════════════════════════════════════════════════════════════════
-- Gnosis messaging (rebuilt) — two render paths chosen at conversation creation:
--   'plain'  : server-readable plaintext, rendered server-side like every other FA
--              surface. Used when ANY participant is not 18+ verified.
--   'sealed' : E2EE. Server stores only ciphertext + per-recipient wrapped keys.
--              Used IFF every participant is 18+ verified (PIAL age_verified+is_adult).
-- mode is fixed at creation by the lowest-capability participant and IMMUTABLE:
-- there is NO setter — the CHECK plus the absence of any UPDATE is the structural
-- no-silent-downgrade guarantee.
-- ════════════════════════════════════════════════════════════════════════════
CREATE TABLE IF NOT EXISTS gnosis_conversations (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  mode        TEXT        NOT NULL CHECK (mode IN ('plain','sealed')),
  created_by  UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  is_group    BOOLEAN     NOT NULL DEFAULT FALSE,
  title       TEXT        NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Participants. Anchored on the account (users.id); pial_id is recorded so the
-- 18+ gate (and the no-downgrade check on member-add) can read it without a join.
CREATE TABLE IF NOT EXISTS gnosis_members (
  conversation_id UUID        NOT NULL REFERENCES gnosis_conversations(id) ON DELETE CASCADE,
  account_id      UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  pial_id         UUID        NOT NULL,
  joined_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_read_at    TIMESTAMPTZ,
  PRIMARY KEY (conversation_id, account_id)
);
CREATE INDEX IF NOT EXISTS idx_gnosis_members_account ON gnosis_members(account_id);

-- Messages. mode is denormalized from the conversation so a row self-describes.
-- plain  : body holds server-readable plaintext.
-- sealed : body stays '' ; body_ct_b64/body_nonce_b64 carry the envelope body and
--          per-recipient wrapped content keys live in gnosis_sealed_keys.
CREATE TABLE IF NOT EXISTS gnosis_messages (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  conversation_id UUID        NOT NULL REFERENCES gnosis_conversations(id) ON DELETE CASCADE,
  sender_account  UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  mode            TEXT        NOT NULL CHECK (mode IN ('plain','sealed')),
  body            TEXT        NOT NULL DEFAULT '',
  body_ct_b64     TEXT        NOT NULL DEFAULT '',
  body_nonce_b64  TEXT        NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_gnosis_messages_convo ON gnosis_messages(conversation_id, created_at);

-- Sealed mode only: the per-recipient wrapped content key. Keyed by recipient
-- ACCOUNT (not PIAL) — each account is its own sealed island; personas of the same
-- human do NOT share message keys.
CREATE TABLE IF NOT EXISTS gnosis_sealed_keys (
  message_id        UUID NOT NULL REFERENCES gnosis_messages(id) ON DELETE CASCADE,
  recipient_account UUID NOT NULL,
  eph_pub_b64       TEXT NOT NULL DEFAULT '',
  sealed_b64        TEXT NOT NULL DEFAULT '',
  sealed_nonce_b64  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (message_id, recipient_account)
);

-- Sealed-mode identity: each ACCOUNT's X25519 messaging keypair. Per-account (not
-- per-PIAL) so logging into one persona never exposes another's messages, and
-- co-members never learn a peer's human-level PIAL. The PUBLIC key is the directory
-- entry; the PRIVATE key is stored only as a blob wrapped under the account's
-- login-derived key (Argon2id over the password, salt = SHA-256(handle)) — the
-- server never holds it in the clear. Login alone unlocks it on any device; there
-- is NO messaging-specific recovery — the account's backup codes are the sole
-- recovery for everything. (v1: single fixed epoch.)
CREATE TABLE IF NOT EXISTS gnosis_identity (
  account_id       UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  pub_b64          TEXT        NOT NULL,
  wrapped_priv_b64 TEXT        NOT NULL DEFAULT '',
  wrap_nonce_b64   TEXT        NOT NULL DEFAULT '',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Auth fold: retire the projection columns ─────────────────────────────────
-- Identity/authz (verification, role, age) now lives on PIAL (pial_roots); these
-- columns are no longer read or written. Run scripts/fold-auth-to-pial.sql first so
-- any account verified only in these columns is reconciled onto PIAL before the drop.
ALTER TABLE user_profiles DROP COLUMN IF EXISTS is_verified;
ALTER TABLE user_roles    DROP COLUMN IF EXISTS is_age_verified;
ALTER TABLE user_roles    DROP COLUMN IF EXISTS is_adult;
ALTER TABLE user_roles    DROP COLUMN IF EXISTS is_minor;
ALTER TABLE user_roles    DROP COLUMN IF EXISTS role_type;
