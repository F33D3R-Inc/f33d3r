//! The synchronous integrity gate.
//!
//! `POST /v1/check` — caeor calls it before an asset becomes ready:
//!
//! ```text
//!   request  {asset_id, kind, uploader_pial, sha256, private_path, duration?}
//!            kind: image|video|voice|audio|clip|avatar|header|encrypted
//!            private_path: relative to the media private store
//!   response {verdict: allow|block|review,
//!             banned_match?: {category, layer, match: exact|near},
//!             ownership?: {original_asset, original_pial, kind, confidence,
//!                          offset_seconds?, state: credited|ml_review},
//!             fingerprint_id, reason}
//! ```
//!
//! Order: exact sha256 against the banned registry (local; a database failure
//! answers 503 — the gate fails CLOSED); then the worker fingerprints the file;
//! then the perceptual banned layers; then ownership against the index. The
//! fingerprint is recorded, PIAL-keyed, permanently, whatever the outcome.
//!
//! `review` means the asset must not be published as judged:
//!   * the worker could not answer → verdict `pending`, and a durable
//!     asset_recheck job re-runs this check until it can (then escalates to a
//!     person). Never allowed by default.
//!   * the media is near banned material → ml_review (csam: straight to a person).
//!   * the worker cannot read the media at all → a person judges it.
//! An uncertain OWNERSHIP match does not hold the asset: the media is not an
//! integrity risk, only its credit is in question. It is allowed, and the
//! ownership review settles credit later (an `ownership` event).
//!
//! The final state of every asset also arrives on safety.events (subject
//! kind `asset`), and can be read back with GET /v1/assets/{id}/verdict.

use std::path::PathBuf;

use axum::extract::{Path as AxPath, State};
use axum::http::StatusCode;
use axum::Json;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

use crate::api::{ApiError, ApiResult, AppState};
use crate::banned::{self, BannedMatch, Outcome};
use crate::index::{self, robust_keys, AudioKind};
use crate::jobs::{self, JobError};
use crate::media::{self, Kind};
use crate::outbox::{self, Event, EventType};
use crate::ownership::{self, Level};
use crate::review::{self, NewItem, Stage};
use crate::verdicts::{self, NewVerdict, Verdict};
use crate::worker::WorkerError;

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct CheckRequest {
    pub asset_id: Uuid,
    pub kind: String,
    pub uploader_pial: Uuid,
    pub sha256: String,
    pub private_path: String,
    #[serde(default)]
    pub duration: Option<f64>,
    /// When the asset was first uploaded. Set only by `abraxas-shield
    /// backfill` for media that predates the gate, so a legacy original keeps
    /// its place in time and is credited ahead of later copies.
    #[serde(default)]
    pub uploaded_at: Option<chrono::DateTime<chrono::Utc>>,
}

#[derive(Debug, Serialize)]
pub struct CheckResponse {
    pub verdict: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub banned_match: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ownership: Option<Value>,
    pub fingerprint_id: Uuid,
    pub reason: String,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Source {
    Check,
    Recheck,
    Backfill,
}

impl Source {
    fn as_str(self) -> &'static str {
        match self {
            Source::Check | Source::Backfill => "check",
            Source::Recheck => "recheck",
        }
    }
}

pub enum Failure {
    Invalid(String),
    Conflict(String),
    Mismatch(String),
    Db(sqlx::Error),
    Worker(WorkerError),
}

impl From<sqlx::Error> for Failure {
    fn from(e: sqlx::Error) -> Self {
        Failure::Db(e)
    }
}

impl From<Failure> for ApiError {
    fn from(f: Failure) -> Self {
        match f {
            Failure::Invalid(s) => ApiError::bad_request(s),
            Failure::Conflict(s) => ApiError::conflict(s),
            Failure::Mismatch(s) => ApiError::new(StatusCode::UNPROCESSABLE_ENTITY, "sha256_mismatch", s),
            Failure::Db(e) => ApiError::from(e),
            Failure::Worker(e) => ApiError::new(StatusCode::SERVICE_UNAVAILABLE, "worker_unavailable", e.to_string()),
        }
    }
}

// ── evidence ─────────────────────────────────────────────────────────────────

pub struct EvidenceRow<'a> {
    pub surface: &'a str,
    pub subject_ref: &'a str,
    pub asset_id: Option<Uuid>,
    pub fingerprint_id: Option<Uuid>,
    pub pial: Option<Uuid>,
    pub media_kind: &'a str,
    pub result: &'a str,
    pub exact: Option<bool>,
    pub matched: Option<&'a BannedMatch>,
    pub sha256: Option<&'a str>,
}

