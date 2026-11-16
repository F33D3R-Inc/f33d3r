//! Review: what no first pass can decide.
//!
//! Every uncertain case is a review_queue row at a stage someone owns:
//!
//!   ml_review     the worker's deep pass (every frame, OCR, the full
//!                 transcript, tiled nudity detection) plus an independent
//!                 re-measurement. If it settles the case, it decides it.
//!   admin_review  a person. Listed by GET /v1/review for the admin Facets,
//!                 decided by POST /v1/review/{id}/decide.
//!   decided       closed, with who decided and how.
//!
//! What the ML stage may decide, by item kind:
//!   content      anything the policy decides on the deep evidence; a case
//!                the deep evidence still leaves in the review band goes on.
//!   banned_near  only CONFIRM a block (independent chromaprint verification
//!                at the block cut). It never clears a near-ban: missing a
//!                ban costs more than a person's minute. csam never reaches
//!                this stage at all.
//!   ownership    only CONFIRM credit (independent chromaprint verification
//!                aligned on the landmark offset, at the duplicate cut). It
//!                never rejects: chromaprint is not pitch-invariant, so a low
//!                score proves nothing about a pitch-shifted derivative.
//!   unjudged     nothing: no machine could read the media. Opens at admin.

use std::collections::HashSet;
use std::sync::Arc;
use std::time::{Duration, Instant};

use axum::extract::{Path, Query, State};
use axum::Json;
use chrono::{DateTime, Utc};
use serde::Deserialize;
use serde_json::{json, Value};
use sqlx::{PgPool, Postgres, Transaction};
use tokio::sync::RwLock;
use uuid::Uuid;

use crate::api::{ApiError, ApiResult, AppState};
use crate::hashing::{chroma_similarity_at, decode_chromaprint, CHROMA_FRAME_SECONDS};
use crate::jobs::{self, JobError};
use crate::outbox::{self, Event, EventType};
use crate::verdicts::{self, NewVerdict, Verdict};

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Stage {
    MlReview,
    AdminReview,
}

impl Stage {
    fn as_str(self) -> &'static str {
        match self {
            Stage::MlReview => "ml_review",
            Stage::AdminReview => "admin_review",
        }
    }
}

pub struct NewItem<'a> {
    pub item_kind: &'a str,
    pub subject_kind: &'a str,
    pub subject_id: &'a str,
    pub ownership_match: Option<Uuid>,
    pub banned_id: Option<Uuid>,
    pub category: Option<&'a str>,
    pub report_id: Option<Uuid>,
    pub dedup_key: String,
    pub stage: Stage,
    pub reason: &'a str,
    pub evidence: Value,
}

/// Opens an item, unless one is already open for the same case. An ml_review
/// item gets its deep-pass job in the same transaction.
pub async fn open(tx: &mut Transaction<'_, Postgres>, it: NewItem<'_>) -> sqlx::Result<Option<Uuid>> {
    let id: Option<Uuid> = sqlx::query_scalar(
        "INSERT INTO review_queue
           (item_kind, subject_kind, subject_id, ownership_match, banned_id, category, dedup_key, stage, reason, evidence, report_id, admin_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, CASE WHEN $8 = 'admin_review' THEN NOW() END)
         ON CONFLICT (dedup_key) WHERE stage <> 'decided' DO NOTHING
         RETURNING id",
    )
    .bind(it.item_kind)
    .bind(it.subject_kind)
    .bind(it.subject_id)
    .bind(it.ownership_match)
    .bind(it.banned_id)
    .bind(it.category)
    .bind(&it.dedup_key)
    .bind(it.stage.as_str())
    .bind(it.reason)
    .bind(&it.evidence)
    .bind(it.report_id)
    .fetch_optional(&mut **tx)
    .await?;
    if let (Some(id), Stage::MlReview) = (id, it.stage) {
        jobs::enqueue(tx, "ml_review", &id.to_string(), json!({})).await?;
    }
    if id.is_some() {
        metrics::counter!("abraxas_review_opened_total", "item_kind" => it.item_kind.to_string(), "stage" => it.stage.as_str())
            .increment(1);
    }
    Ok(id)
}

