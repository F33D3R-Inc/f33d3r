package db

import (
	"database/sql"
	"log"
)

const ddl = `
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
    is_verified     BOOLEAN     NOT NULL DEFAULT FALSE,
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

-- Sprint 0 / S0.6: HLS video assets. master.m3u8 + poster + duration. NULL when not a video.
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_master_url     TEXT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_poster_url     TEXT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_duration_secs  REAL;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_width          INT;
ALTER TABLE posts ADD COLUMN IF NOT EXISTS video_height         INT;

CREATE INDEX IF NOT EXISTS idx_posts_created    ON posts(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_posts_author     ON posts(author_id);
CREATE INDEX IF NOT EXISTS idx_posts_type       ON posts(content_type);
CREATE INDEX IF NOT EXISTS idx_posts_parent     ON posts(parent_id);
CREATE INDEX IF NOT EXISTS idx_posts_fts        ON posts USING gin(to_tsvector('english', body));

-- Add tier column to users if missing
ALTER TABLE users ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'free';

-- Add content_setting column to user_profiles if missing
ALTER TABLE user_profiles ADD COLUMN IF NOT EXISTS content_setting TEXT NOT NULL DEFAULT 'default';

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

CREATE TABLE IF NOT EXISTS follows (
    follower_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    following_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(follower_id, following_id)
);
CREATE INDEX IF NOT EXISTS idx_follows_following ON follows(following_id);

CREATE TABLE IF NOT EXISTS bookmarks (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    post_id    UUID NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(user_id, post_id)
);

CREATE TABLE IF NOT EXISTS post_likes (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    post_id    UUID NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(user_id, post_id)
);

CREATE TABLE IF NOT EXISTS post_reposts (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    post_id    UUID NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(user_id, post_id)
);

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
-- In production, Elohim Veni owns its own database and PIAL acts as the
-- cross-service identity spine via signed capability tokens.

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
CREATE TABLE IF NOT EXISTS user_roles (
    user_id          UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    role_type        TEXT        NOT NULL DEFAULT 'user',
    creator_type     TEXT        NOT NULL DEFAULT '',
    is_adult         BOOLEAN     NOT NULL DEFAULT FALSE,
    is_minor         BOOLEAN     NOT NULL DEFAULT FALSE,
    is_id_verified   BOOLEAN     NOT NULL DEFAULT FALSE,
    is_age_verified  BOOLEAN     NOT NULL DEFAULT FALSE,
    content_prefs    JSONB       NOT NULL DEFAULT '{}',
    onboard_step     INT         NOT NULL DEFAULT 0,
    onboard_done     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Backfill existing users with default role row
INSERT INTO user_roles (user_id, role_type, onboard_done)
SELECT id, 'user', TRUE FROM users
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
    ('clean_slate',     'Clean Slate',        '30 days with zero violations',                            '🌿',  'silver')
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
`

// MigrateUp creates all tables if they don't exist (safe to run on every boot).
func MigrateUp(database *sql.DB) {
	if _, err := database.Exec(ddl); err != nil {
		log.Fatalf("[db] migration failed: %v", err)
	}
	log.Println("[db] schema up to date")
}
