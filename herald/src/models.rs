use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── Push Subscription ─────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct PushSubscription {
    pub subscription_id: Uuid,
    pub pial_shard_id: String,
    pub device_id: String,
    pub endpoint: String,
    pub p256dh_key: String,
    pub auth_key: String,
    pub user_agent: Option<String>,
    pub created_at: DateTime<Utc>,
    pub last_used_at: DateTime<Utc>,
    pub is_active: bool,
}

// ── Notification Preferences ──────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct NotificationPreferences {
    pub pial_shard_id: String,
    pub push_enabled: bool,
    pub messages_enabled: bool,
    pub likes_enabled: bool,
    pub reposts_enabled: bool,
    pub replies_enabled: bool,
    pub follows_enabled: bool,
    pub achievements_enabled: bool,
    pub mentions_enabled: bool,
    pub frequencies_enabled: bool,
    pub quiet_hours_enabled: bool,
    pub quiet_hours_start: i16,
    pub quiet_hours_end: i16,
    pub timezone: String,
}

// ── Notification Log ──────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct NotificationLog {
    pub log_id: Uuid,
    pub pial_shard_id: String,
    pub notification_type: String,
    pub title: String,
    pub body: String,
    pub icon_url: Option<String>,
    pub action_url: Option<String>,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub delivered_at: Option<DateTime<Utc>>,
}

// ── Request DTOs ──────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct SubscribeRequest {
    pub pial_shard_id: String,
    pub device_id: String,
    pub endpoint: String,
    pub p256dh_key: String,
    pub auth_key: String,
    pub user_agent: Option<String>,
}

// ── Notify request (POST /v1/notify) ─────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct NotifyRequest {
    pub pial_shard_id: String,
    pub notification_type: String, // like | reply | follow | mention | dm | achievement | tip
    pub title: String,
    pub body: String,
    pub icon_url: Option<String>,
    pub action_url: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct UpdatePreferencesRequest {
    pub push_enabled: Option<bool>,
    pub messages_enabled: Option<bool>,
    pub likes_enabled: Option<bool>,
    pub reposts_enabled: Option<bool>,
    pub replies_enabled: Option<bool>,
    pub follows_enabled: Option<bool>,
    pub achievements_enabled: Option<bool>,
    pub mentions_enabled: Option<bool>,
    pub frequencies_enabled: Option<bool>,
    pub quiet_hours_enabled: Option<bool>,
    pub quiet_hours_start: Option<i16>,
    pub quiet_hours_end: Option<i16>,
    pub timezone: Option<String>,
}
