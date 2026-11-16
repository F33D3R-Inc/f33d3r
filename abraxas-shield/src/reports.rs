//! Reports: what people tell the platform about content and accounts.
//! Absorbed from zodacare, reshaped as cases.
//!
//! One open CASE per (subject, kind), however many people file it; each
//! person's filing is kept. `ownership_claim` — "this is mine" — opens
//! straight at admin_review with the claimant's own matching assets attached.
//!
//! ```text
//!   POST /v1/reports  {reporter_pial, subject:{kind, id}, kind, reason, detail?}
//!        → {report_id, stage, status}
//!   GET  /v1/reports?status=open|resolved|dismissed&limit=
//!        → {reports:[{report_id, subject, kind, reason, detail, status, stage,
//!                     reporter_pials, report_count, matched_assets, created_at}]}
//!   GET  /v1/reports/{id}
//!   POST /v1/reports/{id}/resolve  {actor_pial, status: resolved|dismissed, note?,
//!                                   action?: none|ban, ban_pial?}
//!   POST /v1/reports/{id}/escalate {actor_pial, note?}
//!   404 no such report · 409 already closed
//! ```
//!
//! A resolution that bans hands the ban to elohim-veni (enforcement.rs).
//! Subject kinds: work (id = work uuid), vision (<pial>.<seq>), asset (asset
//! id), stream (stream id), account (the account holder's PIAL).

use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::Json;
use serde::Deserialize;
use serde_json::{json, Value};
use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

use crate::api::{ApiError, ApiResult, AppState};
use crate::review::{self, Item, NewItem, Stage};

#[derive(Debug, Deserialize)]
pub struct SubjectRef {
    pub kind: String,
    pub id: String,
}

fn validate_subject(s: &SubjectRef) -> ApiResult<()> {
    let ok = match s.kind.as_str() {
        "work" | "asset" | "account" => Uuid::parse_str(&s.id).is_ok(),
        "vision" => s
            .id
            .rsplit_once('.')
            .map(|(p, n)| Uuid::parse_str(p).is_ok() && n.parse::<i64>().map(|n| n > 0).unwrap_or(false))
            .unwrap_or(false),
        "stream" => crate::media::check_rel(&s.id).is_ok() && !s.id.contains('/'),
        _ => return Err(ApiError::bad_request("subject.kind must be work, vision, asset, stream or account")),
    };
    if !ok {
        return Err(ApiError::bad_request(format!("subject id {:?} is not a valid {} id", s.id, s.kind)));
    }
    Ok(())
}

/// The PIAL a subject belongs to, when Abraxas knows it.
async fn owner_of(tx: &mut Transaction<'_, Postgres>, kind: &str, id: &str) -> sqlx::Result<Option<Uuid>> {
    review::pial_of(tx, kind, id).await
}

/// The assets a subject consists of.
async fn subject_assets(tx: &mut Transaction<'_, Postgres>, kind: &str, id: &str) -> sqlx::Result<Vec<Uuid>> {
    match kind {
        "asset" => Ok(Uuid::parse_str(id).ok().into_iter().collect()),
        "work" | "vision" => {
            sqlx::query_scalar("SELECT asset_id FROM subject_assets WHERE subject_kind = $1 AND subject_id = $2")
                .bind(kind)
                .bind(id)
                .fetch_all(&mut **tx)
                .await
        }
        _ => Ok(Vec::new()),
    }
}

/// A claimant's own assets that the index already found inside the subject.
async fn claim_matches(tx: &mut Transaction<'_, Postgres>, kind: &str, id: &str, claimant: Uuid) -> sqlx::Result<Vec<Value>> {
    let assets = subject_assets(tx, kind, id).await?;
    if assets.is_empty() {
        return Ok(Vec::new());
    }
    let rows: Vec<(Uuid, Option<Uuid>, Option<Uuid>, String, f64, Option<Uuid>)> = sqlx::query_as(
        "SELECT id, original_asset, original_pial, kind, confidence, new_asset FROM ownership_matches
          WHERE new_asset = ANY($1) AND original_pial = $2 AND state <> 'rejected'
          ORDER BY confidence DESC",
    )
    .bind(&assets)
    .bind(claimant)
    .fetch_all(&mut **tx)
    .await?;
    Ok(rows
        .into_iter()
        .map(|(mid, oa, op, k, c, na)| json!({
            "asset_id": oa, "pial": op, "kind": k, "confidence": c, "new_asset": na, "match_id": mid,
        }))
        .collect())
}

