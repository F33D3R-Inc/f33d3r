//! KYC evidence: one record per verification attempt.
//!
//! ekyc decides whether an attempt passes; feed-engine forwards the decision
//! and its evidence here; identity (elohim-veni) owns the resulting level.
//! Verity's part is to keep the evidence, refuse a document that already
//! proved a different PIAL, seal the 2257 ID copy, and audit every step — all
//! in one transaction, so an attempt is either fully on record or not at all.

use std::sync::Arc;

use axum::{
    extract::{rejection::JsonRejection, rejection::PathRejection, Path, State},
    http::StatusCode,
    response::Json,
};
use base64::Engine as _;
use chrono::{DateTime, Datelike, NaiveDate, Utc};
use serde_json::{json, Value};
use tracing::{error, info, warn};
use uuid::Uuid;

use crate::api::{bad_request, internal, unprocessable, ApiErr};
use crate::auth::Caller;
use crate::crypto::Sealed;
use crate::db::{self, action};
use crate::models::{Decision, Purpose, SubmissionReq};
use crate::AppState;

/// The reason recorded, and returned, when a document already passed for a
/// different PIAL.
const DOCUMENT_IN_USE: &str = "document_in_use";

/// Upper bound on one decoded ID image. A phone photo of a licence is well
/// under this; anything larger is not an ID capture.
const MAX_IMAGE_BYTES: usize = 10 * 1024 * 1024;

/// Request-body ceiling for the submissions route: three images at the cap,
/// base64-inflated, plus the JSON around them.
pub const SUBMISSION_BODY_LIMIT: usize = 48 * 1024 * 1024;

const MAX_LIST_ITEMS: usize = 64;
const MAX_LIST_ITEM_LEN: usize = 128;
const MAX_FIELD_LEN: usize = 64;
const MAX_NAME_LEN: usize = 256;
const MAX_USER_AGENT_LEN: usize = 1024;

/// How far in the future a client-reported timestamp may sit before it is
/// refused: clock skew between brains, not a licence to backdate.
const MAX_CLOCK_SKEW_SECS: i64 = 300;

/// A validated attempt, ready to be written.
struct Evidence {
    submission_id: Uuid,
    attempt_id: Uuid,
    pial_id: Uuid,
    purpose: Purpose,
    claimed: Decision,
    reasons: Vec<String>,
    evidence_types: Vec<String>,
    doc_hmac: Option<String>,
    doc_type: Option<String>,
    issuing_country: Option<String>,
    issuing_region: Option<String>,
    doc_expiry: Option<NaiveDate>,
    /// Present only when the attempt claims a pass; dropped for a fail.
    legal_name: Option<String>,
    dob: Option<NaiveDate>,
    face_cosine: Option<f32>,
    pad_score: Option<f32>,
    liveness_score: Option<f32>,
    consent_version: String,
    consent_accepted_at: DateTime<Utc>,
    consent_ip: String,
    consent_user_agent: String,
    /// Present only for a creator pass.
    id_copy: Option<SealedCopy>,
}

struct SealedCopy {
    key_id: String,
    front: Sealed,
    back: Option<Sealed>,
    portrait: Sealed,
}

// ── POST /v1/kyc/submissions ──────────────────────────────────────────────────

