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

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
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
               is_active    = true"
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

pub async fn deactivate_subscription(
    pool: &PgPool,
    device_id: &str,
) -> Result<bool> {
    let result = sqlx::query(
        "UPDATE push_subscriptions SET is_active = false WHERE device_id = $1"
    )
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
         ORDER BY last_used_at DESC"
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
                mentions_enabled, quiet_hours_enabled, quiet_hours_start, quiet_hours_end, timezone \
         FROM notification_preferences WHERE pial_shard_id = $1"
    )
    .bind(pial_shard_id)
    .fetch_optional(pool)
    .await?;

    // Return defaults if no row exists yet.
    Ok(row.unwrap_or_else(|| NotificationPreferences {
        pial_shard_id:        pial_shard_id.to_string(),
        push_enabled:         true,
        messages_enabled:     true,
        likes_enabled:        true,
        reposts_enabled:      true,
        replies_enabled:      true,
        follows_enabled:      true,
        achievements_enabled: true,
        mentions_enabled:     true,
        quiet_hours_enabled:  false,
        quiet_hours_start:    22,
        quiet_hours_end:      8,
        timezone:             "UTC".to_string(),
    }))
}

pub async fn upsert_preferences(
    pool: &PgPool,
    pial_shard_id: &str,
    req: &UpdatePreferencesRequest,
) -> Result<NotificationPreferences> {
    // Fetch current (or default) then apply partial update.
    let current = get_preferences(pool, pial_shard_id).await?;

    let push_enabled         = req.push_enabled.unwrap_or(current.push_enabled);
    let messages_enabled     = req.messages_enabled.unwrap_or(current.messages_enabled);
    let likes_enabled        = req.likes_enabled.unwrap_or(current.likes_enabled);
    let reposts_enabled      = req.reposts_enabled.unwrap_or(current.reposts_enabled);
    let replies_enabled      = req.replies_enabled.unwrap_or(current.replies_enabled);
    let follows_enabled      = req.follows_enabled.unwrap_or(current.follows_enabled);
    let achievements_enabled = req.achievements_enabled.unwrap_or(current.achievements_enabled);
    let mentions_enabled     = req.mentions_enabled.unwrap_or(current.mentions_enabled);
    let quiet_hours_enabled  = req.quiet_hours_enabled.unwrap_or(current.quiet_hours_enabled);
    let quiet_hours_start    = req.quiet_hours_start.unwrap_or(current.quiet_hours_start);
    let quiet_hours_end      = req.quiet_hours_end.unwrap_or(current.quiet_hours_end);
    let timezone             = req.timezone.clone().unwrap_or(current.timezone);

    sqlx::query(
        "INSERT INTO notification_preferences \
           (pial_shard_id, push_enabled, messages_enabled, likes_enabled, reposts_enabled, \
            replies_enabled, follows_enabled, achievements_enabled, mentions_enabled, \
            quiet_hours_enabled, quiet_hours_start, quiet_hours_end, timezone) \
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) \
         ON CONFLICT (pial_shard_id) DO UPDATE \
           SET push_enabled         = EXCLUDED.push_enabled, \
               messages_enabled     = EXCLUDED.messages_enabled, \
               likes_enabled        = EXCLUDED.likes_enabled, \
               reposts_enabled      = EXCLUDED.reposts_enabled, \
               replies_enabled      = EXCLUDED.replies_enabled, \
               follows_enabled      = EXCLUDED.follows_enabled, \
               achievements_enabled = EXCLUDED.achievements_enabled, \
               mentions_enabled     = EXCLUDED.mentions_enabled, \
               quiet_hours_enabled  = EXCLUDED.quiet_hours_enabled, \
               quiet_hours_start    = EXCLUDED.quiet_hours_start, \
               quiet_hours_end      = EXCLUDED.quiet_hours_end, \
               timezone             = EXCLUDED.timezone"
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
         RETURNING log_id"
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
    sqlx::query(
        "UPDATE notification_log SET status=$1, delivered_at=NOW() WHERE log_id=$2"
    )
    .bind(status)
    .bind(log_id)
    .execute(pool)
    .await?;
    Ok(())
}

// ── Stats ─────────────────────────────────────────────────────────────────────

pub async fn count_active_subscriptions(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM push_subscriptions WHERE is_active = true"
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}

pub async fn count_notifications_today(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM notification_log WHERE created_at >= NOW() - INTERVAL '24 hours'"
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}