#[derive(Debug, Deserialize)]
pub struct CreateRequest {
    pub reporter_pial: Option<Uuid>,
    pub subject: SubjectRef,
    pub kind: String,
    pub reason: String,
    #[serde(default)]
    pub detail: Option<String>,
}

pub async fn create(State(st): State<AppState>, Json(req): Json<CreateRequest>) -> ApiResult<(StatusCode, Json<Value>)> {
    validate_subject(&req.subject)?;
    let kind = req.kind.trim();
    if kind.len() < 2 || kind.len() > 32 || !kind.starts_with(|c: char| c.is_ascii_lowercase())
        || !kind.chars().all(|c| c.is_ascii_lowercase() || c == '_')
    {
        return Err(ApiError::bad_request("kind must be a lowercase word ([a-z_], 2-32)"));
    }
    if req.reason.trim().is_empty() {
        return Err(ApiError::bad_request("reason is required"));
    }
    let claim = kind == "ownership_claim";
    if claim && req.reporter_pial.is_none() {
        return Err(ApiError::bad_request("an ownership claim names its claimant (reporter_pial)"));
    }
    let (sk, sid) = (req.subject.kind.as_str(), req.subject.id.as_str());

    let mut tx = st.pool.begin().await?;
    let reported = owner_of(&mut tx, sk, sid).await?;
    let stage = if claim { "admin_review" } else { "queue" };
    let (report_id, created): (Uuid, bool) = sqlx::query_as(
        "INSERT INTO reports (subject_kind, subject_id, kind, reason, detail, reported_pial, stage)
         VALUES ($1, $2, $3, $4, $5, $6, $7)
         ON CONFLICT (subject_kind, subject_id, kind) WHERE status = 'open'
         DO UPDATE SET updated_at = NOW()
         RETURNING id, (xmax = 0)",
    )
    .bind(sk)
    .bind(sid)
    .bind(kind)
    .bind(req.reason.trim())
    .bind(&req.detail)
    .bind(reported)
    .bind(stage)
    .fetch_one(&mut *tx)
    .await?;
    sqlx::query(
        "INSERT INTO report_filings (report_id, reporter_pial, reason, detail) VALUES ($1, $2, $3, $4)
         ON CONFLICT (report_id, reporter_pial) WHERE reporter_pial IS NOT NULL DO NOTHING",
    )
    .bind(report_id)
    .bind(req.reporter_pial)
    .bind(req.reason.trim())
    .bind(&req.detail)
    .execute(&mut *tx)
    .await?;

    if claim {
        let claimant = req.reporter_pial.expect("checked above");
        let found = claim_matches(&mut tx, sk, sid, claimant).await?;
        // Union with what earlier claimants brought, by asset.
        let existing: Value = sqlx::query_scalar("SELECT matched_assets FROM reports WHERE id = $1")
            .bind(report_id)
            .fetch_one(&mut *tx)
            .await?;
        let mut all: Vec<Value> = serde_json::from_value(existing).unwrap_or_default();
        for f in found {
            if !all.iter().any(|e| e.get("match_id") == f.get("match_id")) {
                all.push(f);
            }
        }
        sqlx::query("UPDATE reports SET matched_assets = $2 WHERE id = $1")
            .bind(report_id)
            .bind(json!(all))
            .execute(&mut *tx)
            .await?;
        review::open(&mut tx, NewItem {
            item_kind: "ownership_claim",
            subject_kind: sk,
            subject_id: sid,
            ownership_match: None,
            banned_id: None,
            category: None,
            report_id: Some(report_id),
            dedup_key: format!("claim:{report_id}"),
            stage: Stage::AdminReview,
            reason: &format!("ownership claim: {}", req.reason.trim()),
            evidence: json!({ "claimants": [claimant], "matched_assets": all }),
        })
        .await?;
    }
    tx.commit().await?;
    let stage: String = sqlx::query_scalar("SELECT stage FROM reports WHERE id = $1").bind(report_id).fetch_one(&st.pool).await?;
    let status = if created { StatusCode::CREATED } else { StatusCode::OK };
    Ok((status, Json(json!({ "report_id": report_id, "stage": stage, "status": "open", "created": created }))))
}