pub async fn record_evidence(tx: &mut Transaction<'_, Postgres>, e: EvidenceRow<'_>) -> sqlx::Result<()> {
    sqlx::query(
        "INSERT INTO csam_evidence_log
           (surface, subject_ref, asset_id, fingerprint_id, pial, media_kind, result, match_kind,
            layer, category, banned_id, sha256, score, detail)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)",
    )
    .bind(e.surface)
    .bind(e.subject_ref)
    .bind(e.asset_id)
    .bind(e.fingerprint_id)
    .bind(e.pial)
    .bind(e.media_kind)
    .bind(e.result)
    .bind(e.exact.map(|x| if x { "exact" } else { "near" }))
    .bind(e.matched.map(|m| m.layer))
    .bind(e.matched.map(|m| m.category.as_str()))
    .bind(e.matched.map(|m| m.banned_id))
    .bind(e.sha256)
    .bind(e.matched.map(|m| m.score as f32).unwrap_or(0.0))
    .bind(e.matched.map(|m| m.detail.clone()).unwrap_or_else(|| json!({})))
    .execute(&mut **tx)
    .await?;
    Ok(())
}

/// The banned_match event for one finding.
async fn announce_banned(
    tx: &mut Transaction<'_, Postgres>,
    subject_kind: &str,
    subject_id: &str,
    asset_id: Option<Uuid>,
    pial: Option<Uuid>,
    verdict: Verdict,
    m: &BannedMatch,
    exact: bool,
    sha256: &str,
) -> sqlx::Result<()> {
    let signals = vec![format!("banned_{}:{}", m.layer, m.category)];
    outbox::enqueue(
        tx,
        Event {
            kind: EventType::BannedMatch,
            subject_kind,
            subject_id,
            asset_id,
            pial,
            verdict: verdict.as_str(),
            scores: json!({ "match_score": m.score }),
            signals: &signals,
            reason: if exact { "banned registry: exact bytes" } else { "banned registry: perceptual match" },
            ownership: None,
            banned: Some(m.json(exact, sha256)),
            review: None,
        },
    )
    .await
}

// ── fingerprint row ──────────────────────────────────────────────────────────

struct FpRow {
    id: Uuid,
}

async fn ensure_fingerprint(pool: &PgPool, req: &CheckRequest, kind: Kind) -> Result<FpRow, Failure> {
    sqlx::query(
        "INSERT INTO fingerprints (asset_id, uploader_pial, kind, sha256, private_path, duration_seconds, ownership_eligible, created_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, NOW()))
         ON CONFLICT (asset_id) DO NOTHING",
    )
    .bind(req.asset_id)
    .bind(req.uploader_pial)
    .bind(kind.as_str())
    .bind(&req.sha256)
    .bind(&req.private_path)
    .bind(req.duration)
    .bind(kind.ownership_eligible())
    .bind(req.uploaded_at)
    .execute(pool)
    .await?;
    let (id, pial, sha, k, origin, perceptual): (Uuid, Option<Uuid>, Option<String>, Option<String>, String, bool) =
        sqlx::query_as(
            "SELECT id, uploader_pial, sha256, kind, origin, perceptual_at IS NOT NULL FROM fingerprints WHERE asset_id = $1",
        )
        .bind(req.asset_id)
        .fetch_one(pool)
        .await?;
    // A fingerprint imported from feed-engine for this asset is adopted by
    // its first real check: it keeps its id, its provenance and its place in
    // time (the original upload), and gains what only a check knows — the
    // owner caeor records, the kind, the bytes' hash and the private source.
    if origin == "import" && !perceptual {
        if pial.is_some() && pial != Some(req.uploader_pial) {
            return Err(Failure::Conflict(format!(
                "asset {} was imported with a different uploader than caeor records",
                req.asset_id
            )));
        }
        sqlx::query(
            "UPDATE fingerprints
                SET uploader_pial = $2, kind = $3, sha256 = $4, private_path = $5,
                    duration_seconds = COALESCE($6, duration_seconds), ownership_eligible = $7
              WHERE id = $1",
        )
        .bind(id)
        .bind(req.uploader_pial)
        .bind(kind.as_str())
        .bind(&req.sha256)
        .bind(&req.private_path)
        .bind(req.duration)
        .bind(kind.ownership_eligible())
        .execute(pool)
        .await?;
        return Ok(FpRow { id });
    }
    // An asset id names one set of bytes from one person, forever. A second
    // check that disagrees is a caller bug, and answering it would rewrite
    // the record of who owns what.
    if pial != Some(req.uploader_pial) || sha.as_deref() != Some(req.sha256.as_str()) || k.as_deref() != Some(kind.as_str()) {
        return Err(Failure::Conflict(format!(
            "asset {} is already fingerprinted with a different uploader, sha256 or kind",
            req.asset_id
        )));
    }
    Ok(FpRow { id })
}

// ── the check ────────────────────────────────────────────────────────────────

fn validate(st: &AppState, req: &mut CheckRequest) -> Result<(Kind, PathBuf), Failure> {
    let kind = Kind::parse(&req.kind).ok_or_else(|| Failure::Invalid(format!("unknown kind {:?}", req.kind)))?;
    req.sha256 = req.sha256.trim().to_ascii_lowercase();
    if !media::is_sha256_hex(&req.sha256) {
        return Err(Failure::Invalid("sha256 must be 64 hex digits".into()));
    }
    if let Some(d) = req.duration {
        if !d.is_finite() || d < 0.0 {
            return Err(Failure::Invalid("duration must be a non-negative number".into()));
        }
    }
    let abs = media::private_abs(&st.cfg.private_dir, &req.private_path).map_err(Failure::Invalid)?;
    Ok((kind, abs))
}