pub async fn submit(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    body: Result<Json<SubmissionReq>, JsonRejection>,
) -> Result<(StatusCode, Json<Value>), ApiErr> {
    caller.require_service()?;
    let Json(req) = body.map_err(|e| bad_request(&e.body_text()))?;

    let ev = validate(&state, req)?;
    let attempt_id = ev.attempt_id;
    let pial_id = ev.pial_id;

    let mut tx = state
        .pool
        .begin()
        .await
        .map_err(|e| internal("begin submission transaction", attempt_id, e))?;

    // One attempt is recorded once. The lock serialises concurrent deliveries
    // of the same attempt; the second one sees the first one's row and answers
    // exactly as the first did.
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
        .bind(format!("verity:kyc:attempt:{attempt_id}"))
        .execute(&mut *tx)
        .await
        .map_err(|e| internal("lock attempt", attempt_id, e))?;

    let existing: Option<(Uuid, String, String, Vec<String>)> = sqlx::query_as(
        "SELECT pial_id, purpose, decision, reasons FROM kyc_submissions WHERE attempt_id = $1",
    )
    .bind(attempt_id)
    .fetch_optional(&mut *tx)
    .await
    .map_err(|e| internal("read existing attempt", attempt_id, e))?;

    if let Some((rec_pial, rec_purpose, rec_decision, rec_reasons)) = existing {
        if rec_pial != pial_id || rec_purpose != ev.purpose.as_str() {
            warn!(%attempt_id, %pial_id, "refused: attempt_id already recorded for a different PIAL or purpose");
            return Err((
                StatusCode::CONFLICT,
                Json(json!({ "accepted": false, "reason": "attempt_id_conflict" })),
            ));
        }
        info!(%attempt_id, %pial_id, decision = %rec_decision, "kyc submission replayed");
        return Ok(outcome(
            rec_decision == "pass",
            rec_reasons.iter().any(|r| r == DOCUMENT_IN_USE),
        ));
    }

    // One passing document → one PIAL. Every submission naming this document
    // is serialised on it, so the check below and the binding written after it
    // cannot interleave with another PIAL's attempt.
    let mut in_use_by: Option<Uuid> = None;
    if let Some(hmac) = ev.doc_hmac.as_deref() {
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")
            .bind(format!("verity:kyc:doc:{hmac}"))
            .execute(&mut *tx)
            .await
            .map_err(|e| internal("lock document", attempt_id, e))?;

        let bound: Option<(Uuid, Uuid)> = sqlx::query_as(
            "SELECT pial_id, submission_id FROM kyc_document_bindings WHERE doc_hmac = $1",
        )
        .bind(hmac)
        .fetch_optional(&mut *tx)
        .await
        .map_err(|e| internal("read document binding", attempt_id, e))?;

        if let Some((owner, binding_submission)) = bound {
            if owner != pial_id {
                in_use_by = Some(binding_submission);
            }
        }
    }
    let in_use = in_use_by.is_some();

    let decision = if in_use { Decision::Fail } else { ev.claimed };
    let passed = decision == Decision::Pass;
    let mut reasons = ev.reasons.clone();
    if in_use && !reasons.iter().any(|r| r == DOCUMENT_IN_USE) {
        reasons.push(DOCUMENT_IN_USE.to_string());
    }

    sqlx::query(
        "INSERT INTO kyc_submissions
            (id, attempt_id, pial_id, purpose, decision, reasons, evidence_types,
             doc_hmac, doc_type, issuing_country, issuing_region, doc_expiry,
             legal_name, dob, face_cosine, pad_score, liveness_score,
             consent_version, consent_accepted_at, consent_ip, consent_user_agent)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)",
    )
    .bind(ev.submission_id)
    .bind(attempt_id)
    .bind(pial_id)
    .bind(ev.purpose.as_str())
    .bind(decision.as_str())
    .bind(&reasons)
    .bind(&ev.evidence_types)
    .bind(ev.doc_hmac.as_deref())
    .bind(ev.doc_type.as_deref())
    .bind(ev.issuing_country.as_deref())
    .bind(ev.issuing_region.as_deref())
    .bind(ev.doc_expiry)
    .bind(if passed { ev.legal_name.as_deref() } else { None })
    .bind(if passed { ev.dob } else { None })
    .bind(ev.face_cosine)
    .bind(ev.pad_score)
    .bind(ev.liveness_score)
    .bind(&ev.consent_version)
    .bind(ev.consent_accepted_at)
    .bind(&ev.consent_ip)
    .bind(&ev.consent_user_agent)
    .execute(&mut *tx)
    .await
    .map_err(|e| internal("insert submission", attempt_id, e))?;

    db::audit(
        &mut *tx,
        pial_id,
        action::RECEIVED,
        Some(attempt_id),
        Some(ev.submission_id),
        json!({
            "purpose":         ev.purpose.as_str(),
            "claimed":         ev.claimed.as_str(),
            "evidence_types":  ev.evidence_types,
            "consent_version": ev.consent_version,
        }),
    )
    .await
    .map_err(|e| internal("audit received", attempt_id, e))?;

    if let Some(binding_submission) = in_use_by {
        db::audit(
            &mut *tx,
            pial_id,
            action::DOCUMENT_IN_USE,
            Some(attempt_id),
            Some(ev.submission_id),
            json!({ "binding_submission_id": binding_submission }),
        )
        .await
        .map_err(|e| internal("audit document_in_use", attempt_id, e))?;
    }

    if passed {
        if let Some(hmac) = ev.doc_hmac.as_deref() {
            sqlx::query(
                "INSERT INTO kyc_document_bindings (doc_hmac, pial_id, submission_id, purpose, decision)
                 VALUES ($1, $2, $3, $4, 'pass')
                 ON CONFLICT (doc_hmac) DO NOTHING",
            )
            .bind(hmac)
            .bind(pial_id)
            .bind(ev.submission_id)
            .bind(ev.purpose.as_str())
            .execute(&mut *tx)
            .await
            .map_err(|e| internal("bind document", attempt_id, e))?;
        }

        if let Some(copy) = ev.id_copy.as_ref() {
            sqlx::query(
                "INSERT INTO kyc_id_copies
                    (submission_id, pial_id, purpose, decision, key_id,
                     front_nonce, front_ct, back_nonce, back_ct, portrait_nonce, portrait_ct)
                 VALUES ($1, $2, 'creator', 'pass', $3, $4, $5, $6, $7, $8, $9)",
            )
            .bind(ev.submission_id)
            .bind(pial_id)
            .bind(&copy.key_id)
            .bind(&copy.front.nonce)
            .bind(&copy.front.ciphertext)
            .bind(copy.back.as_ref().map(|b| &b.nonce))
            .bind(copy.back.as_ref().map(|b| &b.ciphertext))
            .bind(&copy.portrait.nonce)
            .bind(&copy.portrait.ciphertext)
            .execute(&mut *tx)
            .await
            .map_err(|e| internal("store id copy", attempt_id, e))?;

            db::audit(
                &mut *tx,
                pial_id,
                action::ID_COPY_STORED,
                Some(attempt_id),
                Some(ev.submission_id),
                json!({
                    "key_id": copy.key_id,
                    "parts":  if copy.back.is_some() {
                        json!(["front", "back", "portrait"])
                    } else {
                        json!(["front", "portrait"])
                    },
                }),
            )
            .await
            .map_err(|e| internal("audit id_copy_stored", attempt_id, e))?;
        }
    }

    db::audit(
        &mut *tx,
        pial_id,
        action::DECIDED,
        Some(attempt_id),
        Some(ev.submission_id),
        json!({ "decision": decision.as_str(), "reasons": reasons }),
    )
    .await
    .map_err(|e| internal("audit decided", attempt_id, e))?;

    tx.commit()
        .await
        .map_err(|e| internal("commit submission", attempt_id, e))?;

    info!(
        %attempt_id,
        %pial_id,
        purpose = ev.purpose.as_str(),
        decision = decision.as_str(),
        document_in_use = in_use,
        id_copy_stored = passed && ev.id_copy.is_some(),
        "kyc submission recorded"
    );

    Ok(outcome(passed, in_use))
}