#[derive(sqlx::FromRow)]
struct Row {
    id: Uuid,
    subject_kind: String,
    subject_id: String,
    kind: String,
    reason: String,
    detail: Option<String>,
    status: String,
    stage: String,
    reported_pial: Option<Uuid>,
    matched_assets: Value,
    action: Option<String>,
    resolved_by: Option<String>,
    resolved_note: Option<String>,
    resolved_at: Option<chrono::DateTime<chrono::Utc>>,
    created_at: chrono::DateTime<chrono::Utc>,
    reporter_pials: Vec<Uuid>,
    report_count: i64,
}

const SELECT: &str = "SELECT r.id, r.subject_kind, r.subject_id, r.kind, r.reason, r.detail, r.status, r.stage,
        r.reported_pial, r.matched_assets, r.action, r.resolved_by, r.resolved_note, r.resolved_at, r.created_at,
        COALESCE(ARRAY(SELECT f.reporter_pial FROM report_filings f
                        WHERE f.report_id = r.id AND f.reporter_pial IS NOT NULL ORDER BY f.filed_at), '{}') AS reporter_pials,
        (SELECT COUNT(*) FROM report_filings f WHERE f.report_id = r.id) AS report_count
   FROM reports r";

fn row_json(r: &Row) -> Value {
    json!({
        "report_id": r.id,
        "subject": { "kind": r.subject_kind, "id": r.subject_id },
        "kind": r.kind, "reason": r.reason, "detail": r.detail,
        "status": r.status, "stage": r.stage,
        "reported_pial": r.reported_pial,
        "reporter_pials": r.reporter_pials, "report_count": r.report_count,
        "matched_assets": r.matched_assets,
        "action": r.action, "resolved_by": r.resolved_by, "resolved_note": r.resolved_note, "resolved_at": r.resolved_at,
        "created_at": r.created_at,
    })
}

#[derive(Debug, Deserialize)]
pub struct ListQuery {
    #[serde(default)]
    pub status: Option<String>,
    #[serde(default)]
    pub stage: Option<String>,
    #[serde(default)]
    pub limit: Option<i64>,
    #[serde(default)]
    pub offset: Option<i64>,
}

pub async fn list(State(st): State<AppState>, Query(q): Query<ListQuery>) -> ApiResult<Json<Value>> {
    let status = q.status.filter(|s| !s.is_empty()).unwrap_or_else(|| "open".into());
    if !matches!(status.as_str(), "open" | "resolved" | "dismissed") {
        return Err(ApiError::bad_request("status must be open, resolved or dismissed"));
    }
    let order = if status == "open" { "r.created_at ASC" } else { "r.resolved_at DESC" };
    let rows: Vec<Row> = sqlx::query_as::<_, Row>(&format!(
        "{SELECT} WHERE r.status = $1 AND ($2::text IS NULL OR r.stage = $2) ORDER BY {order} LIMIT $3 OFFSET $4"
    ))
    .bind(&status)
    .bind(&q.stage)
    .bind(q.limit.unwrap_or(50).clamp(1, 200))
    .bind(q.offset.unwrap_or(0).max(0))
    .fetch_all(&st.pool)
    .await?;
    let reports: Vec<Value> = rows.iter().map(row_json).collect();
    Ok(Json(json!({ "reports": reports, "count": reports.len() })))
}

async fn load(pool: &PgPool, id: Uuid) -> sqlx::Result<Option<Row>> {
    sqlx::query_as::<_, Row>(&format!("{SELECT} WHERE r.id = $1")).bind(id).fetch_optional(pool).await
}

pub async fn get(State(st): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let r = load(&st.pool, id).await?.ok_or_else(|| ApiError::not_found("no such report"))?;
    Ok(Json(row_json(&r)))
}

/// Closes an open report. False when it was not open.
pub async fn close_report(tx: &mut Transaction<'_, Postgres>, id: Uuid, status: &str, by: &str, note: Option<&str>) -> sqlx::Result<bool> {
    let n = sqlx::query(
        "UPDATE reports SET status = $2, resolved_by = $3, resolved_note = $4, resolved_at = NOW(), updated_at = NOW(),
                action = COALESCE(action, 'none')
          WHERE id = $1 AND status = 'open'",
    )
    .bind(id)
    .bind(status)
    .bind(by)
    .bind(note)
    .execute(&mut **tx)
    .await?
    .rows_affected();
    Ok(n == 1)
}

