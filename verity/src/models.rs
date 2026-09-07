use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── 18 U.S.C. 2257 compliance types ──────────────────────────────────────────

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct Record2257 {
    pub id: Uuid,
    pub creator_pial_id: Uuid,
    pub kyc_submission_id: Option<Uuid>,
    pub age_band: String,
    pub document_type: String,
    pub document_hash: String,
    pub verification_date: DateTime<Utc>,
    pub record_keeper: String,
    pub record_location: String,
    pub statement_hash: String,
    pub is_active: bool,
    pub revoked_at: Option<DateTime<Utc>>,
    pub revocation_reason: Option<String>,
    pub created_at: DateTime<Utc>,
}

/// Request to create a 2257 record for a creator.
/// Called when a creator enables adult content.
#[derive(Debug, Deserialize)]
pub struct Create2257Req {
    /// PIAL UUID of the creator being registered
    pub pial_id: Uuid,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct CsamScan {
    pub id: Uuid,
    pub content_hash: String,
    pub uploader_pial: Uuid,
    pub scan_provider: String,
    pub result: String,
    pub match_count: i32,
    pub report_sent: bool,
    pub report_sent_at: Option<DateTime<Utc>>,
    pub media_url: Option<String>,
    pub media_type: Option<String>,
    pub scanned_at: DateTime<Utc>,
}

/// CSAM scan request — sent by Caeor after every upload.
#[derive(Debug, Deserialize)]
pub struct CsamScanReq {
    /// SHA-256 hex of the raw file bytes
    pub content_hash: String,
    /// PIAL UUID of the uploader
    pub uploader_pial: Uuid,
    pub media_url: Option<String>,
    pub media_type: Option<String>,
}

// ── Request types ─────────────────────────────────────────────────────────────

/// Core decision request — sent by Elohim Veni or Nantar on behalf of a user.
#[derive(Debug, Deserialize)]
pub struct DecisionReq {
    pub pial_id: Uuid,
    pub context: String, // onboarding | age_gate | creator_signup | payout_activation
    pub device_id: Option<String>,
    pub ip_address: Option<String>,
    pub session_token: Option<String>,
}

/// KYC submission from the frontend (document hash only — raw file handled client-side or by provider).
#[derive(Debug, Deserialize)]
pub struct KycSubmitReq {
    pub pial_id: Uuid,
    pub submission_type: String, // gov_id | liveness | phone | email | manual
    /// SHA-256 hash of the submitted document (document never sent to this service)
    pub document_hash: Option<String>,
    /// Age band extracted by the client or KYC provider
    pub age_band: Option<String>,
    /// Provider confidence score (0.0–1.0)
    pub confidence: Option<f32>,
    pub provider: Option<String>,
    pub metadata: Option<serde_json::Value>,
}

/// Admin override — manually set tier (moderator/compliance use only).
#[derive(Debug, Deserialize)]
pub struct AdminTierReq {
    pub pial_id: Uuid,
    pub tier: i32,
    pub reason: String,
    pub admin_id: String,
}

/// Risk signal push — from Zodacare or Elohim Veni.
#[derive(Debug, Deserialize)]
pub struct RiskSignalReq {
    pub pial_id: Uuid,
    pub device_count: Option<i32>,
    pub failed_kyc_delta: Option<i32>,
    pub ip_reputation: Option<f32>,
    pub velocity_score: Option<f32>,
    pub duplicate_detected: Option<bool>,
}

// ── Response types ────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct DecisionResp {
    pub pial_id: Uuid,
    pub status: String,    // approved | denied | pending_review
    pub age_band: String,  // 18+ | 21+ | underage | unknown
    pub creator_tier: i32, // 0–3
    pub payout_enabled: bool,
    pub nsfw_access: bool,
    pub risk_score: f32,
    pub required_actions: Vec<String>,
    pub decision_hash: String, // SHA-256(pial_id + status + age_band + tier + ts)
    pub evaluated_at: DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct KycSubmitResp {
    pub submission_id: Uuid,
    pub status: String,
    pub message: String,
}

#[derive(Debug, Serialize)]
pub struct ProfileResp {
    pub pial_id: Uuid,
    pub creator_tier: i32,
    pub age_band: String,
    pub payout_enabled: bool,
    pub nsfw_access: bool,
    pub risk_score: f32,
    pub kyc_submissions: i64,
    pub last_decision: Option<DateTime<Utc>>,
}

// ── DB row types ──────────────────────────────────────────────────────────────

#[derive(Debug, sqlx::FromRow)]
pub struct AttestationRow {
    pub id: Uuid,
    pub pial_id: Uuid,
    pub context: String,
    pub status: String,
    pub age_band: String,
    pub creator_tier: i32,
    pub payout_enabled: bool,
    pub nsfw_access: bool,
    pub risk_score: f32,
    pub required_actions: Vec<String>,
    pub decision_hash: String,
    pub created_at: DateTime<Utc>,
    pub expires_at: Option<DateTime<Utc>>,
    pub metadata: serde_json::Value,
}

#[derive(Debug, sqlx::FromRow)]
pub struct RiskProfile {
    pub pial_id: Uuid,
    pub risk_score: f32,
    pub device_count: i32,
    pub failed_kyc_attempts: i32,
    pub ip_reputation_score: f32,
    pub velocity_score: f32,
    pub duplicate_detected: bool,
    pub last_evaluated_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

// ── Decision engine constants ─────────────────────────────────────────────────

pub const RISK_SAFE: f32 = 0.3;
pub const RISK_REVIEW: f32 = 0.7;

pub const TIER_UNVERIFIED: i32 = 0;
pub const TIER_BASIC: i32 = 1;
pub const TIER_AGE: i32 = 2;
pub const TIER_CREATOR: i32 = 3;
