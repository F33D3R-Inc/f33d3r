//! Ownership: who first put this material on the platform.
//!
//! On a duplicate, derivative or excerpt match, the ORIGINAL owner — the PIAL
//! behind the earliest fingerprint that matches — is credited. Credit is
//! keyed by PIAL and survives the original work's deletion and its owner's
//! erasure, because fingerprints do.
//!
//!   exact       identical bytes (sha256).
//!   derivative  the whole work, transformed: re-encoded, resized, cropped,
//!               sped up or slowed, pitch-shifted, music mixed under it.
//!   excerpt     part of one inside the other: a clip lifted from a long
//!               video, a sample inside a track, a trimmed re-upload.
//!
//! The cuts here are STRICTER than the banned gate's. An ownership search
//! draws against every fingerprint on the platform, so the null distribution
//! is sampled millions of times per upload, and a false credit takes a
//! creator's work away from them in public. Anything between "certainly the
//! same" and "certainly not" is not decided here: it is opened as an
//! ownership review, which the worker's deep pass (and then a person) settles.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use chrono::{DateTime, Utc};
use serde_json::{json, Value};
use sqlx::PgPool;
use tokio::sync::RwLock;
use uuid::Uuid;

use crate::align::Alignment;
use crate::hashing::{chroma_similarity, chroma_usable, decode_chromaprint};
use crate::index::{AudioKind, Candidate, Evidence};

// ── Cuts ─────────────────────────────────────────────────────────────────────

/// Image phash: 4 bits covers every measured re-encode and resize (2 bits)
/// with room; 5..=10 is where crops, filters, overlays and stickers land, and
/// also where low-detail images (flat colour, near-black) collide with each
/// other — exactly the band a person must look at, not a machine credit.
pub const IMAGE_CREDIT: u32 = 4;
pub const IMAGE_REVIEW: u32 = 10;
/// Video: five shots in sequence over ten seconds, most of the shots in that
/// stretch explained, is the same footage. Two shots over three seconds is a
/// question: a stock intro, a meme template, a shared news clip.
pub const VIDEO_CREDIT_ALIGNED: usize = 5;
pub const VIDEO_CREDIT_SPAN: f64 = 10.0;
pub const VIDEO_CREDIT_DENSITY: f64 = 0.30;
pub const VIDEO_REVIEW_ALIGNED: usize = 2;
pub const VIDEO_REVIEW_SPAN: f64 = 3.0;
/// Classic audio landmarks (~100 per second from the worker). A clean copy
/// lines up 30–60% of them; music mixed over it at equal loudness, 5–15%.
/// Credit needs 40 distinct landmarks on one 200 ms offset window over at
/// least 5 s, and at least 5% of the query's landmarks in that stretch —
/// the density bar is what stops a long upload that merely shares a sound
/// effect with a work from being credited to it. Review starts at 15 over 3 s.
pub const AUDIO_CREDIT_ALIGNED: usize = 40;
pub const AUDIO_CREDIT_SPAN: f64 = 5.0;
pub const AUDIO_CREDIT_DENSITY: f64 = 0.05;
pub const AUDIO_REVIEW_ALIGNED: usize = 15;
pub const AUDIO_REVIEW_SPAN: f64 = 3.0;
/// Robust compound keys: fewer survive a transform (a key needs two triples)
/// and the alignment searches ~230 time scales, so the best spurious line is
/// the best of 230 draws. Credit needs 40 on one offset AND one scale with 3%
/// density; review starts at 20.
pub const ROBUST_CREDIT_ALIGNED: usize = 40;
pub const ROBUST_CREDIT_DENSITY: f64 = 0.03;
pub const ROBUST_REVIEW_ALIGNED: usize = 20;
/// Legacy chromaprint fingerprints (imported, frozen): the duplicate cuts
/// content-scan measured — every re-encode >= 0.90, the different-work
/// maximum 0.7886, derived audio (noise mixed in, 2% tempo) 0.82–0.90.
pub const CHROMA_CREDIT: f64 = 0.90;
pub const CHROMA_REVIEW: f64 = 0.82;
/// Coverage on both sides at which an aligned match is the whole work
/// (derivative) rather than part of one (excerpt).
pub const WHOLE_WORK_COVERAGE: f64 = 0.80;

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Level {
    Review,
    Credit,
}

