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
pub struct UserAchievement {
    pub earn_id: uuid::Uuid,
    pub pial_shard_id: String,
    pub achievement_id: String,
    pub earned_at: DateTime<Utc>,
    pub context_data: Option<serde_json::Value>,
}

#[derive(Debug, Serialize, Deserialize, sqlx::FromRow)]
pub struct UserXP {
    pub pial_shard_id: String,
    pub total_xp: i32,
    pub current_tier: String,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Serialize, Deserialize, sqlx::FromRow)]
pub struct UserProgress {
    pub pial_shard_id: String,
    pub achievement_id: String,
    pub current_value: i32,
    pub target_value: i32,
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

#[derive(Debug, Deserialize)]
pub struct LoreEvent {
    pub event_type: String,
    pub pial_shard_id: String,
    pub payload: Option<serde_json::Value>,
}
