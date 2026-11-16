//! Internal admin routes: the banned registry, and read views of ownership and
//! verdicts for the admin Facets feed-engine renders.
//!
//! ```text
//!   POST /v1/banned            {hash_type: sha256|phash|chromaprint|audio_fp,
//!                               hash_value, category, note?, added_by}
//!        → 201 {id, layer, category, created: true} | 200 {..., created: false}
//!   POST /v1/banned/from-asset {asset_id, category, note?, added_by}
//!        → {asset_id, banned: [{id, layer}]}   ban by example, every layer
//!   POST /v1/banned/revoke     {hash_type, hash_value, revoked_by, reason}
//!   POST /v1/banned/{id}/revoke {revoked_by, reason}
//!   GET  /v1/banned?category=&limit=&before=
//!   GET  /v1/ownership/pial/{pial}   credit owed to and taken by one PIAL
//!   GET  /v1/verdicts/{kind}/{id}    a subject's verdict history
//! ```
//!
//! A ban is never deleted: removing one REVOKES it (who, when, why), and the
//! row stays as the record that the ban existed.

use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::Json;
use chrono::{DateTime, Utc};
use serde::Deserialize;
use serde_json::{json, Value};
use uuid::Uuid;

use crate::api::{ApiError, ApiResult, AppState};
use crate::hashing::{decode_chromaprint, parse_phash_hex, phash_hex};
use crate::verdicts::{self, NewVerdict, Verdict};

fn category_ok(c: &str) -> bool {
    c.len() >= 2 && c.len() <= 32 && c.starts_with(|x: char| x.is_ascii_lowercase()) && c.chars().all(|x| x.is_ascii_lowercase() || x == '_')
}

/// A hash value in the column its layer stores it in.
enum HashValue {
    Sha(String),
    Phash(i64),
    Chroma(String),
}

fn parse_value(hash_type: &str, raw: &str) -> ApiResult<(&'static str, HashValue)> {
    let raw = raw.trim();
    match hash_type {
        "sha256" => {
            let v = raw.to_ascii_lowercase();
            if !crate::media::is_sha256_hex(&v) {
                return Err(ApiError::bad_request("a sha256 is 64 hex digits"));
            }
            Ok(("sha256", HashValue::Sha(v)))
        }
        "phash" => parse_phash_hex(raw)
            .map(|h| ("phash", HashValue::Phash(h)))
            .ok_or_else(|| ApiError::bad_request("a phash is 16 hex digits")),
        // audio_fp is feed-engine's name for the chromaprint layer.
        "chromaprint" | "audio_fp" => {
            decode_chromaprint(raw).map_err(|e| ApiError::bad_request(format!("chromaprint does not decode: {e}")))?;
            Ok(("chromaprint", HashValue::Chroma(raw.to_string())))
        }
        _ => Err(ApiError::bad_request("hash_type must be sha256, phash, chromaprint or audio_fp")),
    }
}

#[derive(Debug, Deserialize)]
pub struct AddBanned {
    pub hash_type: String,
    pub hash_value: String,
    pub category: String,
    #[serde(default)]
    pub note: Option<String>,
    pub added_by: String,
}

