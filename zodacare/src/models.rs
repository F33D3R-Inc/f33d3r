use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── Content Report ────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct ContentReport {
    pub id:                Uuid,
    pub reporter_pial_id:  Option<Uuid>,
    pub reported_pial_id:  Option<Uuid>,
    pub content_id:        String,
    pub content_type:      String,
    pub reason:            String,
    pub detail:            Option<String>,
    pub status:            String, // pending | reviewing | actioned | dismissed
    pub resolved_by:       Option<String>,
    pub resolved_at:       Option<DateTime<Utc>>,
    pub escalated:         bool,
    pub created_at:        DateTime<Utc>,
}

// ── User Risk Profile ─────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct UserRiskProfile {
    pub pial_id:         Uuid,
    pub risk_score:      f64,      // 0.0 (clean) – 1.0 (high risk)
    pub report_count:    i32,
    pub violation_count: i32,
    pub last_action:     Option<String>,
    pub status:          String,   // active | warned | restricted | banned
    pub created_at:      DateTime<Utc>,
    pub updated_at:      DateTime<Utc>,
}

// ── Request DTOs ──────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct CreateReportRequest {
    pub reporter_pial_id: Option<Uuid>,
    pub reported_pial_id: Option<Uuid>,
    pub content_id:       String,
    pub content_type:     Option<String>,
    pub reason:           String,
    pub detail:           Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct ResolveReportRequest {
    pub status:     String, // actioned | dismissed
    pub resolved_by: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct ModerationActionRequest {
    pub action_type: String, // warn | restrict | ban | unban
    pub reason:      String,
    pub applied_by:  Option<String>,
    pub expires_at:  Option<DateTime<Utc>>,
}

#[derive(Debug, Deserialize)]
pub struct ReportsQuery {
    pub status: Option<String>,
    pub limit:  Option<i64>,
    pub offset: Option<i64>,
}

// ── Response DTOs ─────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct StatsResponse {
    pub service:          &'static str,
    pub total_reports:    i64,
    pub pending_reports:  i64,
    pub actioned_today:   i64,
    pub banned_users:     i64,
}

#[derive(Debug, Serialize)]
pub struct ActionResponse {
    pub pial_id:    Uuid,
    pub action:     String,
    pub status:     String,
    pub elohim_notified: bool,
}