pub async fn pial_of(tx: &mut Transaction<'_, Postgres>, subject_kind: &str, subject_id: &str) -> sqlx::Result<Option<Uuid>> {
    match subject_kind {
        "asset" => {
            let Ok(asset) = Uuid::parse_str(subject_id) else { return Ok(None) };
            Ok(sqlx::query_scalar::<_, Option<Uuid>>("SELECT uploader_pial FROM fingerprints WHERE asset_id = $1")
                .bind(asset)
                .fetch_optional(&mut **tx)
                .await?
                .flatten())
        }
        "account" => Ok(Uuid::parse_str(subject_id).ok()),
        "stream" => Ok(None),
        _ => sqlx::query_scalar("SELECT pial FROM subjects WHERE kind = $1 AND id = $2")
            .bind(subject_kind)
            .bind(subject_id)
            .fetch_optional(&mut **tx)
            .await,
    }
}

/// Media or content no machine could judge: held at human_review and put in
/// front of a person.
pub async fn open_unjudged(tx: &mut Transaction<'_, Postgres>, subject_kind: &str, subject_id: &str, why: &str) -> sqlx::Result<()> {
    let pial = pial_of(tx, subject_kind, subject_id).await?;
    let asset_id = if subject_kind == "asset" { Uuid::parse_str(subject_id).ok() } else { None };
    let reason = format!("no machine could judge this: {why}");
    verdicts::write(tx, NewVerdict {
        subject_kind,
        subject_id,
        verdict: Verdict::HumanReview,
        signals: &["unjudged".to_string()],
        scores: json!({}),
        reason: &reason,
        source: "recheck",
        decided_by: "abraxas",
        asset_id,
        pial,
    })
    .await?;
    open(tx, NewItem {
        item_kind: "unjudged",
        subject_kind,
        subject_id,
        ownership_match: None,
        banned_id: None,
        category: None,
        report_id: None,
        dedup_key: format!("unjudged:{subject_kind}:{subject_id}"),
        stage: Stage::AdminReview,
        reason: &reason,
        evidence: json!({ "cause": why }),
    })
    .await?;
    if let Some(a) = asset_id {
        crate::scan::rescan_for_asset(tx, a).await?;
    }
    Ok(())
}

/// Moves an ml_review item to a person, keeping what the deep pass found.
pub async fn promote_to_admin(tx: &mut Transaction<'_, Postgres>, review_id: &str, why: &str) -> sqlx::Result<()> {
    let Ok(id) = Uuid::parse_str(review_id) else { return Ok(()) };
    promote(tx, id, json!({ "outcome": "escalated", "reason": why })).await
}

async fn promote(tx: &mut Transaction<'_, Postgres>, id: Uuid, outcome: Value) -> sqlx::Result<()> {
    sqlx::query(
        "UPDATE review_queue SET stage = 'admin_review', admin_at = NOW(), ml_outcome = $2
          WHERE id = $1 AND stage = 'ml_review'",
    )
    .bind(id)
    .bind(outcome)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

// ── suppression ──────────────────────────────────────────────────────────────

/// Signals reviewers keep overturning. A signal is suppressed once it has been
/// ruled on at least 5 times, reviewers approved what it raised at least 80% of
/// the time, and approvals outnumber blocks. Suppression lowers a review
/// trigger only; it never lifts a block (policy.rs).
#[derive(Clone, Default)]
pub struct Suppression {
    inner: Arc<RwLock<Option<(Instant, Arc<HashSet<String>>)>>>,
}

impl Suppression {
    pub async fn get(&self, pool: &PgPool) -> sqlx::Result<Arc<HashSet<String>>> {
        if let Some((at, set)) = self.inner.read().await.as_ref() {
            if at.elapsed() < Duration::from_secs(300) {
                return Ok(set.clone());
            }
        }
        let rows: Vec<String> = sqlx::query_scalar(
            "SELECT signal FROM (
                 SELECT split_part(unnest(signals), ':', 1) AS signal, decision FROM signal_feedback
             ) s
             GROUP BY signal
             HAVING COUNT(*) >= 5
                AND COUNT(*) FILTER (WHERE decision = 'approve') >= 0.8 * COUNT(*)
                AND COUNT(*) FILTER (WHERE decision = 'approve') > COUNT(*) FILTER (WHERE decision = 'block')",
        )
        .fetch_all(pool)
        .await?;
        let set = Arc::new(rows.into_iter().collect::<HashSet<_>>());
        *self.inner.write().await = Some((Instant::now(), set.clone()));
        Ok(set)
    }
}

// ── the ML stage ─────────────────────────────────────────────────────────────

