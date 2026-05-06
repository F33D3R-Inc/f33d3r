use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── PIAL State ────────────────────────────────────────────────────────────────

/// Full PIAL state row — available for callers that fetch the whole struct.
#[allow(dead_code)]
#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct PialState {
    pub pial_id:    Uuid,
    pub account_id: Uuid,
    pub status:     String, // ACTIVE | SUSPENDED | REVOKED
    pub tier:       String, // BASIC | VERIFIED | TRUSTED | PRIVILEGED
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

// ── Capability Grant ──────────────────────────────────────────────────────────

#[allow(dead_code)]
#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct CapabilityGrant {
    pub id:         Uuid,
    pub pial_id:    Uuid,
    pub capability: String, // e.g. "post", "message", "monetize", "adult_content", "node_relay"
    pub granted:    bool,
    pub granted_by: Option<Uuid>,
    pub reason:     Option<String>,
    pub expires_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

// ── Trust Score ───────────────────────────────────────────────────────────────

#[allow(dead_code)]
#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct TrustScore {
    pub pial_id:        Uuid,
    pub score:          f64, // 0.0 – 1.0
    pub anomaly_score:  f64, // 0.0 – 1.0; higher = more suspicious
    pub violation_count: i32,
    pub last_decision:  Option<String>,
    pub updated_at:     DateTime<Utc>,
}

// ── Decision Log ──────────────────────────────────────────────────────────────

#[allow(dead_code)]
#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct DecisionLog {
    pub id:                  Uuid,
    pub pial_id:             Uuid,
    pub action:              String,
    pub decision:            String, // ALLOW | ALLOW_RESTRICTED | QUARANTINE | DENY
    pub confidence:          f64,
    pub reason:              String,
    pub capability_required: Option<String>,
    pub content_risk:        f64,
    pub anomaly_score:       f64,
    pub created_at:          DateTime<Utc>,
}

// ── Request / Response DTOs ───────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct BootstrapRequest {
    /// The canonical PIAL UUID issued by feed-engine. Elohim Veni mirrors it — never generates its own.
    pub pial_id:    Uuid,
    pub account_id: Uuid,
    pub tier:       Option<String>,
}

#[derive(Debug, Serialize)]
pub struct BootstrapResponse {
    pub pial_id: Uuid,
    pub created: bool,
}

#[derive(Debug, Serialize)]
pub struct CapabilityMap {
    pub pial_id:      Uuid,
    pub capabilities: Vec<CapabilityEntry>,
}

#[derive(Debug, Serialize)]
pub struct CapabilityEntry {
    pub capability: String,
    pub granted:    bool,
    pub expires_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Deserialize)]
pub struct CapabilityUpdateRequest {
    pub capability: String,
    pub grant:      bool,
    pub reason:     Option<String>,
    pub granted_by: Option<Uuid>,
    pub expires_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Serialize)]
pub struct CapabilityUpdateResponse {
    pub pial_id:    Uuid,
    pub capability: String,
    pub granted:    bool,
}

#[derive(Debug, Serialize)]
pub struct TrustResponse {
    pub pial_id:         Uuid,
    pub score:           f64,
    pub anomaly_score:   f64,
    pub violation_count: i32,
    pub tier:            String,
    pub last_decision:   Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct DecisionRequest {
    pub pial_id: Uuid,
    pub action:  String,
    pub context: DecisionContext,
}

#[derive(Debug, Deserialize)]
pub struct DecisionContext {
    pub content_risk:   f64,
    pub anomaly_score:  f64,
}

#[derive(Debug, Serialize)]
pub struct DecisionResponse {
    pub decision:            String,
    pub confidence:          f64,
    pub reason:              String,
    pub capability_required: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct StatsResponse {
    pub service:          &'static str,
    pub total_pials:      i64,
    pub total_decisions:  i64,
    pub decisions_today:  i64,
    pub deny_rate_today:  f64,
}

#[derive(Debug, Deserialize)]
pub struct UpdateStatusRequest {
    pub status:     String, // ACTIVE | SUSPENDED | REVOKED
    pub reason:     Option<String>,
    pub updated_by: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct UpdateStatusResponse {
    pub pial_id: Uuid,
    pub status:  String,
}

#[derive(Debug, Deserialize)]
pub struct BanRequest {
    pub reason:     String,
    pub applied_by: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct BulkCapabilityUpdate {
    pub pial_id:      Uuid,
    pub capabilities: Vec<CapabilityUpdateRequest>,
}

#[derive(Debug, Serialize)]
pub struct BulkCapabilityResponse {
    pub pial_id:  Uuid,
    pub updated:  usize,
}

#[derive(Debug, Deserialize)]
pub struct DecisionLogQuery {
    pub pial_id: Option<Uuid>,
    pub limit:   Option<i64>,
    pub offset:  Option<i64>,
}

#[derive(Debug, Serialize)]
pub struct DecisionLogEntry {
    pub id:                  Uuid,
    pub pial_id:             Uuid,
    pub action:              String,
    pub decision:            String,
    pub confidence:          f64,
    pub reason:              String,
    pub capability_required: Option<String>,
    pub content_risk:        f64,
    pub anomaly_score:       f64,
    pub created_at:          DateTime<Utc>,
}
