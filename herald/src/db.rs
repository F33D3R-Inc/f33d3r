use anyhow::Result;
use sqlx::PgPool;

use crate::models::{NotificationPreferences, PushSubscription, UpdatePreferencesRequest};

const SCHEMA: &str = r#"
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ── Push Subscriptions ────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS push_subscriptions (
    subscription_id UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_shard_id   VARCHAR(64) NOT NULL,
    device_id       VARCHAR(128) NOT NULL,
    endpoint        TEXT        NOT NULL,
    p256dh_key      TEXT        NOT NULL,
    auth_key        TEXT        NOT NULL,
    user_agent      VARCHAR(256),
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    last_used_at    TIMESTAMPTZ DEFAULT NOW(),
    is_active       BOOL        DEFAULT true,
    UNIQUE (pial_shard_id, device_id)
);
CREATE INDEX IF NOT EXISTS idx_push_subs_pial ON push_subscriptions(pial_shard_id, is_active);

-- ── Notification Preferences ──────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS notification_preferences (
    pial_shard_id        VARCHAR(64) PRIMARY KEY,
    push_enabled         BOOL    DEFAULT true,
    messages_enabled     BOOL    DEFAULT true,
    likes_enabled        BOOL    DEFAULT true,
    reposts_enabled      BOOL    DEFAULT true,
    replies_enabled      BOOL    DEFAULT true,
    follows_enabled      BOOL    DEFAULT true,
    achievements_enabled BOOL    DEFAULT true,
    mentions_enabled     BOOL    DEFAULT true,
    quiet_hours_enabled  BOOL    DEFAULT false,
    quiet_hours_start    SMALLINT DEFAULT 22,
    quiet_hours_end      SMALLINT DEFAULT 8,
    timezone             VARCHAR(50) DEFAULT 'UTC'
);
-- Frequencies (Auralis) notifications: mic granted, co-host, invite.
ALTER TABLE notification_preferences ADD COLUMN IF NOT EXISTS frequencies_enabled BOOL DEFAULT true;

