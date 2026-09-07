use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

#[derive(Debug, Serialize, Deserialize, sqlx::FromRow)]
pub struct AchievementDef {
    pub achievement_id: String,
    pub category: String,
    pub name: String,
    pub description: String,
    pub icon_emoji: Option<String>,
    pub rarity: String,
    pub xp_reward: i32,
    pub is_secret: bool,
    pub is_repeatable: bool,
    pub unlock_condition: serde_json::Value,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Serialize, Deserialize, sqlx::FromRow)]
pub struct UserXP {
    pub identity: String,
    pub total_xp: i32,
    pub current_tier: String,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct UserAchievementView {
    pub achievement_id: String,
    pub category: String,
    pub name: String,
    pub description: String,
    pub icon_emoji: String,
    pub rarity: String,
    pub xp_reward: i32,
    pub is_secret: bool,
    pub earned_at: Option<DateTime<Utc>>,
    pub progress_current: Option<i32>,
    pub progress_target: Option<i32>,
}

#[derive(Debug, Serialize)]
pub struct XPStatus {
    pub total_xp: i32,
    pub current_tier: String,
    pub next_tier: Option<String>,
    pub xp_to_next: i32,
    pub xp_percent: i32,
}

/// An inbound event.
///
/// `identity` is a NAME to be resolved, not a key to be trusted: `pial:<uuid>`,
/// `shard:<64-hex>`, `handle:<handle>`, or the bare PIAL uuid that callers
/// written before the naming plane send. The `pial_shard_id` alias exists
/// because that is the field name the rest of the platform still writes — and
/// because what those callers actually put in it is a raw PIAL, not a shard.
/// Naming the field honestly and accepting the old spelling is how the second
/// vocabulary is retired without dropping an event on the floor.
#[derive(Debug, Deserialize)]
pub struct LoreEvent {
    pub event_type: String,
    #[serde(alias = "pial_shard_id", alias = "pial_id")]
    pub identity: String,
    pub payload: Option<serde_json::Value>,
}