pub async fn add_banned(State(st): State<AppState>, Json(req): Json<AddBanned>) -> ApiResult<(StatusCode, Json<Value>)> {
    if !category_ok(&req.category) {
        return Err(ApiError::bad_request("category must be a lowercase word ([a-z_], 2-32)"));
    }
    if req.added_by.trim().is_empty() {
        return Err(ApiError::bad_request("added_by must name who is adding the ban"));
    }
    let (layer, v) = parse_value(&req.hash_type, &req.hash_value)?;
    let (sha, ph, ch) = match &v {
        HashValue::Sha(s) => (Some(s.clone()), None, None),
        HashValue::Phash(h) => (None, Some(*h), None),
        HashValue::Chroma(c) => (None, None, Some(c.clone())),
    };
    let inserted: Option<Uuid> = sqlx::query_scalar(
        "INSERT INTO banned_hashes (layer, sha256, phash, chromaprint, category, added_by, note)
         VALUES ($1, $2, $3, $4, $5, $6, $7)
         ON CONFLICT DO NOTHING RETURNING id",
    )
    .bind(layer)
    .bind(&sha)
    .bind(ph)
    .bind(&ch)
    .bind(&req.category)
    .bind(req.added_by.trim())
    .bind(req.note.clone().unwrap_or_default())
    .fetch_optional(&st.pool)
    .await?;
    if let Some(id) = inserted {
        return Ok((StatusCode::CREATED, Json(json!({ "id": id, "layer": layer, "category": req.category, "created": true }))));
    }
    let (id, category): (Uuid, String) = sqlx::query_as(
        "SELECT id, category FROM banned_hashes
          WHERE layer = $1 AND revoked_at IS NULL
            AND (sha256 = $2 OR phash = $3 OR chromaprint = $4)",
    )
    .bind(layer)
    .bind(&sha)
    .bind(ph)
    .bind(&ch)
    .fetch_one(&st.pool)
    .await?;
    Ok((StatusCode::OK, Json(json!({ "id": id, "layer": layer, "category": category, "created": false }))))
}

#[derive(Debug, Deserialize)]
pub struct BanFromAsset {
    pub asset_id: Uuid,
    pub category: String,
    #[serde(default)]
    pub note: Option<String>,
    pub added_by: String,
}

/// Bans an asset's material on every layer its fingerprint has: its exact
/// bytes, its image phash, its chromaprint, and its index entries (segments
/// and landmarks) — so re-encodes, excerpts and speed changes of it are found
/// by the same alignment that finds ownership. The asset itself is blocked.
pub async fn ban_from_asset(State(st): State<AppState>, Json(req): Json<BanFromAsset>) -> ApiResult<Json<Value>> {
    if !category_ok(&req.category) {
        return Err(ApiError::bad_request("category must be a lowercase word ([a-z_], 2-32)"));
    }
    let by = req.added_by.trim();
    if by.is_empty() {
        return Err(ApiError::bad_request("added_by must name who is adding the ban"));
    }
    let fp: Option<(Uuid, Option<String>, Option<i64>, Option<String>, i32, i32, Option<Uuid>)> = sqlx::query_as(
        "SELECT id, sha256, phash, chromaprint, segment_count, landmark_count + robust_landmark_count, uploader_pial
           FROM fingerprints WHERE asset_id = $1",
    )
    .bind(req.asset_id)
    .fetch_optional(&st.pool)
    .await?;
    let Some((fp_id, sha, phash, chroma, segments, landmarks, pial)) = fp else {
        return Err(ApiError::not_found("asset has never been checked"));
    };
    let note = req.note.clone().unwrap_or_default();
    let mut tx = st.pool.begin().await?;
    let mut added = Vec::new();
    let mut layers: Vec<(&str, Option<String>, Option<i64>, Option<String>, Option<Uuid>)> = Vec::new();
    if let Some(s) = sha.clone() {
        layers.push(("sha256", Some(s), None, None, None));
    }
    if let Some(h) = phash {
        layers.push(("phash", None, Some(h), None, None));
    }
    if let Some(c) = chroma {
        layers.push(("chromaprint", None, None, Some(c), None));
    }
    if segments > 0 {
        layers.push(("video_segments", None, None, None, Some(fp_id)));
    }
    if landmarks > 0 {
        layers.push(("audio_landmarks", None, None, None, Some(fp_id)));
    }
    for (layer, s, h, c, f) in layers {
        let id: Option<Uuid> = sqlx::query_scalar(
            "INSERT INTO banned_hashes (layer, sha256, phash, chromaprint, fingerprint_id, category, added_by, note)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING RETURNING id",
        )
        .bind(layer)
        .bind(&s)
        .bind(h)
        .bind(&c)
        .bind(f)
        .bind(&req.category)
        .bind(by)
        .bind(&note)
        .fetch_optional(&mut *tx)
        .await?;
        if let Some(id) = id {
            added.push(json!({ "id": id, "layer": layer }));
        }
    }
    let asset = req.asset_id.to_string();
    let signals = vec![format!("banned_by_example:{}", req.category)];
    verdicts::write(&mut tx, NewVerdict {
        subject_kind: "asset",
        subject_id: &asset,
        verdict: Verdict::Block,
        signals: &signals,
        scores: json!({}),
        reason: &format!("banned by {by}: {note}"),
        source: "admin",
        decided_by: by,
        asset_id: Some(req.asset_id),
        pial,
    })
    .await?;
    crate::scan::rescan_for_asset(&mut tx, req.asset_id).await?;
    tx.commit().await?;
    crate::jobs::wake(&st.jobs_wake);
    Ok(Json(json!({ "asset_id": req.asset_id, "fingerprint_id": fp_id, "banned": added })))
}