/// The contract's three answers.
fn outcome(passed: bool, in_use: bool) -> (StatusCode, Json<Value>) {
    if in_use {
        (
            StatusCode::CONFLICT,
            Json(json!({ "accepted": false, "reason": DOCUMENT_IN_USE })),
        )
    } else if passed {
        (StatusCode::CREATED, Json(json!({ "accepted": true })))
    } else {
        (StatusCode::OK, Json(json!({ "accepted": false })))
    }
}

// ── GET /v1/kyc/:pial_id ──────────────────────────────────────────────────────

/// The latest attempt for a PIAL: what was decided, for what, on what evidence,
/// and when. No level — that is identity's to state.
pub async fn summary(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    path: Result<Path<Uuid>, PathRejection>,
) -> Result<Json<Value>, ApiErr> {
    caller.require_service()?;
    let Path(pial_id) = path.map_err(|e| bad_request(&e.body_text()))?;

    let row: Option<(Uuid, String, String, Vec<String>, DateTime<Utc>)> = sqlx::query_as(
        "SELECT attempt_id, decision, purpose, evidence_types, created_at
           FROM kyc_submissions
          WHERE pial_id = $1
          ORDER BY created_at DESC, id DESC
          LIMIT 1",
    )
    .bind(pial_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| internal("read kyc summary", pial_id, e))?;

    Ok(Json(match row {
        Some((attempt_id, decision, purpose, evidence_types, at)) => json!({
            "pial_id":        pial_id,
            "found":          true,
            "attempt_id":     attempt_id,
            "decision":       decision,
            "purpose":        purpose,
            "evidence_types": evidence_types,
            "at":             at,
        }),
        None => json!({
            "pial_id":        pial_id,
            "found":          false,
            "attempt_id":     Value::Null,
            "decision":       Value::Null,
            "purpose":        Value::Null,
            "evidence_types": [],
            "at":             Value::Null,
        }),
    }))
}