-- ── Notification Log ──────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS notification_log (
    log_id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_shard_id     VARCHAR(64) NOT NULL,
    notification_type VARCHAR(50) NOT NULL,
    title             TEXT        NOT NULL,
    body              TEXT        NOT NULL,
    icon_url          TEXT,
    action_url        TEXT,
    status            VARCHAR(20) DEFAULT 'pending',
    created_at        TIMESTAMPTZ DEFAULT NOW(),
    delivered_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_notif_log_pial   ON notification_log(pial_shard_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_notif_log_status ON notification_log(status, created_at DESC);
"#;

// ── Manhattan naming plane ────────────────────────────────────────────────────
//
// Herald's durable handoff to Manhattan, applied by the same boot-time
// migration mechanism as SCHEMA above.
//
// # Why herald needs one at all
//
// Herald does not own identity — elohim-veni does (manhattan/migrations/
// 0002_authority_map.sql). Every row in this database names a person it may not
// define. A push subscription pointing at an identifier nobody can vouch for
// delivers a private notification to whoever holds that identifier next, so the
// name must reach the naming plane, and reach it durably.
//
// A registration sent fire-and-forget is a registration that a restart, a
// timeout or a rolling deploy silently loses. So the handoff is transactional:
// these rows are written by triggers on the tables they describe, inside the
// same transaction as the row that caused them. Either a push subscription
// exists AND its identity registration is queued, or neither happened. A
// background drain (src/manhattan_outbox.rs) then delivers them in order,
// retrying with backoff, and marks them delivered.
//
// The triggers are the reason this cannot rot: registering becomes a property
// of the table rather than a step some future caller must remember.
//
// Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
// does not know them and must not learn them, or the two planes acquire a
// second shared identifier and we are back where we started.
const MANHATTAN_SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL CHECK (op IN ('node', 'name', 'edge', 'revoke_name')),
    payload         JSONB       NOT NULL,
    -- dedup_key makes enqueue idempotent, so a replayed insert or a backfill
    -- cannot queue the same registration twice.
    dedup_key       TEXT        NOT NULL UNIQUE,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ
);

-- The drain's only query: undelivered, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_pending
    ON manhattan_outbox(next_attempt_at, id) WHERE delivered_at IS NULL;

-- ── Quarantine ───────────────────────────────────────────────────────────────
-- Head-of-line blocking is correct while a row can still land: later rows depend
-- on earlier ones, and running ahead would write edges pointing at nodes that do
-- not exist. It stops being correct the moment a row cannot land at all — then
-- it is not ordering the queue, it is ending it. One unfixable row used to stop
-- this brain's entire naming-plane output indefinitely, with a log line as the
-- only signal and a hand-written migration as the only cure.
--
-- A quarantined row is neither dropped nor delivered. It keeps its payload, its
-- error and its id; the drain simply stops claiming it, so everything queued
-- behind it moves again. It represents a naming-plane write that did NOT happen,
-- so it is reported as an incident on every tick until a person clears
-- blocked_at — loud by construction rather than by anyone remembering to look.
--
--   what is stuck:  SELECT id, op, blocked_at, blocked_reason, payload
--                     FROM manhattan_outbox
--                    WHERE blocked_at IS NOT NULL AND delivered_at IS NULL
--                    ORDER BY id;
--   put one back:   UPDATE manhattan_outbox
--                      SET blocked_at = NULL, blocked_reason = NULL,
--                          attempts = 0, next_attempt_at = NOW()
--                    WHERE id = $1;
--
-- Returning a row to the queue re-enters it at its original id, so the ordering
-- the drain depends on survives the round trip.
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_at     TIMESTAMPTZ;
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_reason TEXT;

-- The drain's query is now "undelivered, not quarantined, due, oldest first", so
-- it gets its own partial index. The one above stays: the backlog counters still
-- ask for everything undelivered, quarantined rows included, because a write
-- that did not happen must not disappear from the depth a health check reports.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_deliverable
    ON manhattan_outbox(next_attempt_at, id)
 WHERE delivered_at IS NULL AND blocked_at IS NULL;

-- What is quarantined, read on every drain tick.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_blocked
    ON manhattan_outbox(id) WHERE blocked_at IS NOT NULL AND delivered_at IS NULL;

-- ── enqueue helper ───────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $fn$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$fn$ LANGUAGE plpgsql;

-- ── what herald's column actually holds ──────────────────────────────────────
--
-- Herald's three tables call their identity column pial_shard_id. That is a
-- second name for a concept that already has one, and it is not the same
-- concept the NEXUS layer means by "shard":
--
--   verity / lore  pial_shard_id = encode(sha256(pial_uuid || ':nexus-pial-shard-v1'))
--                  a blinded 64-hex alias of one PIAL, so the NEXUS tables never
--                  hold a raw PIAL.
--   herald         pial_shard_id = the raw PIAL UUID, sent verbatim by
--                  feed-engine (HeraldNotify and heraldSubscribeJSON both pass
--                  users.pial_id::text).
--
-- Both shapes fit VARCHAR(64), which is exactly why nothing has ever caught the
-- confusion. A blinded shard is irreversible and belongs to verity; herald
-- cannot resolve one, and must never register one as an identity, or it would
-- mint a second identity node for a person who already has one and then deliver
-- that person's notifications to it.
--
-- So the shape is decided here, at the table, and a value that is not a PIAL is
-- refused rather than stored.
--
-- The normalisation lives INSIDE the predicate on purpose. The trigger asks
-- about a trimmed, lowercased value and /health asks about the raw column; if
-- each normalised for itself, a value stored with stray whitespace would pass
-- the gate and then be reported as unresolvable by the health check that is
-- supposed to agree with it. One predicate, one answer, whoever asks.
CREATE OR REPLACE FUNCTION herald_is_pial(p_value TEXT) RETURNS BOOLEAN AS $fn$
    SELECT p_value IS NOT NULL
       AND btrim(p_value) ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$';
$fn$ LANGUAGE sql IMMUTABLE;

-- ── identity registration ────────────────────────────────────────────────────
-- A person is a PIAL, never a handle and never a derived shard. Herald does not
-- own the identity node; it registers the NAME, so that whichever brain meets a
-- person first the naming plane knows the person exists. Manhattan derives the
-- owner from the kind, so this write lands as an elohim-veni-owned node.
CREATE OR REPLACE FUNCTION herald_manhattan_register_identity() RETURNS TRIGGER AS $fn$
DECLARE
    v_pial TEXT;
BEGIN
    IF NEW.pial_shard_id IS NULL OR btrim(NEW.pial_shard_id) = '' THEN
        RAISE EXCEPTION 'herald: %.pial_shard_id is empty; a push target must name an identity', TG_TABLE_NAME;
    END IF;

    v_pial := lower(btrim(NEW.pial_shard_id));

    IF NOT herald_is_pial(v_pial) THEN
        RAISE EXCEPTION 'herald: %.pial_shard_id = % is not a PIAL UUID. Herald resolves pial:<uuid> through Manhattan; a NEXUS-derived shard is verity''s blinded alias and cannot be resolved here. Send the PIAL.', TG_TABLE_NAME, NEW.pial_shard_id;
    END IF;

    PERFORM manhattan_enqueue(
        'node',
        'node:pial:' || v_pial,
        jsonb_build_object(
            'kind',      'identity',
            'name',      'pial:' || v_pial,
            'namespace', 'pial'));

    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_push_subscription ON push_subscriptions;
CREATE TRIGGER trg_manhattan_register_push_subscription
    AFTER INSERT OR UPDATE OF pial_shard_id ON push_subscriptions
    FOR EACH ROW EXECUTE FUNCTION herald_manhattan_register_identity();

DROP TRIGGER IF EXISTS trg_manhattan_register_notification_prefs ON notification_preferences;
CREATE TRIGGER trg_manhattan_register_notification_prefs
    AFTER INSERT OR UPDATE OF pial_shard_id ON notification_preferences
    FOR EACH ROW EXECUTE FUNCTION herald_manhattan_register_identity();

DROP TRIGGER IF EXISTS trg_manhattan_register_notification_log ON notification_log;
CREATE TRIGGER trg_manhattan_register_notification_log
    AFTER INSERT OR UPDATE OF pial_shard_id ON notification_log
    FOR EACH ROW EXECUTE FUNCTION herald_manhattan_register_identity();

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Everything already here is enqueued once, so the naming plane starts complete
-- instead of only knowing about people who subscribe after this deploy.
-- ON CONFLICT DO NOTHING in the helper makes re-running this harmless.
--
-- Rows whose identifier is not a PIAL are NOT registered and NOT quietly
-- dropped: they are counted and reported, and /health keeps reporting them
-- until someone resolves them. Guessing what such a row meant is precisely how
-- a notification reaches the wrong person.
DO $do$
DECLARE
    r     RECORD;
    n_bad BIGINT := 0;
BEGIN
    FOR r IN
        SELECT lower(btrim(pial_shard_id)) AS pial FROM push_subscriptions
        UNION
        SELECT lower(btrim(pial_shard_id)) AS pial FROM notification_preferences
        UNION
        SELECT lower(btrim(pial_shard_id)) AS pial FROM notification_log
    LOOP
        IF herald_is_pial(r.pial) THEN
            PERFORM manhattan_enqueue(
                'node',
                'node:pial:' || r.pial,
                jsonb_build_object(
                    'kind',      'identity',
                    'name',      'pial:' || r.pial,
                    'namespace', 'pial'));
        ELSE
            n_bad := n_bad + 1;
        END IF;
    END LOOP;

    IF n_bad > 0 THEN
        RAISE WARNING 'herald: % distinct pial_shard_id value(s) are not PIAL UUIDs and were not registered with Manhattan. They cannot be resolved and must not receive notifications; /health reports the row count as unresolvable_targets.', n_bad;
    END IF;
END
$do$;
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    sqlx::raw_sql(MANHATTAN_SCHEMA).execute(pool).await?;
    Ok(())
}