#[derive(Debug, Deserialize)]
pub struct ResolveRequest {
    pub actor_pial: Uuid,
    pub status: String,
    #[serde(default)]
    pub note: Option<String>,
    /// `ban` hands a ban to elohim-veni in the same transaction.
    #[serde(default)]
    pub action: Option<String>,
    /// Whom to ban; defaults to the subject's owner.
    #[serde(default)]
    pub ban_pial: Option<Uuid>,
}

pub async fn resolve(State(st): State<AppState>, Path(id): Path<Uuid>, Json(req): Json<ResolveRequest>) -> ApiResult<Json<Value>> {
    if !matches!(req.status.as_str(), "resolved" | "dismissed") {
        return Err(ApiError::bad_request("status must be resolved or dismissed"));
    }
    let ban = match req.action.as_deref().unwrap_or("none") {
        "none" => false,
        "ban" => true,
        _ => return Err(ApiError::bad_request("action must be none or ban")),
    };
    if ban && req.status != "resolved" {
        return Err(ApiError::bad_request("a dismissed report bans nobody"));
    }
    let r = load(&st.pool, id).await?.ok_or_else(|| ApiError::not_found("no such report"))?;
    if r.status != "open" {
        return Err(ApiError::conflict(format!("report is already {}", r.status)));
    }
    let by = req.actor_pial.to_string();
    let mut tx = st.pool.begin().await?;
    if !close_report(&mut tx, id, &req.status, &by, req.note.as_deref()).await? {
        return Err(ApiError::conflict("report is already closed"));
    }
    if ban {
        let target = req
            .ban_pial
            .or(r.reported_pial)
            .ok_or_else(|| ApiError::conflict("nobody to ban: the subject has no known owner; pass ban_pial"))?;
        crate::enforcement::enqueue_ban(&mut tx, target, req.note.as_deref().unwrap_or(&r.reason), &by, &format!("report:{id}")).await?;
        sqlx::query("UPDATE reports SET action = 'ban' WHERE id = $1").bind(id).execute(&mut *tx).await?;
    }
    // A queue item opened for this report is closed with it: the decision
    // was taken on the report itself.
    sqlx::query(
        "UPDATE review_queue SET stage = 'decided', decision = 'withdrawn', decided_by = $2, decided_at = NOW(),
                note = 'report resolved directly'
          WHERE report_id = $1 AND stage <> 'decided'",
    )
    .bind(id)
    .bind(&by)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;
    let r = load(&st.pool, id).await?.ok_or_else(|| ApiError::not_found("no such report"))?;
    Ok(Json(row_json(&r)))
}

#[derive(Debug, Deserialize)]
pub struct EscalateRequest {
    pub actor_pial: Uuid,
    #[serde(default)]
    pub note: Option<String>,
}

pub async fn escalate(State(st): State<AppState>, Path(id): Path<Uuid>, Json(req): Json<EscalateRequest>) -> ApiResult<Json<Value>> {
    let r = load(&st.pool, id).await?.ok_or_else(|| ApiError::not_found("no such report"))?;
    if r.status != "open" {
        return Err(ApiError::conflict(format!("report is already {}", r.status)));
    }
    let mut tx = st.pool.begin().await?;
    sqlx::query("UPDATE reports SET stage = 'admin_review', escalated_at = NOW(), updated_at = NOW() WHERE id = $1 AND status = 'open'")
        .bind(id)
        .execute(&mut *tx)
        .await?;
    if r.kind != "ownership_claim" {
        review::open(&mut tx, NewItem {
            item_kind: "report",
            subject_kind: &r.subject_kind,
            subject_id: &r.subject_id,
            ownership_match: None,
            banned_id: None,
            category: None,
            report_id: Some(id),
            dedup_key: format!("report:{id}"),
            stage: Stage::AdminReview,
            reason: &format!("report ({}): {}", r.kind, r.reason),
            evidence: json!({
                "escalated_by": req.actor_pial, "note": req.note,
                "reporter_pials": r.reporter_pials, "report_count": r.report_count,
            }),
        })
        .await?;
    }
    tx.commit().await?;
    let r = load(&st.pool, id).await?.ok_or_else(|| ApiError::not_found("no such report"))?;
    Ok(Json(row_json(&r)))
}