pub async fn run(st: &AppState, mut req: CheckRequest, source: Source) -> Result<CheckResponse, Failure> {
    let (kind, abs) = validate(st, &mut req)?;
    let asset = req.asset_id.to_string();
    let pial = Some(req.uploader_pial);
    let current = verdicts::current_pool(&st.pool, "asset", &asset).await?.map(|(v, _)| v);

    // 1. Exact bytes. Fails closed: an error here is a 503, never an allow.
    //    Runs on every call, so a hash banned after an asset was allowed
    //    takes it down the next time anyone asks.
    if let Some(m) = banned::sha256(&st.pool, &req.sha256).await? {
        let fp = ensure_fingerprint(&st.pool, &req, kind).await?;
        if current == Some(Verdict::Block) {
            if let Some(resp) = stored_response(&st.pool, req.asset_id, fp.id).await? {
                return Ok(resp);
            }
        }
        let mut tx = st.pool.begin().await?;
        record_evidence(&mut tx, EvidenceRow {
            surface: "upload", subject_ref: &asset, asset_id: Some(req.asset_id), fingerprint_id: Some(fp.id),
            pial, media_kind: kind.as_str(), result: "flagged", exact: Some(true), matched: Some(&m), sha256: Some(&req.sha256),
        }).await?;
        let signals = vec![format!("banned_sha256:{}", m.category)];
        verdicts::write(&mut tx, NewVerdict {
            subject_kind: "asset", subject_id: &asset, verdict: Verdict::Block, signals: &signals,
            scores: json!({}), reason: "banned registry: exact bytes", source: source.as_str(),
            decided_by: "abraxas", asset_id: Some(req.asset_id), pial,
        }).await?;
        announce_banned(&mut tx, "asset", &asset, Some(req.asset_id), pial, Verdict::Block, &m, true, &req.sha256).await?;
        crate::scan::rescan_for_asset(&mut tx, req.asset_id).await?;
        tx.commit().await?;
        jobs::wake(&st.jobs_wake);
        return Ok(CheckResponse {
            verdict: "block",
            banned_match: Some(m.json(true, &req.sha256)),
            ownership: None,
            fingerprint_id: fp.id,
            reason: "banned registry: exact bytes".into(),
        });
    }

    let fp = ensure_fingerprint(&st.pool, &req, kind).await?;

    // The check is idempotent per asset: a repeat call answers the asset's
    // CURRENT standing — a decided review turns `review` into `allow` or
    // `block` — and a pending asset whose recheck is queued is not
    // re-fingerprinted on every poll; the recheck job owns that.
    if source == Source::Check {
        match current {
            Some(Verdict::Pending) => {
                let queued: bool = sqlx::query_scalar(
                    "SELECT EXISTS (SELECT 1 FROM jobs WHERE kind = 'asset_recheck' AND ref = $1 AND state = 'pending')",
                )
                .bind(&asset)
                .fetch_one(&st.pool)
                .await?;
                if queued {
                    return Ok(CheckResponse {
                        verdict: "review",
                        banned_match: None,
                        ownership: None,
                        fingerprint_id: fp.id,
                        reason: "integrity check queued: the worker has not judged this media yet".into(),
                    });
                }
            }
            Some(_) => {
                if let Some(resp) = stored_response(&st.pool, req.asset_id, fp.id).await? {
                    return Ok(resp);
                }
            }
            None => {}
        }
    }

    // 2. Ciphertext: the exact-bytes layer is all there is.
    if kind.is_opaque() {
        let mut tx = st.pool.begin().await?;
        verdicts::write(&mut tx, NewVerdict {
            subject_kind: "asset", subject_id: &asset, verdict: Verdict::Approve, signals: &["opaque_media".to_string()],
            scores: json!({}), reason: "ciphertext: exact-bytes layer only", source: source.as_str(),
            decided_by: "abraxas", asset_id: Some(req.asset_id), pial,
        }).await?;
        tx.commit().await?;
        return Ok(CheckResponse {
            verdict: "allow",
            banned_match: None,
            ownership: None,
            fingerprint_id: fp.id,
            reason: "ciphertext: exact-bytes layer only".into(),
        });
    }

    // 3. The worker's fingerprint.
    let wfp = match st.worker.fingerprint(&abs, kind.as_str()).await {
        Ok(f) => f,
        Err(e) => return worker_failed(st, &req, kind, fp.id, source, e).await,
    };
    if wfp.sha256 != req.sha256 {
        return Err(Failure::Mismatch(format!(
            "the file at {} hashes to {}, not the {} the caller sent",
            req.private_path, wfp.sha256, req.sha256
        )));
    }
    let segments = index::segments_of(wfp.phash, wfp.segments());
    let audio = wfp.audio.as_ref();
    let chroma: Option<Vec<u32>> = match audio.and_then(|a| a.chromaprint.as_deref()) {
        Some(c) => Some(crate::hashing::decode_chromaprint(c).map_err(|e| {
            Failure::Worker(WorkerError::Contract(format!("chromaprint does not decode: {e}")))
        })?),
        None => None,
    };
    let robust = audio.map(|a| robust_keys(&a.robust_landmarks)).unwrap_or_default();
    let version = match audio {
        Some(a) => Some(index::landmark_version_id(&st.pool, &a.landmark_version).await?),
        None => None,
    };
    let audio_query = match (audio, version) {
        (Some(a), Some(v)) => Some((v, a.landmarks.as_slice(), robust.as_slice())),
        _ => None,
    };

    // 4. Candidates, then the banned layers, then ownership.
    let candidates = index::all_candidates(&st.pool, &segments, audio_query, Some(fp.id), Some(req.asset_id)).await?;
    let outcome = banned::perceptual(&st.pool, &st.chroma, &segments, chroma.as_deref(), &candidates).await?;
    let owner = if kind.ownership_eligible() && !matches!(outcome, Outcome::Block(_)) {
        ownership::find(&st.pool, &st.legacy, &ownership::Query {
            fingerprint_id: fp.id,
            uploader_pial: req.uploader_pial,
            sha256: &req.sha256,
            duration: wfp.duration.or(req.duration),
            chroma: chroma.as_deref(),
            candidates: &candidates,
        })
        .await?
    } else {
        None
    };

    // 5. Record everything in one transaction.
    let mut tx = st.pool.begin().await?;
    let updated = sqlx::query(
        "UPDATE fingerprints
            SET phash = $2, chromaprint = $3, duration_seconds = COALESCE($4, duration_seconds),
                segment_count = $5, landmark_count = $6, robust_landmark_count = $7,
                landmark_version = $8, perceptual_at = NOW()
          WHERE id = $1 AND perceptual_at IS NULL",
    )
    .bind(fp.id)
    .bind(wfp.phash)
    .bind(audio.and_then(|a| a.chromaprint.clone()))
    .bind(wfp.duration)
    .bind(segments.len() as i32)
    .bind(audio.map(|a| a.landmarks.len() as i32).unwrap_or(0))
    .bind(robust.len() as i32)
    .bind(audio.map(|a| a.landmark_version.clone()))
    .execute(&mut *tx)
    .await?
    .rows_affected();
    // The index takes only media that can be an original; avatars, headers
    // and the like are gated, never credited against.
    if updated == 1 && kind.ownership_eligible() {
        index::write_segments(&mut tx, fp.id, Some(req.asset_id), &segments).await?;
        if let (Some(a), Some(v)) = (audio, version) {
            index::write_landmarks(&mut tx, req.asset_id, AudioKind::Classic, v, &a.landmarks).await?;
            index::write_landmarks(&mut tx, req.asset_id, AudioKind::Robust, v, &robust).await?;
        }
    }

    let (verdict, reason, banned_json, mut signals) = match &outcome {
        Outcome::Block(m) => {
            record_evidence(&mut tx, EvidenceRow {
                surface: "upload", subject_ref: &asset, asset_id: Some(req.asset_id), fingerprint_id: Some(fp.id),
                pial, media_kind: kind.as_str(), result: "flagged", exact: Some(false), matched: Some(m), sha256: Some(&req.sha256),
            }).await?;
            announce_banned(&mut tx, "asset", &asset, Some(req.asset_id), pial, Verdict::Block, m, false, &req.sha256).await?;
            (Verdict::Block, format!("banned registry: {} match ({})", m.layer, m.category), Some(m.json(false, &req.sha256)),
             vec![format!("banned_{}:{}", m.layer, m.category)])
        }
        Outcome::Near(m) => {
            record_evidence(&mut tx, EvidenceRow {
                surface: "upload", subject_ref: &asset, asset_id: Some(req.asset_id), fingerprint_id: Some(fp.id),
                pial, media_kind: kind.as_str(), result: "review", exact: Some(false), matched: Some(m), sha256: Some(&req.sha256),
            }).await?;
            announce_banned(&mut tx, "asset", &asset, Some(req.asset_id), pial, Verdict::HumanReview, m, false, &req.sha256).await?;
            // Suspected child sexual abuse material goes to a person at once:
            // no machine clears it, and the account consequence is a human call.
            let stage = if m.category == "csam" { Stage::AdminReview } else { Stage::MlReview };
            review::open(&mut tx, NewItem {
                item_kind: "banned_near", subject_kind: "asset", subject_id: &asset, ownership_match: None,
                banned_id: Some(m.banned_id), category: Some(&m.category), report_id: None,
                dedup_key: format!("banned_near:asset:{asset}"), stage,
                reason: &format!("near banned material: {} ({})", m.layer, m.category),
                evidence: json!({ "match": m.json(false, &req.sha256), "fingerprint_id": fp.id, "uploader_pial": req.uploader_pial }),
            }).await?;
            (Verdict::HumanReview, format!("near banned material: {} ({})", m.layer, m.category), Some(m.json(false, &req.sha256)),
             vec![format!("banned_{}_near:{}", m.layer, m.category)])
        }
        Outcome::Clear => (Verdict::Approve, "clear".to_string(), None, Vec::new()),
    };

    let mut ownership_json = None;
    if let Some(o) = &owner {
        let state = if o.level == Level::Credit { "credited" } else { "ml_review" };
        let match_id: Option<Uuid> = sqlx::query_scalar(
            "INSERT INTO ownership_matches
               (new_fingerprint, new_asset, new_pial, original_fingerprint, original_asset, original_pial,
                kind, layer, confidence, offset_seconds, offsets, state, decided_by, decided_at)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
                     CASE WHEN $12 = 'credited' THEN 'abraxas' END, CASE WHEN $12 = 'credited' THEN NOW() END)
             ON CONFLICT (new_fingerprint, original_fingerprint) DO NOTHING
             RETURNING id",
        )
        .bind(fp.id)
        .bind(req.asset_id)
        .bind(req.uploader_pial)
        .bind(o.original_fingerprint)
        .bind(o.original_asset)
        .bind(o.original_pial)
        .bind(o.kind)
        .bind(o.layer)
        .bind(o.confidence)
        .bind(o.offset_seconds)
        .bind(&o.offsets)
        .bind(state)
        .fetch_optional(&mut *tx)
        .await?;
        let body = json!({
            "match_id": match_id,
            "original_asset": o.original_asset,
            "original_pial": o.original_pial,
            "original_fingerprint": o.original_fingerprint,
            "kind": o.kind,
            "layer": o.layer,
            "confidence": o.confidence,
            "offset_seconds": o.offset_seconds,
            "state": state,
        });
        if let Some(mid) = match_id {
            if o.level == Level::Credit {
                announce_ownership(&mut tx, &asset, req.asset_id, pial, verdict, &body).await?;
            } else {
                review::open(&mut tx, NewItem {
                    item_kind: "ownership", subject_kind: "asset", subject_id: &asset, ownership_match: Some(mid),
                    banned_id: None, category: None, report_id: None, dedup_key: format!("ownership:{mid}"), stage: Stage::MlReview,
                    reason: &format!("possible {} of an earlier work ({})", o.kind, o.layer),
                    evidence: json!({ "match": body, "offsets": o.offsets }),
                }).await?;
            }
        }
        signals.push(format!("ownership_{}:{}", state, o.kind));
        ownership_json = Some(body);
    }

    verdicts::write(&mut tx, NewVerdict {
        subject_kind: "asset", subject_id: &asset, verdict, signals: &signals,
        scores: json!({}), reason: &reason, source: source.as_str(), decided_by: "abraxas",
        asset_id: Some(req.asset_id), pial,
    }).await?;
    crate::scan::rescan_for_asset(&mut tx, req.asset_id).await?;
    tx.commit().await?;
    jobs::wake(&st.jobs_wake);

    Ok(CheckResponse {
        verdict: match verdict {
            Verdict::Block => "block",
            Verdict::Approve | Verdict::AgeGate => "allow",
            Verdict::HumanReview | Verdict::Pending => "review",
        },
        banned_match: banned_json,
        ownership: ownership_json,
        fingerprint_id: fp.id,
        reason,
    })
}