#[derive(sqlx::FromRow)]
pub struct Item {
    pub id: Uuid,
    pub item_kind: String,
    pub subject_kind: String,
    pub subject_id: String,
    pub ownership_match: Option<Uuid>,
    pub banned_id: Option<Uuid>,
    pub category: Option<String>,
    pub report_id: Option<Uuid>,
    pub stage: String,
    pub reason: String,
    pub evidence: Value,
    pub ml_outcome: Option<Value>,
    pub decision: Option<String>,
    pub decided_by: Option<String>,
    pub note: Option<String>,
    pub created_at: DateTime<Utc>,
    pub admin_at: Option<DateTime<Utc>>,
    pub decided_at: Option<DateTime<Utc>>,
}

const ITEM_COLUMNS: &str = "id, item_kind, subject_kind, subject_id, ownership_match, banned_id, category, report_id, stage, \
     reason, evidence, ml_outcome, decision, decided_by, note, created_at, admin_at, decided_at";

pub async fn load(pool: &PgPool, id: Uuid) -> sqlx::Result<Option<Item>> {
    sqlx::query_as::<_, Item>(&format!("SELECT {ITEM_COLUMNS} FROM review_queue WHERE id = $1"))
        .bind(id)
        .fetch_optional(pool)
        .await
}

pub async fn run_ml_review(st: &AppState, review_id: &str) -> Result<(), JobError> {
    let id = Uuid::parse_str(review_id).map_err(|_| JobError::Escalate(format!("bad review id {review_id}")))?;
    let Some(item) = load(&st.pool, id).await? else {
        return Err(JobError::Escalate(format!("review {id} does not exist")));
    };
    if item.stage != "ml_review" {
        return Ok(());
    }
    match item.item_kind.as_str() {
        "content" => ml_content(st, &item).await,
        "banned_near" => ml_banned_near(st, &item).await,
        "ownership" => ml_ownership(st, &item).await,
        _ => {
            let mut tx = st.pool.begin().await?;
            promote(&mut tx, id, json!({ "outcome": "not_machine_decidable" })).await?;
            tx.commit().await?;
            Ok(())
        }
    }
}

async fn ml_content(st: &AppState, item: &Item) -> Result<(), JobError> {
    let a = crate::scan::assess(st, &item.subject_kind, &item.subject_id, true).await?;
    let mut tx = st.pool.begin().await?;
    a.store(&mut tx, st, true).await?;
    if a.decision.verdict == Verdict::HumanReview || a.decision.verdict == Verdict::Pending {
        promote(&mut tx, item.id, json!({
            "outcome": "still_uncertain",
            "verdict": a.decision.verdict.as_str(),
            "signals": a.decision.signals,
            "scores": a.decision.scores,
            "reason": a.decision.reason,
        }))
        .await?;
    } else {
        let decision = a.decision.verdict.as_str();
        close(&mut tx, item, decision, "ml", None, json!({ "deep_scores": a.decision.scores, "signals": a.decision.signals })).await?;
        verdicts::write(&mut tx, NewVerdict {
            subject_kind: &item.subject_kind,
            subject_id: &item.subject_id,
            verdict: a.decision.verdict,
            signals: &a.decision.signals,
            scores: a.decision.scores.clone(),
            reason: &format!("deep pass: {}", a.decision.reason),
            source: "ml_review",
            decided_by: "ml",
            asset_id: None,
            pial: Some(a.pial),
        })
        .await?;
        announce_decided(&mut tx, item, a.decision.verdict, decision, "ml", None).await?;
        crate::scan::release_body(&mut tx, &item.subject_kind, &item.subject_id).await?;
    }
    tx.commit().await?;
    Ok(())
}

/// Chromaprint frames of a fingerprint, when it has a usable one.
async fn chroma_of(pool: &PgPool, fingerprint: Uuid) -> sqlx::Result<Option<Vec<u32>>> {
    let fp: Option<Option<String>> = sqlx::query_scalar("SELECT chromaprint FROM fingerprints WHERE id = $1")
        .bind(fingerprint)
        .fetch_optional(pool)
        .await?;
    Ok(fp.flatten().and_then(|c| decode_chromaprint(&c).ok()))
}