#[derive(Debug, Clone)]
pub struct Match {
    pub level: Level,
    pub original_fingerprint: Uuid,
    pub original_asset: Option<Uuid>,
    pub original_pial: Option<Uuid>,
    pub original_created: DateTime<Utc>,
    pub kind: &'static str,
    pub layer: &'static str,
    pub confidence: f64,
    pub offset_seconds: Option<f64>,
    pub offsets: Value,
}

pub fn alignment_json(a: &Alignment, density: Option<f64>) -> Value {
    json!({
        "offset_seconds": a.offset,
        "scale": a.scale,
        "aligned": a.aligned,
        "density": density,
        "new_start": a.q_start,
        "new_end": a.q_end,
        "original_start": a.r_start,
        "original_end": a.r_end,
    })
}

struct Meta {
    asset: Option<Uuid>,
    pial: Option<Uuid>,
    created: DateTime<Utc>,
    duration: Option<f64>,
}

async fn metadata(pool: &PgPool, ids: &[Uuid]) -> sqlx::Result<HashMap<Uuid, Meta>> {
    // Only media that can be an original: ownership-eligible and not itself
    // banned material.
    let rows: Vec<(Uuid, Option<Uuid>, Option<Uuid>, DateTime<Utc>, Option<f64>)> = sqlx::query_as(
        "SELECT f.id, f.asset_id, f.uploader_pial, f.created_at, f.duration_seconds
           FROM fingerprints f
          WHERE f.id = ANY($1) AND f.ownership_eligible
            AND NOT EXISTS (SELECT 1 FROM banned_hashes b
                             WHERE b.fingerprint_id = f.id AND b.revoked_at IS NULL)",
    )
    .bind(ids)
    .fetch_all(pool)
    .await?;
    Ok(rows
        .into_iter()
        .map(|(id, asset, pial, created, duration)| (id, Meta { asset, pial, created, duration }))
        .collect())
}

fn kind_of(a: &Alignment, robust: bool, new_duration: Option<f64>, orig_duration: Option<f64>) -> &'static str {
    if robust || a.rescaled() {
        return "derivative";
    }
    match (new_duration, orig_duration) {
        (Some(n), Some(o)) if n > 0.0 && o > 0.0 => {
            if a.q_span() / n >= WHOLE_WORK_COVERAGE && a.r_span() / o >= WHOLE_WORK_COVERAGE {
                "derivative"
            } else {
                "excerpt"
            }
        }
        // Without both lengths coverage cannot be measured; claiming "excerpt"
        // would assert a fact nobody measured.
        _ => "derivative",
    }
}