// ── Validation ────────────────────────────────────────────────────────────────

fn validate(state: &AppState, req: SubmissionReq) -> Result<Evidence, ApiErr> {
    let now = Utc::now();
    let claimed = req.decision;
    let purpose = req.purpose;

    let reasons = string_list("reasons", req.reasons)?;
    let evidence_types = string_list("evidence_types", req.evidence_types)?;

    let doc_hmac = match present(req.doc_hmac) {
        Some(h) => {
            let h = h.to_ascii_lowercase();
            if h.len() != 64 || !h.bytes().all(|b| b.is_ascii_hexdigit()) {
                return Err(unprocessable("doc_hmac must be 64 hex characters (HMAC-SHA256)"));
            }
            Some(h)
        }
        None => None,
    };
    let doc_type = match present(req.doc_type) {
        Some(t) if matches!(t.as_str(), "passport" | "id_card" | "drivers_license") => Some(t),
        Some(_) => {
            return Err(unprocessable(
                "doc_type must be passport, id_card or drivers_license",
            ))
        }
        None => None,
    };
    let issuing_country = match present(req.issuing_country) {
        Some(c) if (2..=3).contains(&c.len()) && c.bytes().all(|b| b.is_ascii_uppercase()) => {
            Some(c)
        }
        Some(_) => {
            return Err(unprocessable(
                "issuing_country must be an ISO country code (2–3 upper-case letters)",
            ))
        }
        None => None,
    };
    let issuing_region = bounded("issuing_region", present(req.issuing_region), MAX_FIELD_LEN)?;
    let doc_expiry = present(req.doc_expiry)
        .map(|d| date("doc_expiry", &d))
        .transpose()?;

    let (face_cosine, pad_score, liveness_score) = match req.scores {
        Some(s) => (
            finite("scores.face_cosine", s.face_cosine)?,
            finite("scores.pad", s.pad)?,
            finite("scores.liveness", s.liveness)?,
        ),
        None => (None, None, None),
    };

    let consent_version = present(Some(req.consent.version))
        .ok_or_else(|| unprocessable("consent.version is required"))?;
    let consent_version = bounded("consent.version", Some(consent_version), MAX_FIELD_LEN)?
        .unwrap_or_default();
    let consent_accepted_at = DateTime::parse_from_rfc3339(req.consent.accepted_at.trim())
        .map_err(|_| unprocessable("consent.accepted_at must be an RFC 3339 timestamp"))?
        .with_timezone(&Utc);
    if (consent_accepted_at - now).num_seconds() > MAX_CLOCK_SKEW_SECS {
        return Err(unprocessable("consent.accepted_at is in the future"));
    }
    let consent_ip = present(Some(req.consent.ip))
        .ok_or_else(|| unprocessable("consent.ip is required"))?;
    if consent_ip.parse::<std::net::IpAddr>().is_err() {
        return Err(unprocessable("consent.ip must be an IP address"));
    }
    let consent_user_agent = req.consent.user_agent.trim().to_string();
    if consent_user_agent.len() > MAX_USER_AGENT_LEN {
        return Err(unprocessable("consent.user_agent is too long"));
    }

    // Identity facts are stored only for a pass. For a fail they are not
    // validated or kept: the attempt did not establish them.
    let (legal_name, dob) = if claimed == Decision::Pass {
        let name = bounded("legal_name", present(req.legal_name), MAX_NAME_LEN)?
            .ok_or_else(|| unprocessable("a pass requires legal_name"))?;
        let dob = present(req.dob)
            .map(|d| date("dob", &d))
            .transpose()?
            .ok_or_else(|| unprocessable("a pass requires dob"))?;
        (Some(name), Some(dob))
    } else {
        (None, None)
    };

    let mut id_copy_images: Option<(Vec<u8>, Option<Vec<u8>>, Vec<u8>)> = None;

    if claimed == Decision::Pass {
        // A pass is re-checked against what the contract says a pass is. Verity
        // does not re-decide the attempt, but it will not file as a pass
        // evidence that contradicts one.
        if doc_hmac.is_none() || doc_type.is_none() || issuing_country.is_none() {
            return Err(unprocessable(
                "a pass requires doc_hmac, doc_type and issuing_country",
            ));
        }
        let expiry = doc_expiry.ok_or_else(|| unprocessable("a pass requires doc_expiry"))?;
        if expiry < now.date_naive() {
            return Err(unprocessable("a pass cannot rest on an expired document"));
        }
        let Some(dob) = dob else {
            return Err(unprocessable("a pass requires dob"));
        };
        if dob > now.date_naive() {
            return Err(unprocessable("dob is in the future"));
        }
        if age_on(dob, now.date_naive()) < 18 {
            return Err(unprocessable("a pass requires age 18 or over"));
        }
        if face_cosine.is_none() || pad_score.is_none() || liveness_score.is_none() {
            return Err(unprocessable(
                "a pass requires scores.face_cosine, scores.pad and scores.liveness",
            ));
        }
        if evidence_types.is_empty() {
            return Err(unprocessable("a pass requires evidence_types"));
        }

        match (purpose, req.id_copy) {
            (Purpose::Creator, Some(copy)) => {
                let front = image("id_copy.front", &copy.front)?
                    .ok_or_else(|| unprocessable("id_copy.front is required"))?;
                let back = image("id_copy.back", &copy.back)?;
                let portrait = image("id_copy.portrait", &copy.portrait)?
                    .ok_or_else(|| unprocessable("id_copy.portrait is required"))?;
                id_copy_images = Some((front, back, portrait));
            }
            (Purpose::Creator, None) => {
                return Err(unprocessable(
                    "a creator pass requires id_copy (18 U.S.C. 2257)",
                ))
            }
            (Purpose::Age, Some(_)) => {
                return Err(unprocessable(
                    "an age-only pass stores no images; id_copy must be absent",
                ))
            }
            (Purpose::Age, None) => {}
        }
    } else if req.id_copy.is_some() {
        return Err(unprocessable("a fail stores no images; id_copy must be absent"));
    }

    let submission_id = Uuid::new_v4();
    let id_copy = match id_copy_images {
        Some((front, back, portrait)) => {
            let key = &state.records_key;
            let seal = |role: &str, bytes: &[u8]| {
                key.seal(submission_id, role, bytes).map_err(|e| {
                    error!(attempt_id = %req.attempt_id, role, error = %e, "id copy encryption failed");
                    crate::api::internal_plain()
                })
            };
            Some(SealedCopy {
                key_id: key.key_id().to_string(),
                front: seal("front", &front)?,
                back: back.as_deref().map(|b| seal("back", b)).transpose()?,
                portrait: seal("portrait", &portrait)?,
            })
        }
        None => None,
    };

    Ok(Evidence {
        submission_id,
        attempt_id: req.attempt_id,
        pial_id: req.pial_id,
        purpose,
        claimed,
        reasons,
        evidence_types,
        doc_hmac,
        doc_type,
        issuing_country,
        issuing_region,
        doc_expiry,
        legal_name,
        dob,
        face_cosine,
        pad_score,
        liveness_score,
        consent_version,
        consent_accepted_at,
        consent_ip,
        consent_user_agent,
        id_copy,
    })
}