async fn ml_banned_near(st: &AppState, item: &Item) -> Result<(), JobError> {
    let new_fp = item.evidence.get("fingerprint_id").and_then(|v| v.as_str()).and_then(|s| Uuid::parse_str(s).ok());
    let banned = match item.banned_id {
        Some(b) => sqlx::query_as::<_, (String, Option<String>, Option<Uuid>)>(
            "SELECT layer, chromaprint, fingerprint_id FROM banned_hashes WHERE id = $1",
        )
        .bind(b)
        .fetch_optional(&st.pool)
        .await?,
        None => None,
    };
    let mut similarity: Option<f64> = None;
    if let (Some(new_fp), Some((_, chroma, banned_fp))) = (new_fp, banned) {
        let reference = match (chroma, banned_fp) {
            (Some(c), _) => decode_chromaprint(&c).ok(),
            (None, Some(fp)) => chroma_of(&st.pool, fp).await?,
            _ => None,
        };
        if let (Some(r), Some(q)) = (reference, chroma_of(&st.pool, new_fp).await?) {
            similarity = crate::hashing::chroma_similarity(&q, &r);
        }
    }
    let mut tx = st.pool.begin().await?;
    match similarity {
        Some(s) if s >= crate::banned::CHROMA_BLOCK => {
            let asset = Uuid::parse_str(&item.subject_id).ok();
            let pial = pial_of(&mut tx, "asset", &item.subject_id).await?;
            close(&mut tx, item, "block", "ml", None, json!({ "verification": "chromaprint", "similarity": s })).await?;
            verdicts::write(&mut tx, NewVerdict {
                subject_kind: "asset",
                subject_id: &item.subject_id,
                verdict: Verdict::Block,
                signals: &[format!("banned_confirmed:{}", item.category.clone().unwrap_or_default())],
                scores: json!({ "similarity": s }),
                reason: "deep pass confirmed banned material",
                source: "ml_review",
                decided_by: "ml",
                asset_id: asset,
                pial,
            })
            .await?;
            announce_decided(&mut tx, item, Verdict::Block, "block", "ml", None).await?;
            if let Some(a) = asset {
                crate::scan::rescan_for_asset(&mut tx, a).await?;
            }
        }
        other => {
            promote(&mut tx, item.id, json!({
                "outcome": "not_confirmed",
                "verification": "chromaprint",
                "similarity": other,
            }))
            .await?;
        }
    }
    tx.commit().await?;
    Ok(())
}

async fn ml_ownership(st: &AppState, item: &Item) -> Result<(), JobError> {
    let Some(mid) = item.ownership_match else {
        return Err(JobError::Escalate("ownership item without a match".into()));
    };
    let m: Option<(Uuid, Uuid, Value)> = sqlx::query_as(
        "SELECT new_fingerprint, original_fingerprint, offsets FROM ownership_matches WHERE id = $1",
    )
    .bind(mid)
    .fetch_optional(&st.pool)
    .await?;
    let Some((new_fp, orig_fp, offsets)) = m else {
        return Err(JobError::Escalate(format!("ownership match {mid} does not exist")));
    };
    let mut similarity = None;
    if let (Some(q), Some(r)) = (chroma_of(&st.pool, new_fp).await?, chroma_of(&st.pool, orig_fp).await?) {
        let scale = offsets.get("scale").and_then(|v| v.as_f64()).unwrap_or(1.0);
        // Chromaprint cannot be compared across a speed change; only a
        // unit-scale alignment can be verified this way.
        if (scale - 1.0).abs() <= 0.01 {
            similarity = match (offsets.get("new_start").and_then(|v| v.as_f64()), offsets.get("original_start").and_then(|v| v.as_f64())) {
                (Some(ns), Some(os)) => chroma_similarity_at(
                    &q,
                    &r,
                    (ns / CHROMA_FRAME_SECONDS).max(0.0) as usize,
                    (os / CHROMA_FRAME_SECONDS).max(0.0) as usize,
                    8,
                ),
                _ => crate::hashing::chroma_similarity(&q, &r),
            };
        }
    }
    let mut tx = st.pool.begin().await?;
    match similarity {
        Some(s) if s >= crate::ownership::CHROMA_CREDIT => {
            let body = credit(&mut tx, mid, "ml").await?;
            close(&mut tx, item, "credit", "ml", None, json!({ "verification": "chromaprint", "similarity": s })).await?;
            let verdict = verdicts::current(&mut *tx, "asset", &item.subject_id).await?.map(|(v, _)| v).unwrap_or(Verdict::Approve);
            announce_decided(&mut tx, item, verdict, "credit", "ml", Some(body)).await?;
        }
        other => {
            promote(&mut tx, item.id, json!({ "outcome": "not_confirmed", "verification": "chromaprint", "similarity": other }))
                .await?;
        }
    }
    tx.commit().await?;
    Ok(())
}