#[derive(Debug, Deserialize)]
pub struct Revoke {
    #[serde(default)]
    pub hash_type: Option<String>,
    #[serde(default)]
    pub hash_value: Option<String>,
    pub revoked_by: String,
    pub reason: String,
}

async fn revoke_where(st: &AppState, id: Option<Uuid>, req: &Revoke) -> ApiResult<Json<Value>> {
    if req.revoked_by.trim().is_empty() || req.reason.trim().is_empty() {
        return Err(ApiError::bad_request("revoked_by and reason are required"));
    }
    let (sha, ph, ch, layer) = match id {
        Some(_) => (None, None, None, None),
        None => {
            let (t, v) = match (&req.hash_type, &req.hash_value) {
                (Some(t), Some(v)) => (t, v),
                _ => return Err(ApiError::bad_request("hash_type and hash_value are required")),
            };
            let (layer, v) = parse_value(t, v)?;
            match v {
                HashValue::Sha(s) => (Some(s), None, None, Some(layer)),
                HashValue::Phash(h) => (None, Some(h), None, Some(layer)),
                HashValue::Chroma(c) => (None, None, Some(c), Some(layer)),
            }
        }
    };
    let rows: Vec<Uuid> = sqlx::query_scalar(
        "UPDATE banned_hashes SET revoked_at = NOW(), revoked_by = $6, revoked_reason = $7
          WHERE revoked_at IS NULL
            AND (($1::uuid IS NOT NULL AND id = $1)
              OR ($1::uuid IS NULL AND layer = $2 AND (sha256 = $3 OR phash = $4 OR chromaprint = $5)))
          RETURNING id",
    )
    .bind(id)
    .bind(layer)
    .bind(&sha)
    .bind(ph)
    .bind(&ch)
    .bind(req.revoked_by.trim())
    .bind(req.reason.trim())
    .fetch_all(&st.pool)
    .await?;
    if rows.is_empty() {
        return Err(ApiError::not_found("no active ban matches"));
    }
    Ok(Json(json!({ "revoked": rows })))
}

pub async fn revoke_banned(State(st): State<AppState>, Path(id): Path<Uuid>, Json(req): Json<Revoke>) -> ApiResult<Json<Value>> {
    revoke_where(&st, Some(id), &req).await
}

pub async fn revoke_banned_value(State(st): State<AppState>, Json(req): Json<Revoke>) -> ApiResult<Json<Value>> {
    revoke_where(&st, None, &req).await
}

#[derive(Debug, Deserialize)]
pub struct BannedQuery {
    #[serde(default)]
    pub category: Option<String>,
    #[serde(default)]
    pub before: Option<DateTime<Utc>>,
    #[serde(default)]
    pub limit: Option<i64>,
    #[serde(default)]
    pub include_revoked: bool,
}