/// A trimmed value, with an empty string read as absent.
fn present(v: Option<String>) -> Option<String> {
    v.map(|s| s.trim().to_string()).filter(|s| !s.is_empty())
}

fn bounded(field: &str, v: Option<String>, max: usize) -> Result<Option<String>, ApiErr> {
    match v {
        Some(s) if s.chars().count() > max => {
            Err(unprocessable(&format!("{field} is longer than {max} characters")))
        }
        other => Ok(other),
    }
}

fn string_list(field: &str, items: Vec<String>) -> Result<Vec<String>, ApiErr> {
    if items.len() > MAX_LIST_ITEMS {
        return Err(unprocessable(&format!(
            "{field} has more than {MAX_LIST_ITEMS} entries"
        )));
    }
    items
        .into_iter()
        .map(|s| {
            let s = s.trim().to_string();
            if s.is_empty() || s.len() > MAX_LIST_ITEM_LEN {
                Err(unprocessable(&format!(
                    "{field} entries must be 1–{MAX_LIST_ITEM_LEN} characters"
                )))
            } else {
                Ok(s)
            }
        })
        .collect()
}

fn date(field: &str, s: &str) -> Result<NaiveDate, ApiErr> {
    NaiveDate::parse_from_str(s, "%Y-%m-%d")
        .map_err(|_| unprocessable(&format!("{field} must be YYYY-MM-DD")))
}