/// Credits an ownership match. The credit reaches feed-engine as the
/// review_decided event that carries the returned body.
async fn credit(tx: &mut Transaction<'_, Postgres>, mid: Uuid, by: &str) -> sqlx::Result<Value> {
    let row: (Option<Uuid>, Option<Uuid>, Uuid, String, String, f64, Option<f64>) = sqlx::query_as(
        "UPDATE ownership_matches SET state = 'credited', decided_by = $2, decided_at = NOW()
          WHERE id = $1
          RETURNING original_asset, original_pial, original_fingerprint, kind, layer, confidence, offset_seconds",
    )
    .bind(mid)
    .bind(by)
    .fetch_one(&mut **tx)
    .await?;
    let (original_asset, original_pial, original_fp, kind, layer, confidence, offset) = row;
    Ok(json!({
        "match_id": mid, "original_asset": original_asset, "original_pial": original_pial,
        "original_fingerprint": original_fp, "kind": kind, "layer": layer,
        "confidence": confidence, "offset_seconds": offset, "state": "credited",
    }))
}

pub async fn close(tx: &mut Transaction<'_, Postgres>, item: &Item, decision: &str, by: &str, note: Option<&str>, outcome: Value) -> sqlx::Result<()> {
    sqlx::query(
        "UPDATE review_queue
            SET stage = 'decided', decision = $2, decided_by = $3, note = $4, decided_at = NOW(),
                ml_outcome = COALESCE(ml_outcome, '{}'::jsonb) || $5
          WHERE id = $1 AND stage <> 'decided'",
    )
    .bind(item.id)
    .bind(decision)
    .bind(by)
    .bind(note)
    .bind(outcome)
    .execute(&mut **tx)
    .await?;
    metrics::counter!("abraxas_review_decided_total", "item_kind" => item.item_kind.clone(), "by" => if by == "ml" { "ml" } else { "admin" })
        .increment(1);
    Ok(())
}

pub async fn announce_decided(
    tx: &mut Transaction<'_, Postgres>,
    item: &Item,
    verdict: Verdict,
    decision: &str,
    by: &str,
    ownership: Option<Value>,
) -> sqlx::Result<()> {
    let pial = pial_of(tx, &item.subject_kind, &item.subject_id).await?;
    let asset_id = if item.subject_kind == "asset" { Uuid::parse_str(&item.subject_id).ok() } else { None };
    let signals: Vec<String> = Vec::new();
    outbox::enqueue(
        tx,
        Event {
            kind: EventType::ReviewDecided,
            subject_kind: &item.subject_kind,
            subject_id: &item.subject_id,
            asset_id,
            pial,
            verdict: verdict.as_str(),
            scores: json!({}),
            signals: &signals,
            reason: &item.reason,
            ownership,
            banned: item.category.as_ref().map(|c| json!({ "category": c, "banned_id": item.banned_id })),
            review: Some(json!({
                "id": item.id, "item_kind": item.item_kind, "stage": "decided",
                "decision": decision, "decided_by": by,
            })),
        },
    )
    .await
}

// ── admin API ────────────────────────────────────────────────────────────────

/// Only numbers: the admin Facets read `scores` as name → number.
fn numeric_scores(v: Option<&Value>) -> Value {
    let mut out = serde_json::Map::new();
    if let Some(Value::Object(m)) = v {
        for (k, x) in m {
            if let Some(n) = x.as_f64() {
                out.insert(k.clone(), json!(n));
            }
        }
    }
    Value::Object(out)
}

