//! Durable participation: one row per visit.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use super::role::FrequencyRole;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, sqlx::Type)]
#[serde(rename_all = "snake_case")]
#[sqlx(type_name = "TEXT", rename_all = "snake_case")]
pub enum ParticipantState {
    Joined,
    Left,
    Removed,
}

#[derive(Debug, Clone, Serialize, sqlx::FromRow)]
pub struct Participant {
    pub id: i64,
    pub frequency_id: Uuid,
    pub pial: String,
    pub role: FrequencyRole,
    pub state: ParticipantState,
    pub muted: bool,
    pub joined_at: DateTime<Utc>,
    pub left_at: Option<DateTime<Utc>>,
    pub removed_at: Option<DateTime<Utc>>,
    pub remove_reason: Option<String>,
    pub removed_by: Option<String>,
}