pub async fn list_banned(State(st): State<AppState>, Query(q): Query<BannedQuery>) -> ApiResult<Json<Value>> {
    #[allow(clippy::type_complexity)]
    let rows: Vec<(Uuid, String, Option<String>, Option<i64>, Option<String>, Option<Uuid>, String, String, String, String, DateTime<Utc>, Option<DateTime<Utc>>, Option<String>)> =
        sqlx::query_as(
            "SELECT id, layer, sha256, phash, chromaprint, fingerprint_id, category, added_by, note, origin, added_at, revoked_at, revoked_by
               FROM banned_hashes
              WHERE ($1::text IS NULL OR category = $1) AND ($2::timestamptz IS NULL OR added_at < $2)
                AND ($3 OR revoked_at IS NULL)
              ORDER BY added_at DESC LIMIT $4",
        )
        .bind(&q.category)
        .bind(q.before)
        .bind(q.include_revoked)
        .bind(q.limit.unwrap_or(100).clamp(1, 500))
        .fetch_all(&st.pool)
        .await?;
    let items: Vec<Value> = rows
        .into_iter()
        .map(|(id, layer, sha, ph, ch, fp, cat, by, note, origin, at, rev, rev_by)| {
            let value = sha.or(ph.map(phash_hex)).or(ch.map(|c| c.chars().take(32).collect::<String>() + "…"));
            json!({ "id": id, "layer": layer, "hash_value": value, "fingerprint_id": fp, "category": cat,
                    "added_by": by, "note": note, "origin": origin, "added_at": at,
                    "revoked_at": rev, "revoked_by": rev_by })
        })
        .collect();
    Ok(Json(json!({ "banned": items, "count": items.len() })))
}

pub async fn ownership_of_pial(State(st): State<AppState>, Path(pial): Path<Uuid>) -> ApiResult<Json<Value>> {
    #[allow(clippy::type_complexity)]
    let q = |col: &'static str| {
        format!(
            "SELECT id, new_asset, new_pial, original_asset, original_pial, kind, layer, confidence, offset_seconds, state, created_at
               FROM ownership_matches WHERE {col} = $1 ORDER BY created_at DESC LIMIT 500"
        )
    };
    type Row = (Uuid, Option<Uuid>, Option<Uuid>, Option<Uuid>, Option<Uuid>, String, String, f64, Option<f64>, String, DateTime<Utc>);
    let to_json = |r: Row| {
        json!({ "match_id": r.0, "new_asset": r.1, "new_pial": r.2, "original_asset": r.3, "original_pial": r.4,
                "kind": r.5, "layer": r.6, "confidence": r.7, "offset_seconds": r.8, "state": r.9, "created_at": r.10 })
    };
    let owed: Vec<Row> = sqlx::query_as(&q("original_pial")).bind(pial).fetch_all(&st.pool).await?;
    let taken: Vec<Row> = sqlx::query_as(&q("new_pial")).bind(pial).fetch_all(&st.pool).await?;
    Ok(Json(json!({
        "pial": pial,
        "credited_to": owed.into_iter().map(to_json).collect::<Vec<_>>(),
        "matched_against": taken.into_iter().map(to_json).collect::<Vec<_>>(),
    })))
}

pub async fn subject_verdicts(State(st): State<AppState>, Path((kind, id)): Path<(String, String)>) -> ApiResult<Json<Value>> {
    if !matches!(kind.as_str(), "work" | "vision" | "asset" | "stream") {
        return Err(ApiError::bad_request("kind must be work, vision, asset or stream"));
    }
    let rows: Vec<(String, Vec<String>, Value, String, String, String, DateTime<Utc>)> = sqlx::query_as(
        "SELECT verdict, signals, scores, reason, source, decided_by, created_at FROM verdicts
          WHERE subject_kind = $1 AND subject_id = $2 ORDER BY id DESC LIMIT 100",
    )
    .bind(&kind)
    .bind(&id)
    .fetch_all(&st.pool)
    .await?;
    let history: Vec<Value> = rows
        .into_iter()
        .map(|(v, s, sc, r, src, by, at)| json!({ "verdict": v, "signals": s, "scores": sc, "reason": r, "source": src, "decided_by": by, "at": at }))
        .collect();
    Ok(Json(json!({ "subject": { "kind": kind, "id": id }, "current": history.first().cloned(), "history": history })))
}