async fn item_json(pool: &PgPool, i: &Item) -> sqlx::Result<Value> {
    let current = verdicts::current_pool(pool, &i.subject_kind, &i.subject_id).await?;
    let asset_id = if i.subject_kind == "asset" { Some(i.subject_id.clone()) } else { None };
    let pial: Option<Uuid> = match i.subject_kind.as_str() {
        "asset" => match Uuid::parse_str(&i.subject_id) {
            Ok(a) => sqlx::query_scalar::<_, Option<Uuid>>("SELECT uploader_pial FROM fingerprints WHERE asset_id = $1")
                .bind(a)
                .fetch_optional(pool)
                .await?
                .flatten(),
            Err(_) => None,
        },
        "account" => Uuid::parse_str(&i.subject_id).ok(),
        "stream" => None,
        _ => sqlx::query_scalar("SELECT pial FROM subjects WHERE kind = $1 AND id = $2")
            .bind(&i.subject_kind)
            .bind(&i.subject_id)
            .fetch_optional(pool)
            .await?,
    };
    let ownership = match i.ownership_match {
        Some(mid) => sqlx::query_as::<_, (Option<Uuid>, Option<Uuid>, String, f64, Option<f64>, String)>(
            "SELECT original_pial, original_asset, kind, confidence, offset_seconds, state FROM ownership_matches WHERE id = $1",
        )
        .bind(mid)
        .fetch_optional(pool)
        .await?
        .map(|(p, a, k, c, o, s)| json!({
            "match_id": mid, "original_pial": p, "original_asset": a, "kind": k,
            "confidence": c, "offset_seconds": o, "state": s,
        })),
        None => None,
    };
    let signals: Vec<String> = i
        .ml_outcome
        .as_ref()
        .and_then(|o| o.get("signals"))
        .or_else(|| i.evidence.get("signals"))
        .and_then(|s| serde_json::from_value(s.clone()).ok())
        .or_else(|| current.as_ref().map(|(_, s)| s.clone()))
        .unwrap_or_default();
    let scores = numeric_scores(
        i.ml_outcome.as_ref().and_then(|o| o.get("scores")).or_else(|| i.evidence.get("scores")),
    );
    Ok(json!({
        "review_id": i.id,
        "item_kind": i.item_kind,
        "stage": i.stage,
        "subject": { "kind": i.subject_kind, "id": i.subject_id },
        "asset_id": asset_id,
        "pial": pial,
        "reason": i.reason,
        "scores": scores,
        "signals": signals,
        "ownership": ownership,
        "report_id": i.report_id,
        "category": i.category,
        "evidence": i.evidence,
        "ml_outcome": i.ml_outcome,
        "decision": i.decision,
        "decided_by": i.decided_by,
        "note": i.note,
        "current_verdict": current.as_ref().map(|(v, _)| v.as_str()),
        "created_at": i.created_at,
        "admin_at": i.admin_at,
        "decided_at": i.decided_at,
    }))
}

#[derive(Debug, Deserialize)]
pub struct ListQuery {
    #[serde(default)]
    pub stage: Option<String>,
    #[serde(default)]
    pub item_kind: Option<String>,
    /// created_at cursor: the page after the last item seen.
    #[serde(default)]
    pub before: Option<DateTime<Utc>>,
    #[serde(default)]
    pub limit: Option<i64>,
}

/// `GET /v1/review?stage=admin_review&item_kind=&before=&limit=` — oldest
/// waiting first for open stages, newest first for decided.
pub async fn list(State(st): State<AppState>, Query(q): Query<ListQuery>) -> ApiResult<Json<Value>> {
    let stage = q.stage.filter(|s| !s.is_empty()).unwrap_or_else(|| "admin_review".into());
    if !matches!(stage.as_str(), "ml_review" | "admin_review" | "decided") {
        return Err(ApiError::bad_request("stage must be ml_review, admin_review or decided"));
    }
    let limit = q.limit.unwrap_or(50).clamp(1, 200);
    let (order, cmp) = if stage == "decided" { ("DESC", "<") } else { ("ASC", ">") };
    let items: Vec<Item> = sqlx::query_as::<_, Item>(&format!(
        "SELECT {ITEM_COLUMNS} FROM review_queue
          WHERE stage = $1 AND ($2::text IS NULL OR item_kind = $2)
            AND ($3::timestamptz IS NULL OR created_at {cmp} $3)
          ORDER BY created_at {order} LIMIT $4"
    ))
    .bind(&stage)
    .bind(&q.item_kind)
    .bind(q.before)
    .bind(limit)
    .fetch_all(&st.pool)
    .await?;
    let mut out = Vec::with_capacity(items.len());
    for i in &items {
        out.push(item_json(&st.pool, i).await?);
    }
    let next = items.last().map(|i| i.created_at);
    Ok(Json(json!({ "items": out, "count": out.len(), "next_before": next })))
}

pub async fn get(State(st): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let Some(i) = load(&st.pool, id).await? else { return Err(ApiError::not_found("no such review item")) };
    Ok(Json(item_json(&st.pool, &i).await?))
}

