use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── Brain registration ────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct BrainRegistrationRecord {
    pub brain_id: String,
    pub current_version: i32,
    pub port: i32,
    pub host: String,
    pub signal_types: Vec<String>,
    pub description: String,
    pub status: String,
    pub last_seen: DateTime<Utc>,
}

#[derive(Debug, Deserialize)]
pub struct RegisterBrainRequest {
    pub brain_id: String,
    pub schema_version: i32,
    pub port: i32,
    pub host: Option<String>,
    pub signal_types: Vec<String>,
    pub description: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct HeartbeatRequest {
    pub brain_id: String,
}

// ── Schema CRUD ───────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SchemaRecord {
    pub id: Uuid,
    pub brain_id: String,
    pub signal_type: String,
    pub version: i32,
    pub schema_json: serde_json::Value,
    pub description: String,
    pub registered_at: DateTime<Utc>,
}

#[derive(Debug, Deserialize)]
pub struct RegisterSchemaRequest {
    pub brain_id: String,
    pub signal_type: String,
    pub version: i32,
    pub schema_json: serde_json::Value,
    pub description: Option<String>,
    pub registered_by: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct RegisterSchemaResponse {
    pub schema_id: Uuid,
    pub brain_id: String,
    pub signal_type: String,
    pub version: i32,
    pub compatible: bool,
    pub violations: Vec<String>,
}

// ── Compatibility ─────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct CompatibilityResponse {
    pub brain_id: String,
    pub signal_type: String,
    pub from_version: Option<i32>,
    pub to_version: i32,
    pub compatible: bool,
    pub violations: Vec<String>,
}

#[derive(Debug, Deserialize)]
pub struct ValidateRequest {
    pub brain_id: String,
    pub signal_type: String,
    pub version: Option<i32>,
    pub payload: serde_json::Value,
}

#[derive(Debug, Serialize)]
pub struct ValidateResponse {
    pub valid: bool,
    pub violations: Vec<String>,
}

// ── Compatibility log ─────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CompatibilityLogEntry {
    pub id: Uuid,
    pub brain_id: String,
    pub signal_type: String,
    pub from_version: Option<i32>,
    pub to_version: i32,
    pub is_compatible: bool,
    pub violations: serde_json::Value,
    pub checked_at: DateTime<Utc>,
}

// ── Error response ────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct ErrorResponse {
    pub error: String,
    pub details: Option<String>,
}

impl ErrorResponse {
    pub fn new(msg: impl Into<String>) -> Self {
        Self {
            error: msg.into(),
            details: None,
        }
    }
    pub fn with_details(msg: impl Into<String>, details: impl Into<String>) -> Self {
        Self {
            error: msg.into(),
            details: Some(details.into()),
        }
    }
}