pub async fn announce_ownership(
    tx: &mut Transaction<'_, Postgres>,
    asset: &str,
    asset_id: Uuid,
    pial: Option<Uuid>,
    verdict: Verdict,
    body: &Value,
) -> sqlx::Result<()> {
    let signals: Vec<String> = Vec::new();
    outbox::enqueue(
        tx,
        Event {
            kind: EventType::Ownership,
            subject_kind: "asset",
            subject_id: asset,
            asset_id: Some(asset_id),
            pial,
            verdict: verdict.as_str(),
            scores: json!({ "confidence": body["confidence"] }),
            signals: &signals,
            reason: "ownership credited to the original",
            ownership: Some(body.clone()),
            banned: None,
            review: None,
        },
    )
    .await
}

/// What the worker's failure means for this asset.
async fn worker_failed(
    st: &AppState,
    req: &CheckRequest,
    kind: Kind,
    fingerprint_id: Uuid,
    source: Source,
    e: WorkerError,
) -> Result<CheckResponse, Failure> {
    let asset = req.asset_id.to_string();
    let pial = Some(req.uploader_pial);
    if e.retryable() {
        if source == Source::Recheck {
            return Err(Failure::Worker(e));
        }
        // Check and backfill: record the asset as pending and queue the recheck.
        tracing::warn!(asset = %asset, error = %e, "worker could not fingerprint; asset pending, recheck queued");
        let mut tx = st.pool.begin().await?;
        record_evidence(&mut tx, EvidenceRow {
            surface: "upload", subject_ref: &asset, asset_id: Some(req.asset_id), fingerprint_id: Some(fingerprint_id),
            pial, media_kind: kind.as_str(), result: "pending_scan", exact: None, matched: None, sha256: Some(&req.sha256),
        }).await?;
        let reason = format!("not judged yet: {e}");
        verdicts::write(&mut tx, NewVerdict {
            subject_kind: "asset", subject_id: &asset, verdict: Verdict::Pending, signals: &["worker_unavailable".to_string()],
            scores: json!({}), reason: &reason, source: source.as_str(), decided_by: "abraxas",
            asset_id: Some(req.asset_id), pial,
        }).await?;
        let payload = serde_json::to_value(req).map_err(|e| Failure::Invalid(e.to_string()))?;
        jobs::enqueue(&mut tx, "asset_recheck", &asset, payload).await?;
        tx.commit().await?;
        jobs::wake(&st.jobs_wake);
        return Ok(CheckResponse {
            verdict: "review",
            banned_match: None,
            ownership: None,
            fingerprint_id,
            reason,
        });
    }
    // The worker read the request and cannot read the media. No retry will;
    // a person judges it.
    tracing::warn!(asset = %asset, error = %e, "worker rejected the media; a person judges it");
    let mut tx = st.pool.begin().await?;
    record_evidence(&mut tx, EvidenceRow {
        surface: "upload", subject_ref: &asset, asset_id: Some(req.asset_id), fingerprint_id: Some(fingerprint_id),
        pial, media_kind: kind.as_str(), result: "error", exact: None, matched: None, sha256: Some(&req.sha256),
    }).await?;
    review::open_unjudged(&mut tx, "asset", &asset, &e.to_string()).await?;
    tx.commit().await?;
    Ok(CheckResponse {
        verdict: "review",
        banned_match: None,
        ownership: None,
        fingerprint_id,
        reason: format!("media unreadable by the worker: {e}"),
    })
}