// ── Subscriptions ─────────────────────────────────────────────────────────────

pub async fn upsert_subscription(
    pool: &PgPool,
    pial_shard_id: &str,
    device_id: &str,
    endpoint: &str,
    p256dh_key: &str,
    auth_key: &str,
    user_agent: Option<&str>,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO push_subscriptions \
           (pial_shard_id, device_id, endpoint, p256dh_key, auth_key, user_agent, is_active) \
         VALUES ($1, $2, $3, $4, $5, $6, true) \
         ON CONFLICT (pial_shard_id, device_id) DO UPDATE \
           SET endpoint     = EXCLUDED.endpoint, \
               p256dh_key   = EXCLUDED.p256dh_key, \
               auth_key     = EXCLUDED.auth_key, \
               user_agent   = EXCLUDED.user_agent, \
               last_used_at = NOW(), \
               is_active    = true",
    )
    .bind(pial_shard_id)
    .bind(device_id)
    .bind(endpoint)
    .bind(p256dh_key)
    .bind(auth_key)
    .bind(user_agent)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn deactivate_subscription(pool: &PgPool, device_id: &str) -> Result<bool> {
    let result =
        sqlx::query("UPDATE push_subscriptions SET is_active = false WHERE device_id = $1")
            .bind(device_id)
            .execute(pool)
            .await?;
    Ok(result.rows_affected() > 0)
}