#[derive(Debug, Deserialize)]
pub struct DecideRequest {
    pub decision: String,
    /// The moderator's PIAL. Recorded as who decided.
    pub actor_pial: Uuid,
    #[serde(default)]
    pub note: Option<String>,
    /// Ownership claims only: the claimant's asset to credit as the original,
    /// when Abraxas found none of theirs matching on its own.
    #[serde(default)]
    pub original_asset: Option<Uuid>,
}

/// `POST /v1/review/{id}/decide {decision, actor_pial, note?, original_asset?}`
///
///   content, unjudged           approve | age_gate | block
///   banned_near                 approve | block
///   report                      approve (report dismissed) | block (subject
///                               blocked — an account subject is banned —
///                               and the report resolved)
///   ownership, ownership_claim  credit | reject
///
/// Every decision publishes review_decided; a credit carries `ownership`.
/// 404: no such item. 409: already decided.
pub async fn decide(State(st): State<AppState>, Path(id): Path<Uuid>, Json(req): Json<DecideRequest>) -> ApiResult<Json<Value>> {
    let by = req.actor_pial.to_string();
    let Some(item) = load(&st.pool, id).await? else { return Err(ApiError::not_found("no such review item")) };
    if item.stage == "decided" {
        return Err(ApiError::conflict(format!("already decided: {}", item.decision.clone().unwrap_or_default())));
    }
    let allowed: &[&str] = match item.item_kind.as_str() {
        "content" | "unjudged" => &["approve", "age_gate", "block"],
        "banned_near" | "report" => &["approve", "block"],
        "ownership" | "ownership_claim" => &["credit", "reject"],
        _ => &[],
    };
    if !allowed.contains(&req.decision.as_str()) {
        return Err(ApiError::bad_request(format!("a {} item takes one of: {}", item.item_kind, allowed.join(", "))));
    }

    let mut tx = st.pool.begin().await?;
    // Lock the row so two moderators cannot both decide it.
    let stage: String = sqlx::query_scalar("SELECT stage FROM review_queue WHERE id = $1 FOR UPDATE")
        .bind(id)
        .fetch_one(&mut *tx)
        .await?;
    if stage == "decided" {
        return Err(ApiError::conflict("already decided"));
    }
    close(&mut tx, &item, &req.decision, &by, req.note.as_deref(), json!({})).await?;

    match item.item_kind.as_str() {
        "ownership" => {
            let mid = item.ownership_match.ok_or_else(|| ApiError::conflict("ownership item without a match"))?;
            let verdict = verdicts::current(&mut *tx, &item.subject_kind, &item.subject_id).await?.map(|(v, _)| v).unwrap_or(Verdict::Approve);
            let body = if req.decision == "credit" {
                credit(&mut tx, mid, &by).await?
            } else {
                sqlx::query("UPDATE ownership_matches SET state = 'rejected', decided_by = $2, decided_at = NOW() WHERE id = $1")
                    .bind(mid)
                    .bind(&by)
                    .execute(&mut *tx)
                    .await?;
                json!({ "match_id": mid, "state": "rejected" })
            };
            announce_decided(&mut tx, &item, verdict, &req.decision, &by, Some(body)).await?;
        }
        "ownership_claim" => {
            let report = item.report_id.ok_or_else(|| ApiError::conflict("claim without a report"))?;
            let verdict = verdicts::current(&mut *tx, &item.subject_kind, &item.subject_id).await?.map(|(v, _)| v).unwrap_or(Verdict::Approve);
            let body = if req.decision == "credit" {
                let body = crate::reports::credit_claim(&mut tx, &item, report, req.original_asset, &by).await?;
                crate::reports::close_report(&mut tx, report, "resolved", &by, req.note.as_deref()).await?;
                body
            } else {
                crate::reports::close_report(&mut tx, report, "dismissed", &by, req.note.as_deref()).await?;
                json!({ "state": "rejected" })
            };
            announce_decided(&mut tx, &item, verdict, &req.decision, &by, Some(body)).await?;
        }
        "report" => {
            let report = item.report_id.ok_or_else(|| ApiError::conflict("report item without a report"))?;
            if req.decision == "block" {
                apply_block(&mut tx, &item, &by, req.note.as_deref(), report).await?;
                crate::reports::close_report(&mut tx, report, "resolved", &by, req.note.as_deref()).await?;
            } else {
                crate::reports::close_report(&mut tx, report, "dismissed", &by, req.note.as_deref()).await?;
            }
            let verdict = if req.decision == "block" { Verdict::Block } else { Verdict::Approve };
            announce_decided(&mut tx, &item, verdict, &req.decision, &by, None).await?;
        }
        _ => {
            let verdict = Verdict::parse(&req.decision).ok_or_else(|| ApiError::bad_request("unknown decision"))?;
            let asset_id = if item.subject_kind == "asset" { Uuid::parse_str(&item.subject_id).ok() } else { None };
            let pial = pial_of(&mut tx, &item.subject_kind, &item.subject_id).await?;
            let reason = format!("admin decision on {}: {}", item.item_kind, req.note.clone().unwrap_or_default());
            let signals = vec![format!("admin:{}", req.decision)];
            verdicts::write(&mut tx, NewVerdict {
                subject_kind: &item.subject_kind,
                subject_id: &item.subject_id,
                verdict,
                signals: &signals,
                scores: json!({}),
                reason: &reason,
                source: "admin",
                decided_by: &by,
                asset_id,
                pial,
            })
            .await?;
            if item.item_kind == "banned_near" && verdict == Verdict::Block {
                crate::check::record_evidence(&mut tx, crate::check::EvidenceRow {
                    surface: "upload", subject_ref: &item.subject_id, asset_id, fingerprint_id: None, pial,
                    media_kind: "asset", result: "flagged", exact: Some(false), matched: None, sha256: None,
                })
                .await?;
            }
            // The ruling trains suppression: which signals raised this, and
            // what a person made of them.
            if item.item_kind == "content" {
                let raised: Vec<String> = item
                    .ml_outcome
                    .as_ref()
                    .and_then(|o| o.get("signals"))
                    .or_else(|| item.evidence.get("signals"))
                    .and_then(|s| serde_json::from_value(s.clone()).ok())
                    .unwrap_or_default();
                if !raised.is_empty() {
                    sqlx::query(
                        "INSERT INTO signal_feedback (review_id, subject_ref, signals, decision, decided_by)
                         VALUES ($1, $2, $3, $4, $5)",
                    )
                    .bind(item.id)
                    .bind(format!("{}:{}", item.subject_kind, item.subject_id))
                    .bind(&raised)
                    .bind(&req.decision)
                    .bind(&by)
                    .execute(&mut *tx)
                    .await?;
                }
            }
            announce_decided(&mut tx, &item, verdict, &req.decision, &by, None).await?;
            match asset_id {
                Some(a) => crate::scan::rescan_for_asset(&mut tx, a).await?,
                None => crate::scan::release_body(&mut tx, &item.subject_kind, &item.subject_id).await?,
            }
        }
    }
    tx.commit().await?;
    jobs::wake(&st.jobs_wake);
    let after = load(&st.pool, id).await?.ok_or_else(|| ApiError::not_found("no such review item"))?;
    Ok(Json(item_json(&st.pool, &after).await?))
}