/// The answer already on record for an asset, or None when it is not final
/// enough to repeat (pending).
async fn stored_response(pool: &PgPool, asset_id: Uuid, fingerprint_id: Uuid) -> sqlx::Result<Option<CheckResponse>> {
    let asset = asset_id.to_string();
    let Some((v, _)) = verdicts::current(pool, "asset", &asset).await? else { return Ok(None) };
    if v == Verdict::Pending {
        return Ok(None);
    }
    let reason: String = sqlx::query_scalar(
        "SELECT reason FROM verdicts WHERE subject_kind = 'asset' AND subject_id = $1 ORDER BY id DESC LIMIT 1",
    )
    .bind(&asset)
    .fetch_one(pool)
    .await?;
    let banned: Option<(String, String, Option<String>)> = sqlx::query_as(
        "SELECT category, layer, match_kind FROM csam_evidence_log
          WHERE asset_id = $1 AND result IN ('flagged','review') ORDER BY recorded_at DESC LIMIT 1",
    )
    .bind(asset_id)
    .fetch_optional(pool)
    .await?;
    let owner: Option<(Uuid, Option<Uuid>, Option<Uuid>, String, f64, Option<f64>, String)> = sqlx::query_as(
        "SELECT id, original_asset, original_pial, kind, confidence, offset_seconds, state
           FROM ownership_matches WHERE new_fingerprint = $1 ORDER BY created_at DESC LIMIT 1",
    )
    .bind(fingerprint_id)
    .fetch_optional(pool)
    .await?;
    Ok(Some(CheckResponse {
        verdict: match v {
            Verdict::Block => "block",
            Verdict::Approve | Verdict::AgeGate => "allow",
            _ => "review",
        },
        banned_match: banned.map(|(c, l, m)| json!({ "category": c, "layer": l, "match": m })),
        ownership: owner.map(|(id, oa, op, k, c, off, s)| {
            json!({ "match_id": id, "original_asset": oa, "original_pial": op, "kind": k,
                    "confidence": c, "offset_seconds": off, "state": s })
        }),
        fingerprint_id,
        reason,
    }))
}

