//! 18 U.S.C. 2257 records.
//!
//! A record links one piece of 18+ content to the passing `purpose=creator`
//! submission whose encrypted ID copy is the performer's record for it. Verity
//! refuses a record for a PIAL that has no such submission: eligibility comes
//! from verity's evidence alone — never from a tier, and never from an admin
//! override in identity.
//!
//! Verity cannot see content being removed, so feed-engine reports it. The
//! latest removal across a submission's records starts the seven-year clock
//! the retention job applies to that submission's ID copy.

use std::sync::Arc;

use axum::{
    extract::{rejection::JsonRejection, rejection::PathRejection, Path, State},
    http::StatusCode,
    response::Json,
};
use chrono::{DateTime, Utc};
use serde_json::{json, Value};
use tracing::{info, warn};
use uuid::Uuid;

use crate::api::{bad_request, internal, unprocessable, ApiErr};
use crate::auth::Caller;
use crate::db::{self, action};
use crate::models::{ContentRemovedReq, Record2257Req};
use crate::AppState;

const MAX_CONTENT_ID_LEN: usize = 128;
const MAX_CLOCK_SKEW_SECS: i64 = 300;

type RecordRow = (
    Uuid,
    Uuid,
    String,
    String,
    DateTime<Utc>,
    Option<DateTime<Utc>>,
);

fn record_json(pial_id: Uuid, row: &RecordRow, already_existed: bool) -> Value {
    let (record_id, attempt_id, content_id, record_type, created_at, removed_at) = row;
    json!({
        "recorded":        true,
        "record_id":       record_id,
        "pial_id":         pial_id,
        "content_id":      content_id,
        "record_type":     record_type,
        "attempt_id":      attempt_id,
        "created_at":      created_at,
        "removed_at":      removed_at,
        "already_existed": already_existed,
    })
}

const SELECT_RECORD: &str = "SELECT r.id, s.attempt_id, r.content_id, r.record_type, r.created_at, r.removed_at
                               FROM records_2257 r
                               JOIN kyc_submissions s ON s.id = r.submission_id
                              WHERE r.pial_id = $1 AND r.content_id = $2";

// ── POST /v1/compliance/2257/record ───────────────────────────────────────────

pub async fn record(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    body: Result<Json<Record2257Req>, JsonRejection>,
) -> Result<(StatusCode, Json<Value>), ApiErr> {
    caller.require_service()?;
    let Json(req) = body.map_err(|e| bad_request(&e.body_text()))?;
    let pial_id = req.pial_id;
    let content_id = content_id(req.content_id)?;
    let record_type = req.record_type.trim().to_string();
    if !valid_record_type(&record_type) {
        return Err(unprocessable(
            "record_type must be lower-case letters, digits and underscores (1–64), starting with a letter",
        ));
    }

    let mut tx = state
        .pool
        .begin()
        .await
        .map_err(|e| internal("begin 2257 record transaction", pial_id, e))?;

    let existing: Option<RecordRow> = sqlx::query_as(SELECT_RECORD)
        .bind(pial_id)
        .bind(&content_id)
        .fetch_optional(&mut *tx)
        .await
        .map_err(|e| internal("read 2257 record", pial_id, e))?;
    if let Some(row) = existing {
        return Ok((StatusCode::OK, Json(record_json(pial_id, &row, true))));
    }

    // The ID copy row is share-locked until this transaction ends, so the
    // retention job cannot purge the copy this record is about to rest on.
    let eligible: Option<(Uuid, Uuid)> = sqlx::query_as(
        "SELECT s.id, s.attempt_id
           FROM kyc_submissions s
           JOIN kyc_id_copies c ON c.submission_id = s.id
          WHERE s.pial_id = $1 AND s.purpose = 'creator' AND s.decision = 'pass'
          ORDER BY s.created_at DESC
          LIMIT 1
          FOR SHARE OF c",
    )
    .bind(pial_id)
    .fetch_optional(&mut *tx)
    .await
    .map_err(|e| internal("read 2257 eligibility", pial_id, e))?;

    let Some((submission_id, attempt_id)) = eligible else {
        warn!(%pial_id, "2257 record refused: no passing creator submission with an ID copy");
        return Err((
            StatusCode::CONFLICT,
            Json(json!({ "recorded": false, "reason": "no_creator_verification" })),
        ));
    };

    let inserted: Option<(Uuid, DateTime<Utc>)> = sqlx::query_as(
        "INSERT INTO records_2257 (pial_id, content_id, record_type, submission_id, purpose, decision)
         VALUES ($1, $2, $3, $4, 'creator', 'pass')
         ON CONFLICT (pial_id, content_id) DO NOTHING
         RETURNING id, created_at",
    )
    .bind(pial_id)
    .bind(&content_id)
    .bind(&record_type)
    .bind(submission_id)
    .fetch_optional(&mut *tx)
    .await
    .map_err(|e| internal("insert 2257 record", pial_id, e))?;

    let Some((record_id, created_at)) = inserted else {
        // A concurrent call recorded the same content first; answer with it.
        drop(tx);
        let row: RecordRow = sqlx::query_as(SELECT_RECORD)
            .bind(pial_id)
            .bind(&content_id)
            .fetch_one(&state.pool)
            .await
            .map_err(|e| internal("re-read 2257 record", pial_id, e))?;
        return Ok((StatusCode::OK, Json(record_json(pial_id, &row, true))));
    };

    db::audit(
        &mut *tx,
        pial_id,
        action::RECORD_2257,
        Some(attempt_id),
        Some(submission_id),
        json!({ "record_id": record_id, "content_id": content_id, "record_type": record_type }),
    )
    .await
    .map_err(|e| internal("audit 2257_record", pial_id, e))?;

    tx.commit()
        .await
        .map_err(|e| internal("commit 2257 record", pial_id, e))?;

    info!(%pial_id, %record_id, "2257 record written");

    let row: RecordRow = (record_id, attempt_id, content_id, record_type, created_at, None);
    Ok((StatusCode::CREATED, Json(record_json(pial_id, &row, false))))
}