/// A reported subject a moderator blocked. Content is blocked by verdict; an
/// account is banned — by elohim-veni, through the identity command outbox.
async fn apply_block(tx: &mut Transaction<'_, Postgres>, item: &Item, by: &str, note: Option<&str>, report: Uuid) -> ApiResult<()> {
    if item.subject_kind == "account" {
        let pial = Uuid::parse_str(&item.subject_id)
            .map_err(|_| ApiError::conflict("an account subject's id must be its PIAL"))?;
        crate::enforcement::enqueue_ban(tx, pial, note.unwrap_or("report upheld"), by, &format!("report:{report}")).await?;
        return Ok(());
    }
    let asset_id = if item.subject_kind == "asset" { Uuid::parse_str(&item.subject_id).ok() } else { None };
    let pial = pial_of(tx, &item.subject_kind, &item.subject_id).await?;
    verdicts::write(tx, NewVerdict {
        subject_kind: &item.subject_kind,
        subject_id: &item.subject_id,
        verdict: Verdict::Block,
        signals: &[format!("report_upheld:{report}")],
        scores: json!({}),
        reason: &format!("report upheld: {}", note.unwrap_or("")),
        source: "admin",
        decided_by: by,
        asset_id,
        pial,
    })
    .await?;
    if let Some(a) = asset_id {
        crate::scan::rescan_for_asset(tx, a).await?;
    }
    Ok(())
}