pub async fn check_asset(State(st): State<AppState>, Json(req): Json<CheckRequest>) -> ApiResult<Json<CheckResponse>> {
    Ok(Json(run(&st, req, Source::Check).await?))
}

/// The asset_recheck job: the same check, with the worker's failures retried.
pub async fn run_recheck(st: &AppState, _ref: &str, payload: &Value) -> Result<(), JobError> {
    let req: CheckRequest = serde_json::from_value(payload.clone())
        .map_err(|e| JobError::Escalate(format!("recheck payload does not parse: {e}")))?;
    match run(st, req, Source::Recheck).await {
        Ok(_) => Ok(()),
        Err(Failure::Db(e)) => Err(JobError::Retry(format!("database: {e}"))),
        Err(Failure::Worker(e)) => Err(JobError::Retry(e.to_string())),
        Err(Failure::Invalid(s)) | Err(Failure::Conflict(s)) | Err(Failure::Mismatch(s)) => Err(JobError::Escalate(s)),
    }
}

pub async fn asset_verdict(State(st): State<AppState>, AxPath(asset_id): AxPath<Uuid>) -> ApiResult<Json<Value>> {
    let fp: Option<Uuid> = sqlx::query_scalar("SELECT id FROM fingerprints WHERE asset_id = $1")
        .bind(asset_id)
        .fetch_optional(&st.pool)
        .await?;
    let Some(fp) = fp else { return Err(ApiError::not_found("asset has never been checked")) };
    let current = verdicts::current_pool(&st.pool, "asset", &asset_id.to_string()).await?;
    let resp = stored_response(&st.pool, asset_id, fp).await?;
    Ok(Json(json!({
        "asset_id": asset_id,
        "fingerprint_id": fp,
        "verdict": current.as_ref().map(|(v, _)| v.as_str()),
        "signals": current.map(|(_, s)| s).unwrap_or_default(),
        "check": resp,
    })))
}