fn evaluate(e: &Evidence, new_duration: Option<f64>, orig_duration: Option<f64>) -> Option<(Level, &'static str, &'static str, f64, Option<f64>, Value)> {
    match e {
        Evidence::Image { distance, reference_t } => {
            let level = if *distance <= IMAGE_CREDIT {
                Level::Credit
            } else if *distance <= IMAGE_REVIEW {
                Level::Review
            } else {
                return None;
            };
            let conf = 1.0 - *distance as f64 / 32.0;
            Some((level, "derivative", "phash", conf, None, json!({ "distance": distance, "original_t": reference_t })))
        }
        Evidence::Video { alignment: a, density, .. } => {
            let level = if a.aligned >= VIDEO_CREDIT_ALIGNED && a.q_span() >= VIDEO_CREDIT_SPAN && *density >= VIDEO_CREDIT_DENSITY {
                Level::Credit
            } else if a.aligned >= VIDEO_REVIEW_ALIGNED && a.q_span() >= VIDEO_REVIEW_SPAN {
                Level::Review
            } else {
                return None;
            };
            let conf = 0.6 * (a.aligned as f64 / (2 * VIDEO_CREDIT_ALIGNED) as f64).min(1.0) + 0.4 * density.min(1.0);
            Some((level, kind_of(a, false, new_duration, orig_duration), "video_segments", conf, Some(a.offset), alignment_json(a, Some(*density))))
        }
        Evidence::Audio { kind, alignment: a, density } => {
            let robust = *kind == AudioKind::Robust;
            let (credit, credit_density, review) = if robust {
                (ROBUST_CREDIT_ALIGNED, ROBUST_CREDIT_DENSITY, ROBUST_REVIEW_ALIGNED)
            } else {
                (AUDIO_CREDIT_ALIGNED, AUDIO_CREDIT_DENSITY, AUDIO_REVIEW_ALIGNED)
            };
            let level = if a.aligned >= credit && a.q_span() >= AUDIO_CREDIT_SPAN && *density >= credit_density {
                Level::Credit
            } else if a.aligned >= review && a.q_span() >= AUDIO_REVIEW_SPAN {
                Level::Review
            } else {
                return None;
            };
            let conf = 0.6 * (a.aligned as f64 / (2 * credit) as f64).min(1.0) + 0.4 * (density / 0.25).min(1.0);
            let mut detail = alignment_json(a, Some(*density));
            detail["robust"] = json!(robust);
            Some((level, kind_of(a, robust, new_duration, orig_duration), "audio_landmarks", conf, Some(a.offset), detail))
        }
    }
}

// ── legacy chromaprint set ───────────────────────────────────────────────────

struct LegacyEntry {
    fingerprint_id: Uuid,
    frames: Vec<u32>,
}

/// Imported fingerprints that carry a chromaprint and no landmarks (a backfill
/// that re-fingerprints one moves it into the index and out of this set). The
/// set only ever shrinks — nothing new enters it — so a linear scan over it is a
/// bounded, known cost; it is reloaded every ten minutes so a completed
/// import is picked up without a restart.
#[derive(Clone, Default)]
pub struct LegacyAudio {
    inner: Arc<RwLock<Option<(Instant, Arc<Vec<LegacyEntry>>)>>>,
}

impl LegacyAudio {
    async fn entries(&self, pool: &PgPool) -> sqlx::Result<Arc<Vec<LegacyEntry>>> {
        if let Some((at, list)) = self.inner.read().await.as_ref() {
            if at.elapsed() < Duration::from_secs(600) {
                return Ok(list.clone());
            }
        }
        let rows: Vec<(Uuid, String)> = sqlx::query_as(
            "SELECT id, chromaprint FROM fingerprints
              WHERE origin = 'import' AND chromaprint IS NOT NULL AND ownership_eligible
                AND perceptual_at IS NULL",
        )
        .fetch_all(pool)
        .await?;
        let mut list = Vec::with_capacity(rows.len());
        let mut unusable = 0usize;
        for (id, fp) in rows {
            match decode_chromaprint(&fp).and_then(|f| chroma_usable(&f).map(|_| f)) {
                Ok(frames) => list.push(LegacyEntry { fingerprint_id: id, frames }),
                Err(_) => unusable += 1,
            }
        }
        if unusable > 0 {
            tracing::warn!(unusable, "legacy chromaprints that cannot carry an ownership comparison (too short, silent or undecodable)");
        }
        let list = Arc::new(list);
        *self.inner.write().await = Some((Instant::now(), list.clone()));
        Ok(list)
    }
}

// ── The search ───────────────────────────────────────────────────────────────

pub struct Query<'a> {
    pub fingerprint_id: Uuid,
    pub uploader_pial: Uuid,
    pub sha256: &'a str,
    pub duration: Option<f64>,
    pub chroma: Option<&'a [u32]>,
    pub candidates: &'a [Candidate],
}