fn finite(field: &str, v: Option<f32>) -> Result<Option<f32>, ApiErr> {
    match v {
        Some(x) if !x.is_finite() => Err(unprocessable(&format!("{field} must be a finite number"))),
        other => Ok(other),
    }
}

/// Decodes one base64 ID image and checks it is an image. Empty means absent.
fn image(field: &str, b64: &str) -> Result<Option<Vec<u8>>, ApiErr> {
    let b64 = b64.trim();
    if b64.is_empty() {
        return Ok(None);
    }
    if b64.len() > MAX_IMAGE_BYTES / 3 * 4 + 4 {
        return Err(unprocessable(&format!("{field} is larger than {MAX_IMAGE_BYTES} bytes")));
    }
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(b64)
        .map_err(|_| unprocessable(&format!("{field} is not valid base64")))?;
    let is_jpeg = bytes.starts_with(&[0xFF, 0xD8, 0xFF]);
    let is_png = bytes.starts_with(&[0x89, b'P', b'N', b'G', 0x0D, 0x0A, 0x1A, 0x0A]);
    let is_webp = bytes.len() >= 12 && &bytes[0..4] == b"RIFF" && &bytes[8..12] == b"WEBP";
    if !(is_jpeg || is_png || is_webp) {
        return Err(unprocessable(&format!("{field} must be a JPEG, PNG or WebP image")));
    }
    Ok(Some(bytes))
}

/// Whole years between `dob` and `today`.
fn age_on(dob: NaiveDate, today: NaiveDate) -> i32 {
    let mut age = today.year() - dob.year();
    if (today.month(), today.day()) < (dob.month(), dob.day()) {
        age -= 1;
    }
    age
}