// ── live frames ──────────────────────────────────────────────────────────────

/// `POST /v1/check/frame` — one sampled live keyframe.
///
/// ```text
///   request  {stream_id, frame_name, pial?, sha256?, is_adult_creator?}
///            the frame is read from LIVE_SCAN_DIR/<stream_id>/<frame_name>;
///            sha256, when sent, must match the bytes on disk. (An image_b64
///            field is accepted and ignored: the volume is the one source.)
///   response {verdict: allow|age_gate|review|block, risk_level:
///             clean|age_gate|review|block, signals, scores, sha256,
///             banned_match?: {category, layer, sha256, match, ...}}
/// ```
///
/// Fail closed: when the worker cannot judge the frame the answer is 503
/// `worker_unavailable`, which the live lane holds as unjudged — never a clean
/// frame. A frame the worker cannot decode is `review`. Each frame is one row
/// in the history of subject `stream:<stream_id>`.
#[derive(Debug, Deserialize)]
pub struct FrameRequest {
    pub stream_id: String,
    pub frame_name: String,
    #[serde(default)]
    pub pial: Option<Uuid>,
    #[serde(default)]
    pub sha256: Option<String>,
    #[serde(default)]
    pub is_adult_creator: bool,
}

const MAX_FRAME_BYTES: u64 = 8 << 20;

fn frame_answer(v: Verdict) -> (&'static str, &'static str) {
    match v {
        Verdict::Approve => ("allow", "clean"),
        Verdict::AgeGate => ("age_gate", "age_gate"),
        Verdict::HumanReview | Verdict::Pending => ("review", "review"),
        Verdict::Block => ("block", "block"),
    }
}