// ── GET /v1/compliance/2257/:pial_id ──────────────────────────────────────────

pub async fn get(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    path: Result<Path<Uuid>, PathRejection>,
) -> Result<Json<Value>, ApiErr> {
    caller.require_service()?;
    let Path(pial_id) = path.map_err(|e| bad_request(&e.body_text()))?;

    let submission: Option<(Uuid, DateTime<Utc>, String, String, DateTime<Utc>)> = sqlx::query_as(
        "SELECT s.attempt_id, s.created_at, s.doc_type, s.issuing_country, c.created_at
           FROM kyc_submissions s
           JOIN kyc_id_copies c ON c.submission_id = s.id
          WHERE s.pial_id = $1 AND s.purpose = 'creator' AND s.decision = 'pass'
          ORDER BY s.created_at DESC
          LIMIT 1",
    )
    .bind(pial_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| internal("read 2257 eligibility", pial_id, e))?;

    let records: Vec<(Uuid, Uuid, String, String, DateTime<Utc>, Option<DateTime<Utc>>)> =
        sqlx::query_as(
            "SELECT r.id, s.attempt_id, r.content_id, r.record_type, r.created_at, r.removed_at
               FROM records_2257 r
               JOIN kyc_submissions s ON s.id = r.submission_id
              WHERE r.pial_id = $1
              ORDER BY r.created_at DESC, r.id DESC",
        )
        .bind(pial_id)
        .fetch_all(&state.pool)
        .await
        .map_err(|e| internal("read 2257 records", pial_id, e))?;

    let records: Vec<Value> = records
        .into_iter()
        .map(|(record_id, attempt_id, content_id, record_type, created_at, removed_at)| {
            json!({
                "record_id":   record_id,
                "attempt_id":  attempt_id,
                "content_id":  content_id,
                "record_type": record_type,
                "created_at":  created_at,
                "removed_at":  removed_at,
            })
        })
        .collect();

    Ok(Json(json!({
        "pial_id":    pial_id,
        "eligible":   submission.is_some(),
        "submission": submission.map(|(attempt_id, verified_at, doc_type, country, copy_at)| json!({
            "attempt_id":        attempt_id,
            "verified_at":       verified_at,
            "doc_type":          doc_type,
            "issuing_country":   country,
            "id_copy_stored_at": copy_at,
        })),
        "records": records,
    })))
}

// ── POST /v1/compliance/2257/content-removed ──────────────────────────────────

pub async fn content_removed(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    body: Result<Json<ContentRemovedReq>, JsonRejection>,
) -> Result<Json<Value>, ApiErr> {
    caller.require_service()?;
    let Json(req) = body.map_err(|e| bad_request(&e.body_text()))?;
    let content_id = content_id(req.content_id)?;
    let removed_at = DateTime::parse_from_rfc3339(req.removed_at.trim())
        .map_err(|_| unprocessable("removed_at must be an RFC 3339 timestamp"))?
        .with_timezone(&Utc);
    if (removed_at - Utc::now()).num_seconds() > MAX_CLOCK_SKEW_SECS {
        return Err(unprocessable("removed_at is in the future"));
    }

    let mut tx = state
        .pool
        .begin()
        .await
        .map_err(|e| internal("begin content-removed transaction", &content_id, e))?;

    // The clock runs from the LAST removal, so a later report moves it forward
    // and an earlier, late-arriving one never moves it back.
    let updated: Vec<(Uuid, Uuid, Uuid, DateTime<Utc>)> = sqlx::query_as(
        "UPDATE records_2257
            SET removed_at = GREATEST(COALESCE(removed_at, $2), $2)
          WHERE content_id = $1
          RETURNING id, pial_id, submission_id, removed_at",
    )
    .bind(&content_id)
    .bind(removed_at)
    .fetch_all(&mut *tx)
    .await
    .map_err(|e| internal("mark 2257 content removed", &content_id, e))?;

    for (record_id, pial_id, submission_id, effective) in &updated {
        db::audit(
            &mut *tx,
            *pial_id,
            action::CONTENT_REMOVED_2257,
            None,
            Some(*submission_id),
            json!({
                "record_id":  record_id,
                "content_id": content_id,
                "reported":   removed_at,
                "removed_at": effective,
            }),
        )
        .await
        .map_err(|e| internal("audit 2257_content_removed", &content_id, e))?;
    }

    tx.commit()
        .await
        .map_err(|e| internal("commit content-removed", &content_id, e))?;

    if !updated.is_empty() {
        info!(content_id = %content_id, records = updated.len(), "2257 content removal recorded");
    }

    Ok(Json(json!({
        "content_id":      content_id,
        "records_updated": updated.len(),
    })))
}

fn content_id(raw: String) -> Result<String, ApiErr> {
    let id = raw.trim().to_string();
    if id.is_empty() || id.chars().count() > MAX_CONTENT_ID_LEN || id.chars().any(char::is_control)
    {
        return Err(unprocessable(&format!(
            "content_id must be 1–{MAX_CONTENT_ID_LEN} printable characters"
        )));
    }
    Ok(id)
}

fn valid_record_type(t: &str) -> bool {
    let b = t.as_bytes();
    !b.is_empty()
        && b.len() <= 64
        && b[0].is_ascii_lowercase()
        && b.iter().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || *c == b'_')
}