pub async fn get_active_subscriptions(
    pool: &PgPool,
    pial_shard_id: &str,
) -> Result<Vec<PushSubscription>> {
    let rows = sqlx::query_as::<_, PushSubscription>(
        "SELECT subscription_id, pial_shard_id, device_id, endpoint, \
                p256dh_key, auth_key, user_agent, created_at, last_used_at, is_active \
         FROM push_subscriptions \
         WHERE pial_shard_id = $1 AND is_active = true \
         ORDER BY last_used_at DESC",
    )
    .bind(pial_shard_id)
    .fetch_all(pool)
    .await?;
    Ok(rows)
}

// ── Preferences ───────────────────────────────────────────────────────────────

pub async fn get_preferences(
    pool: &PgPool,
    pial_shard_id: &str,
) -> Result<NotificationPreferences> {
    let row = sqlx::query_as::<_, NotificationPreferences>(
        "SELECT pial_shard_id, push_enabled, messages_enabled, likes_enabled, \
                reposts_enabled, replies_enabled, follows_enabled, achievements_enabled, \
                mentions_enabled, frequencies_enabled, quiet_hours_enabled, quiet_hours_start, \
                quiet_hours_end, timezone \
         FROM notification_preferences WHERE pial_shard_id = $1"
    )
    .bind(pial_shard_id)
    .fetch_optional(pool)
    .await?;

    // Return defaults if no row exists yet.
    Ok(row.unwrap_or_else(|| NotificationPreferences {
        pial_shard_id: pial_shard_id.to_string(),
        push_enabled: true,
        messages_enabled: true,
        likes_enabled: true,
        reposts_enabled: true,
        replies_enabled: true,
        follows_enabled: true,
        achievements_enabled: true,
        mentions_enabled: true,
        frequencies_enabled: true,
        quiet_hours_enabled: false,
        quiet_hours_start: 22,
        quiet_hours_end: 8,
        timezone: "UTC".to_string(),
    }))
}

pub async fn upsert_preferences(
    pool: &PgPool,
    pial_shard_id: &str,
    req: &UpdatePreferencesRequest,
) -> Result<NotificationPreferences> {
    // Fetch current (or default) then apply partial update.
    let current = get_preferences(pool, pial_shard_id).await?;

    let push_enabled = req.push_enabled.unwrap_or(current.push_enabled);
    let messages_enabled = req.messages_enabled.unwrap_or(current.messages_enabled);
    let likes_enabled = req.likes_enabled.unwrap_or(current.likes_enabled);
    let reposts_enabled = req.reposts_enabled.unwrap_or(current.reposts_enabled);
    let replies_enabled = req.replies_enabled.unwrap_or(current.replies_enabled);
    let follows_enabled = req.follows_enabled.unwrap_or(current.follows_enabled);
    let achievements_enabled = req
        .achievements_enabled
        .unwrap_or(current.achievements_enabled);
    let mentions_enabled = req.mentions_enabled.unwrap_or(current.mentions_enabled);
    let frequencies_enabled = req
        .frequencies_enabled
        .unwrap_or(current.frequencies_enabled);
    let quiet_hours_enabled = req
        .quiet_hours_enabled
        .unwrap_or(current.quiet_hours_enabled);
    let quiet_hours_start = req.quiet_hours_start.unwrap_or(current.quiet_hours_start);
    let quiet_hours_end = req.quiet_hours_end.unwrap_or(current.quiet_hours_end);
    let timezone = req.timezone.clone().unwrap_or(current.timezone);

    sqlx::query(
        "INSERT INTO notification_preferences \
           (pial_shard_id, push_enabled, messages_enabled, likes_enabled, reposts_enabled, \
            replies_enabled, follows_enabled, achievements_enabled, mentions_enabled, \
            frequencies_enabled, quiet_hours_enabled, quiet_hours_start, quiet_hours_end, timezone) \
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) \
         ON CONFLICT (pial_shard_id) DO UPDATE \
           SET push_enabled         = EXCLUDED.push_enabled, \
               messages_enabled     = EXCLUDED.messages_enabled, \
               likes_enabled        = EXCLUDED.likes_enabled, \
               reposts_enabled      = EXCLUDED.reposts_enabled, \
               replies_enabled      = EXCLUDED.replies_enabled, \
               follows_enabled      = EXCLUDED.follows_enabled, \
               achievements_enabled = EXCLUDED.achievements_enabled, \
               mentions_enabled     = EXCLUDED.mentions_enabled, \
               frequencies_enabled  = EXCLUDED.frequencies_enabled, \
               quiet_hours_enabled  = EXCLUDED.quiet_hours_enabled, \
               quiet_hours_start    = EXCLUDED.quiet_hours_start, \
               quiet_hours_end      = EXCLUDED.quiet_hours_end, \
               timezone             = EXCLUDED.timezone",
    )
    .bind(pial_shard_id)
    .bind(push_enabled)
    .bind(messages_enabled)
    .bind(likes_enabled)
    .bind(reposts_enabled)
    .bind(replies_enabled)
    .bind(follows_enabled)
    .bind(achievements_enabled)
    .bind(mentions_enabled)
    .bind(frequencies_enabled)
    .bind(quiet_hours_enabled)
    .bind(quiet_hours_start)
    .bind(quiet_hours_end)
    .bind(&timezone)
    .execute(pool)
    .await?;

    Ok(NotificationPreferences {
        pial_shard_id: pial_shard_id.to_string(),
        push_enabled,
        messages_enabled,
        likes_enabled,
        reposts_enabled,
        replies_enabled,
        follows_enabled,
        achievements_enabled,
        mentions_enabled,
        frequencies_enabled,
        quiet_hours_enabled,
        quiet_hours_start,
        quiet_hours_end,
        timezone,
    })
}