pub async fn check_frame(State(st): State<AppState>, Json(req): Json<FrameRequest>) -> ApiResult<Json<Value>> {
    let abs = media::live_frame_abs(&st.cfg.live_scan_dir, &req.stream_id, &req.frame_name).map_err(ApiError::bad_request)?;
    let meta = tokio::fs::metadata(&abs)
        .await
        .map_err(|e| ApiError::not_found(format!("frame {}: {e}", abs.display())))?;
    if meta.len() == 0 || meta.len() > MAX_FRAME_BYTES {
        return Err(ApiError::bad_request(format!("frame is {} bytes; a keyframe is 1..{MAX_FRAME_BYTES}", meta.len())));
    }
    let bytes = tokio::fs::read(&abs).await.map_err(|e| ApiError::not_found(format!("frame {}: {e}", abs.display())))?;
    let sha = hex::encode(Sha256::digest(&bytes));
    if let Some(claimed) = req.sha256.as_deref().map(|s| s.trim().to_ascii_lowercase()) {
        if !claimed.is_empty() && claimed != sha {
            return Err(ApiError::new(StatusCode::UNPROCESSABLE_ENTITY, "sha256_mismatch",
                format!("the frame on disk hashes to {sha}, not {claimed}")));
        }
    }
    let stream = req.stream_id.as_str();
    let frame = req.frame_name.as_str();

    // Exact bytes first; a database failure is a 503 (unjudged), never clean.
    if let Some(m) = banned::sha256(&st.pool, &sha).await? {
        let signals = vec![format!("banned_sha256:{}", m.category), format!("frame:{frame}")];
        let mut tx = st.pool.begin().await?;
        frame_banned(&mut tx, stream, frame, req.pial, &sha, &m, true, Verdict::Block).await?;
        verdicts::write(&mut tx, NewVerdict {
            subject_kind: "stream", subject_id: stream, verdict: Verdict::Block, signals: &signals, scores: json!({}),
            reason: "banned registry: exact bytes", source: "frame", decided_by: "abraxas", asset_id: None, pial: req.pial,
        }).await?;
        tx.commit().await?;
        return Ok(Json(json!({
            "verdict": "block", "risk_level": "block", "signals": signals, "scores": {},
            "sha256": sha, "banned_match": m.json(true, &sha),
        })));
    }

    let (fp_result, analysis_result) = tokio::join!(
        st.worker.fingerprint(&abs, "image"),
        st.worker.analyze_media(&abs, "image", false)
    );
    let (wfp, analysis) = match (fp_result, analysis_result) {
        (Ok(f), Ok(a)) => (f, a),
        (Err(e), _) | (_, Err(e)) if e.retryable() => {
            return Err(ApiError::new(StatusCode::SERVICE_UNAVAILABLE, "worker_unavailable", e.to_string()))
        }
        (Err(e), _) | (_, Err(e)) => {
            let reason = format!("frame {frame} unreadable: {e}");
            let signals = vec!["frame_unreadable".to_string(), format!("frame:{frame}")];
            let mut tx = st.pool.begin().await?;
            verdicts::write(&mut tx, NewVerdict {
                subject_kind: "stream", subject_id: stream, verdict: Verdict::HumanReview, signals: &signals,
                scores: json!({}), reason: &reason, source: "frame", decided_by: "abraxas", asset_id: None, pial: req.pial,
            }).await?;
            tx.commit().await?;
            return Ok(Json(json!({ "verdict": "review", "risk_level": "review", "signals": signals, "scores": {}, "sha256": sha })));
        }
    };

    let segments = index::segments_of(wfp.phash, &[]);
    let candidates = index::visual_candidates(&st.pool, &segments, None).await?;
    let outcome = banned::perceptual(&st.pool, &st.chroma, &segments, None, &candidates).await?;

    let suppressed = st.suppression.get(&st.pool).await?;
    let behaviour = crate::policy::Behaviour::default();
    let media = vec![(frame.to_string(), analysis.clone())];
    let decision = crate::policy::decide(&crate::policy::Inputs {
        text: None,
        media: &media,
        opaque_media: 0,
        asset_verdicts: &[],
        behaviour: &behaviour,
        is_adult_creator: req.is_adult_creator,
        unresolved_media: &[],
        suppressed: &suppressed,
    });

    let mut tx = st.pool.begin().await?;
    sqlx::query(
        "INSERT INTO scan_results (subject_kind, subject_id, deep, scan_version, scores, labels, signals)
         VALUES ('stream', $1, FALSE, $2, $3, $4, $5)",
    )
    .bind(stream)
    .bind(&st.cfg.scan_version)
    .bind(json!({ "frame": frame, "decision": decision.scores, "analysis": analysis }))
    .bind(&analysis.labels)
    .bind(&decision.signals)
    .execute(&mut *tx)
    .await?;

    let (verdict, banned_json, mut signals, reason) = match &outcome {
        Outcome::Block(m) => {
            frame_banned(&mut tx, stream, frame, req.pial, &sha, m, false, Verdict::Block).await?;
            (Verdict::Block, Some(m.json(false, &sha)), vec![format!("banned_{}:{}", m.layer, m.category)],
             format!("banned registry: {} ({})", m.layer, m.category))
        }
        Outcome::Near(m) => {
            frame_banned(&mut tx, stream, frame, req.pial, &sha, m, false, Verdict::HumanReview).await?;
            let v = decision.verdict.max(Verdict::HumanReview);
            (v, Some(m.json(false, &sha)), vec![format!("banned_{}_near:{}", m.layer, m.category)],
             format!("near banned material: {} ({})", m.layer, m.category))
        }
        Outcome::Clear => (decision.verdict, None, Vec::new(), decision.reason.clone()),
    };
    signals.extend(decision.signals.iter().cloned());
    signals.push(format!("frame:{frame}"));
    verdicts::write(&mut tx, NewVerdict {
        subject_kind: "stream", subject_id: stream, verdict, signals: &signals,
        scores: decision.scores.clone(), reason: &reason, source: "frame", decided_by: "abraxas",
        asset_id: None, pial: req.pial,
    }).await?;
    tx.commit().await?;

    let (answer, risk) = frame_answer(verdict);
    let mut body = json!({
        "verdict": answer,
        "risk_level": risk,
        "signals": signals,
        "scores": decision.scores,
        "sha256": sha,
        "reason": reason,
    });
    if let Some(b) = banned_json {
        body["banned_match"] = b;
    }
    Ok(Json(body))
}

#[allow(clippy::too_many_arguments)]
async fn frame_banned(
    tx: &mut Transaction<'_, Postgres>,
    stream: &str,
    frame: &str,
    pial: Option<Uuid>,
    sha: &str,
    m: &BannedMatch,
    exact: bool,
    verdict: Verdict,
) -> sqlx::Result<()> {
    let subject_ref = format!("{stream}/{frame}");
    record_evidence(tx, EvidenceRow {
        surface: "live", subject_ref: &subject_ref, asset_id: None, fingerprint_id: None, pial, media_kind: "frame",
        result: if verdict == Verdict::Block { "flagged" } else { "review" }, exact: Some(exact), matched: Some(m), sha256: Some(sha),
    }).await?;
    announce_banned(tx, "stream", stream, None, pial, verdict, m, exact, sha).await
}