/// The original this media should be credited to, if any. Same-owner matches
/// are not ownership events: re-uploading your own work credits you already.
pub async fn find(pool: &PgPool, legacy: &LegacyAudio, q: &Query<'_>) -> sqlx::Result<Option<Match>> {
    let mut found: Vec<Match> = Vec::new();

    // exact bytes
    let exact: Option<(Uuid, Option<Uuid>, Option<Uuid>, DateTime<Utc>)> = sqlx::query_as(
        "SELECT f.id, f.asset_id, f.uploader_pial, f.created_at FROM fingerprints f
          WHERE f.sha256 = $1 AND f.id <> $2 AND f.ownership_eligible
            AND NOT EXISTS (SELECT 1 FROM banned_hashes b WHERE b.fingerprint_id = f.id AND b.revoked_at IS NULL)
          ORDER BY f.created_at, f.id LIMIT 1",
    )
    .bind(q.sha256)
    .bind(q.fingerprint_id)
    .fetch_optional(pool)
    .await?;
    if let Some((id, asset, pial, created)) = exact {
        found.push(Match {
            level: Level::Credit,
            original_fingerprint: id,
            original_asset: asset,
            original_pial: pial,
            original_created: created,
            kind: "exact",
            layer: "sha256",
            confidence: 1.0,
            offset_seconds: Some(0.0),
            offsets: json!({ "offset_seconds": 0.0, "scale": 1.0 }),
        });
    }

    // perceptual index
    let ids: Vec<Uuid> = q.candidates.iter().map(|c| c.fingerprint_id).collect();
    let meta = metadata(pool, &ids).await?;
    for c in q.candidates {
        let Some(m) = meta.get(&c.fingerprint_id) else { continue };
        if let Some((level, kind, layer, confidence, offset, offsets)) = evaluate(&c.evidence, q.duration, m.duration) {
            found.push(Match {
                level,
                original_fingerprint: c.fingerprint_id,
                original_asset: m.asset,
                original_pial: m.pial,
                original_created: m.created,
                kind,
                layer,
                confidence: confidence.clamp(0.0, 1.0),
                offset_seconds: offset,
                offsets,
            });
        }
    }

    // legacy chromaprints
    if let Some(query) = q.chroma.filter(|f| chroma_usable(f).is_ok()) {
        let entries = legacy.entries(pool).await?;
        let query = query.to_vec();
        let scored: Vec<(Uuid, f64)> = tokio::task::spawn_blocking(move || {
            entries
                .iter()
                .filter_map(|e| chroma_similarity(&query, &e.frames).map(|s| (e.fingerprint_id, s)))
                .filter(|(_, s)| *s >= CHROMA_REVIEW)
                .collect()
        })
        .await
        .map_err(|e| sqlx::Error::Protocol(format!("legacy chromaprint scan failed: {e}")))?;
        let ids: Vec<Uuid> = scored.iter().map(|(id, _)| *id).filter(|id| *id != q.fingerprint_id).collect();
        let meta = metadata(pool, &ids).await?;
        for (id, s) in scored {
            let Some(m) = meta.get(&id) else { continue };
            found.push(Match {
                level: if s >= CHROMA_CREDIT { Level::Credit } else { Level::Review },
                original_fingerprint: id,
                original_asset: m.asset,
                original_pial: m.pial,
                original_created: m.created,
                kind: "derivative",
                layer: "chromaprint",
                confidence: s,
                offset_seconds: None,
                offsets: json!({ "similarity": s }),
            });
        }
    }

    // The original is the EARLIEST match at the strongest level found: in a
    // chain A → B (copied A) → C (copied B), C matches both, and A is owed.
    let best_level = found.iter().map(|m| m.level).max();
    let Some(level) = best_level else { return Ok(None) };
    let original = found
        .into_iter()
        .filter(|m| m.level == level)
        .min_by(|a, b| a.original_created.cmp(&b.original_created).then(b.confidence.total_cmp(&a.confidence)));
    Ok(original.filter(|m| m.original_pial != Some(q.uploader_pial)))
}