// ── Notification log ──────────────────────────────────────────────────────────

pub async fn log_notification(
    pool: &PgPool,
    pial_shard_id: &str,
    notification_type: &str,
    title: &str,
    body: &str,
    icon_url: Option<&str>,
    action_url: Option<&str>,
) -> Result<uuid::Uuid> {
    let row: (uuid::Uuid,) = sqlx::query_as(
        "INSERT INTO notification_log \
           (pial_shard_id, notification_type, title, body, icon_url, action_url, status) \
         VALUES ($1, $2, $3, $4, $5, $6, 'pending') \
         RETURNING log_id",
    )
    .bind(pial_shard_id)
    .bind(notification_type)
    .bind(title)
    .bind(body)
    .bind(icon_url)
    .bind(action_url)
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}

pub async fn mark_delivered(pool: &PgPool, log_id: uuid::Uuid, success: bool) -> Result<()> {
    let status = if success { "delivered" } else { "failed" };
    sqlx::query("UPDATE notification_log SET status=$1, delivered_at=NOW() WHERE log_id=$2")
        .bind(status)
        .bind(log_id)
        .execute(pool)
        .await?;
    Ok(())
}

// ── Stats ─────────────────────────────────────────────────────────────────────

pub async fn count_active_subscriptions(pool: &PgPool) -> Result<i64> {
    let row: (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM push_subscriptions WHERE is_active = true")
            .fetch_one(pool)
            .await?;
    Ok(row.0)
}

pub async fn count_notifications_today(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM notification_log WHERE created_at >= NOW() - INTERVAL '24 hours'",
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}

// ── Manhattan outbox reads ────────────────────────────────────────────────────

/// How many registrations are queued and not yet delivered. A backlog that only
/// grows means the drain is blocked on its head row, which /health must say out
/// loud rather than leave to a log nobody is reading.
pub async fn manhattan_backlog_depth(pool: &PgPool) -> Result<i64> {
    let row: (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL")
            .fetch_one(pool)
            .await?;
    Ok(row.0)
}

/// Rows whose identity column holds something herald cannot resolve to a PIAL —
/// a NEXUS-derived shard, a handle, anything that is not the identity root.
/// These are never registered and never delivered to; they are reported until
/// someone fixes them at the source.
pub async fn count_unresolvable_targets(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT (SELECT COUNT(*) FROM push_subscriptions        WHERE NOT herald_is_pial(pial_shard_id)) \
              + (SELECT COUNT(*) FROM notification_preferences  WHERE NOT herald_is_pial(pial_shard_id))"
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}
