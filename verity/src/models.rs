//! Wire types for verity's evidence and 2257 API. The shapes are the shared
//! KYC contract; unknown fields are refused so nothing outside the contract —
//! a document number above all — can be slipped into an evidence record.

use serde::Deserialize;
use uuid::Uuid;

// ── POST /v1/kyc/submissions ──────────────────────────────────────────────────

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Purpose {
    Age,
    Creator,
}

impl Purpose {
    pub fn as_str(self) -> &'static str {
        match self {
            Purpose::Age => "age",
            Purpose::Creator => "creator",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Decision {
    Pass,
    Fail,
}

impl Decision {
    pub fn as_str(self) -> &'static str {
        match self {
            Decision::Pass => "pass",
            Decision::Fail => "fail",
        }
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Scores {
    pub face_cosine: Option<f32>,
    pub pad: Option<f32>,
    pub liveness: Option<f32>,
}

// No `Debug` on the types that carry PII or images: a stray `{:?}` in a log
// line cannot print what does not implement it.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Consent {
    pub version: String,
    /// RFC 3339.
    pub accepted_at: String,
    pub ip: String,
    #[serde(default)]
    pub user_agent: String,
}

/// The ID images, base64. Present only for a passing `purpose=creator` attempt.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct IdCopy {
    pub front: String,
    #[serde(default)]
    pub back: String,
    pub portrait: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SubmissionReq {
    pub attempt_id: Uuid,
    pub pial_id: Uuid,
    pub purpose: Purpose,
    pub decision: Decision,
    #[serde(default)]
    pub reasons: Vec<String>,
    #[serde(default)]
    pub evidence_types: Vec<String>,
    #[serde(default)]
    pub doc_hmac: Option<String>,
    #[serde(default)]
    pub doc_type: Option<String>,
    #[serde(default)]
    pub issuing_country: Option<String>,
    #[serde(default)]
    pub issuing_region: Option<String>,
    #[serde(default)]
    pub legal_name: Option<String>,
    /// YYYY-MM-DD.
    #[serde(default)]
    pub dob: Option<String>,
    /// YYYY-MM-DD.
    #[serde(default)]
    pub doc_expiry: Option<String>,
    #[serde(default)]
    pub scores: Option<Scores>,
    pub consent: Consent,
    #[serde(default)]
    pub id_copy: Option<IdCopy>,
}

// ── POST /v1/compliance/2257/record ───────────────────────────────────────────

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Record2257Req {
    pub pial_id: Uuid,
    pub content_id: String,
    pub record_type: String,
}

// ── POST /v1/compliance/2257/content-removed ──────────────────────────────────

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ContentRemovedReq {
    pub content_id: String,
    /// RFC 3339.
    pub removed_at: String,
}