/// An admin upholding an ownership claim: the claimant's asset becomes the
/// credited original of the subject's asset.
pub async fn credit_claim(
    tx: &mut Transaction<'_, Postgres>,
    item: &Item,
    report: Uuid,
    original_asset: Option<Uuid>,
    by: &str,
) -> ApiResult<Value> {
    let claimants: Vec<Uuid> = sqlx::query_scalar(
        "SELECT reporter_pial FROM report_filings WHERE report_id = $1 AND reporter_pial IS NOT NULL",
    )
    .bind(report)
    .fetch_all(&mut **tx)
    .await?;
    let matched: Value = sqlx::query_scalar("SELECT matched_assets FROM reports WHERE id = $1")
        .bind(report)
        .fetch_one(&mut **tx)
        .await?;
    let first = matched.as_array().and_then(|a| a.first()).cloned();
    let pick = |k: &str| first.as_ref().and_then(|m| m.get(k)).and_then(|v| v.as_str()).and_then(|s| Uuid::parse_str(s).ok());
    let original = original_asset
        .or_else(|| pick("asset_id"))
        .ok_or_else(|| ApiError::conflict("no asset of the claimant matches; pass original_asset"))?;
    let new_asset = match item.subject_kind.as_str() {
        "asset" => Uuid::parse_str(&item.subject_id).ok(),
        _ => pick("new_asset").or(subject_assets(tx, &item.subject_kind, &item.subject_id).await?.first().copied()),
    }
    .ok_or_else(|| ApiError::conflict("the reported subject carries no asset to credit"))?;

    let orig: Option<(Uuid, Option<Uuid>)> =
        sqlx::query_as("SELECT id, uploader_pial FROM fingerprints WHERE asset_id = $1")
            .bind(original)
            .fetch_optional(&mut **tx)
            .await?;
    let (orig_fp, orig_pial) = orig.ok_or_else(|| ApiError::conflict("the claimed original has never been checked"))?;
    if !orig_pial.map(|p| claimants.contains(&p)).unwrap_or(false) {
        return Err(ApiError::conflict("the claimed original does not belong to a claimant"));
    }
    let new: Option<(Uuid, Option<Uuid>)> = sqlx::query_as("SELECT id, uploader_pial FROM fingerprints WHERE asset_id = $1")
        .bind(new_asset)
        .fetch_optional(&mut **tx)
        .await?;
    let (new_fp, new_pial) = new.ok_or_else(|| ApiError::conflict("the reported asset has never been checked"))?;
    if new_fp == orig_fp {
        return Err(ApiError::conflict("the claimed original is the reported asset itself"));
    }
    let (mid, kind, confidence): (Uuid, String, f64) = sqlx::query_as(
        "INSERT INTO ownership_matches
           (new_fingerprint, new_asset, new_pial, original_fingerprint, original_asset, original_pial,
            kind, layer, confidence, offsets, state, decided_by, decided_at)
         VALUES ($1, $2, $3, $4, $5, $6, 'derivative', 'claim', 1.0, $7, 'credited', $8, NOW())
         ON CONFLICT (new_fingerprint, original_fingerprint)
         DO UPDATE SET state = 'credited', decided_by = EXCLUDED.decided_by, decided_at = NOW()
         RETURNING id, kind, confidence",
    )
    .bind(new_fp)
    .bind(new_asset)
    .bind(new_pial)
    .bind(orig_fp)
    .bind(original)
    .bind(orig_pial)
    .bind(json!({ "claim_report": report }))
    .bind(by)
    .fetch_one(&mut **tx)
    .await?;
    Ok(json!({
        "match_id": mid, "original_asset": original, "original_pial": orig_pial, "original_fingerprint": orig_fp,
        "new_asset": new_asset, "kind": kind, "layer": "claim", "confidence": confidence, "state": "credited",
        "report_id": report,
    }))
}

pub async fn stats(State(st): State<AppState>) -> ApiResult<Json<Value>> {
    let (open, queue, admin, resolved_today, bans): (i64, i64, i64, i64, i64) = sqlx::query_as(
        "SELECT COUNT(*) FILTER (WHERE status = 'open'),
                COUNT(*) FILTER (WHERE status = 'open' AND stage = 'queue'),
                COUNT(*) FILTER (WHERE status = 'open' AND stage = 'admin_review'),
                COUNT(*) FILTER (WHERE status <> 'open' AND resolved_at > NOW() - INTERVAL '24 hours'),
                COUNT(*) FILTER (WHERE action = 'ban')
           FROM reports",
    )
    .fetch_one(&st.pool)
    .await?;
    Ok(Json(json!({
        "service": "abraxas-shield",
        "reports_open": open, "reports_queue": queue, "reports_admin_review": admin,
        "reports_closed_24h": resolved_today, "reports_banned": bans,
    })))
}
