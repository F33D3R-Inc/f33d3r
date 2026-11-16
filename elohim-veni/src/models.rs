use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── Request / Response DTOs ───────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct BootstrapRequest {
    /// The PIAL this identity is filed under, and the only key it is filed
    /// under. Elohim Veni is the identity authority: bootstrapping is where a
    /// PIAL becomes an asserted identity — a node in the naming plane owned by
    /// this brain, which every other brain's PIAL column then refers to.
    pub pial_id: Uuid,
    pub tier: Option<String>,
}

// Note: callers that still send `account_id` are unaffected — it is another
// brain's row id, it is no longer stored, and an unknown field is ignored.

#[derive(Debug, Serialize)]
pub struct BootstrapResponse {
    pub pial_id: Uuid,
    pub created: bool,
}

#[derive(Debug, Deserialize)]
pub struct DecisionRequest {
    pub pial_id: Uuid,
    pub action: String,
    pub context: DecisionContext,
}

#[derive(Debug, Deserialize)]
pub struct DecisionContext {
    pub content_risk: f64,
    pub anomaly_score: f64,
}

#[derive(Debug, Serialize)]
pub struct DecisionResponse {
    pub decision: String,
    pub confidence: f64,
    pub reason: String,
    pub capability_required: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct StatsResponse {
    pub service: &'static str,
    pub total_pials: i64,
    pub total_decisions: i64,
    pub decisions_today: i64,
    pub deny_rate_today: f64,
}

#[derive(Debug, Deserialize)]
pub struct DecisionLogQuery {
    pub pial_id: Option<Uuid>,
    pub limit: Option<i64>,
    pub offset: Option<i64>,
}

#[derive(Debug, Serialize)]
pub struct DecisionLogEntry {
    pub id: Uuid,
    pub pial_id: Uuid,
    pub action: String,
    pub decision: String,
    pub confidence: f64,
    pub reason: String,
    pub capability_required: Option<String>,
    pub content_risk: f64,
    pub anomaly_score: f64,
    pub created_at: DateTime<Utc>,
}
