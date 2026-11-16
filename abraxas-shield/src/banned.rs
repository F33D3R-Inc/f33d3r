//! The banned-content gate.
//!
//! Layers, in the order they run:
//!
//!   sha256           exact bytes, local, FAILS CLOSED: a lookup error refuses
//!                    the check rather than admitting unjudged media.
//!   phash            the image phash and every video segment phash, by
//!                    Hamming distance, against banned phash values.
//!   chromaprint      bit similarity against banned chromaprints (the legacy
//!                    registry form, imported from feed-engine).
//!   video_segments   the perceptual index, against fingerprints registered
//!   audio_landmarks  as banned — so a re-encode, an excerpt, a speed change or
//!                    banned audio under new music is found by the same
//!                    alignment that finds ownership.
//!
//! Each perceptual layer has two radii: inside the tight one the media is
//! blocked; between the two it is admitted for review. The cuts are looser
//! than ownership's, deliberately: a missed ban admits banned material, while
//! a near match is never refused silently — it goes to review. And the banned
//! registry is small and curated, so each upload draws a few hundred times
//! from the null distribution, not millions; the same evidence supports cuts
//! closer to the noise here than an index-wide ownership search can.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use sqlx::PgPool;
use tokio::sync::RwLock;
use uuid::Uuid;

use crate::hashing::{chroma_similarity, chroma_usable, decode_chromaprint};
use crate::index::{AudioKind, Candidate, Evidence};

// ── Cuts ─────────────────────────────────────────────────────────────────────

/// phash: measured on this pipeline a JPEG re-encode or an 80% resize moves
/// imagehash.phash by 2 bits of 64; 6 bits is a random-collision probability
/// near 4e-12 per comparison. Beyond 12 low-detail frames stop clustering.
pub const PHASH_BLOCK: u32 = 6;
pub const PHASH_REVIEW: u32 = 12;
/// chromaprint (content-scan's measurement campaign, carried over): every
/// re-encode, re-container, telephone-band rendering, trim, compression and
/// echo of a work scores >= 0.88 against its original; the highest score ever
/// measured between different works is 0.7886.
pub const CHROMA_BLOCK: f64 = 0.88;
pub const CHROMA_REVIEW: f64 = 0.80;
/// Classic landmarks against banned audio. The worker emits ~100 classic
/// landmarks per second; 20 aligned within 4 s is a fraction of what a clean
/// copy produces and far beyond what unrelated audio lines up on one 200 ms
/// offset window (random hits spread over ten bins per second of offset).
pub const AUDIO_BLOCK_ALIGNED: usize = 20;
pub const AUDIO_BLOCK_SPAN: f64 = 4.0;
pub const AUDIO_REVIEW_ALIGNED: usize = 8;
pub const AUDIO_REVIEW_SPAN: f64 = 2.0;
/// Compound robust keys are aligned over ~230 time scales, so the best
/// spurious peak is the best of 230 draws: the bar is set higher by that.
pub const ROBUST_BLOCK_ALIGNED: usize = 30;
pub const ROBUST_REVIEW_ALIGNED: usize = 12;
/// Visual segments against a banned video: two shots in sequence at the
/// block radius is the same footage; one is enough for a person to look.
pub const SEGMENT_BLOCK_ALIGNED: usize = 2;
pub const SEGMENT_REVIEW_ALIGNED: usize = 1;

#[derive(Debug, Clone)]
pub struct BannedMatch {
    pub banned_id: Uuid,
    pub category: String,
    pub layer: &'static str,
    pub score: f64,
    pub detail: Value,
}

impl BannedMatch {
    /// The `banned_match` object on the wire: what matched and the exact
    /// bytes it was found in.
    pub fn json(&self, exact: bool, sha256: &str) -> Value {
        json!({
            "banned_id": self.banned_id,
            "category": self.category,
            "layer": self.layer,
            "match": if exact { "exact" } else { "near" },
            "sha256": sha256,
            "score": self.score,
            "detail": self.detail,
        })
    }
}

#[derive(Debug, Clone)]
pub enum Outcome {
    Clear,
    Near(BannedMatch),
    Block(BannedMatch),
}

/// Severity order for combining layers: a block beats a near; among equals,
/// csam beats every other category.
fn stronger(a: Outcome, b: Outcome) -> Outcome {
    fn rank(o: &Outcome) -> (u8, u8) {
        match o {
            Outcome::Clear => (0, 0),
            Outcome::Near(m) => (1, (m.category == "csam") as u8),
            Outcome::Block(m) => (2, (m.category == "csam") as u8),
        }
    }
    if rank(&b) > rank(&a) {
        b
    } else {
        a
    }
}

// ── Exact ────────────────────────────────────────────────────────────────────

pub async fn sha256(pool: &PgPool, sha: &str) -> sqlx::Result<Option<BannedMatch>> {
    let row: Option<(Uuid, String)> = sqlx::query_as(
        "SELECT id, category FROM banned_hashes
          WHERE layer = 'sha256' AND sha256 = $1 AND revoked_at IS NULL",
    )
    .bind(sha)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|(id, category)| BannedMatch {
        banned_id: id,
        category,
        layer: "sha256",
        score: 1.0,
        detail: json!({}),
    }))
}

// ── chromaprint registry cache ───────────────────────────────────────────────

struct ChromaEntry {
    id: Uuid,
    category: String,
    frames: Vec<u32>,
}

/// The banned chromaprints, decoded once and refreshed every minute. A row
/// that will not decode, or carries too little information to match safely,
/// is a ban that is NOT being enforced: it is logged as an incident on every
/// refresh until someone replaces it, never skipped quietly.
#[derive(Clone, Default)]
pub struct ChromaRegistry {
    inner: Arc<RwLock<Option<(Instant, Arc<Vec<ChromaEntry>>)>>>,
}

impl ChromaRegistry {
    async fn entries(&self, pool: &PgPool) -> sqlx::Result<Arc<Vec<ChromaEntry>>> {
        if let Some((at, list)) = self.inner.read().await.as_ref() {
            if at.elapsed() < Duration::from_secs(60) {
                return Ok(list.clone());
            }
        }
        let rows: Vec<(Uuid, String, String)> = sqlx::query_as(
            "SELECT id, category, chromaprint FROM banned_hashes
              WHERE layer = 'chromaprint' AND revoked_at IS NULL",
        )
        .fetch_all(pool)
        .await?;
        let mut list = Vec::with_capacity(rows.len());
        for (id, category, fp) in rows {
            match decode_chromaprint(&fp).and_then(|f| chroma_usable(&f).map(|_| f)) {
                Ok(frames) => list.push(ChromaEntry { id, category, frames }),
                Err(e) => {
                    metrics::counter!("abraxas_banned_registry_faults_total", "layer" => "chromaprint").increment(1);
                    tracing::error!(banned_id = %id, category = %category, error = %e,
                        "INCIDENT: banned chromaprint cannot be enforced — replace it with a fingerprint of the material");
                }
            }
        }
        let list = Arc::new(list);
        *self.inner.write().await = Some((Instant::now(), list.clone()));
        Ok(list)
    }
}

// ── Perceptual ───────────────────────────────────────────────────────────────

/// Every perceptual layer for one fingerprint. `segments` is the image phash
/// as one segment, or the video's segments; `chroma` is the decoded
/// chromaprint, when the media has one; `candidates` comes from the index.
pub async fn perceptual(
    pool: &PgPool,
    registry: &ChromaRegistry,
    segments: &[(f64, i64)],
    chroma: Option<&[u32]>,
    candidates: &[Candidate],
) -> sqlx::Result<Outcome> {
    let mut out = Outcome::Clear;

    // phash values
    if !segments.is_empty() {
        let hashes: Vec<i64> = segments.iter().map(|(_, h)| *h).collect();
        let row: Option<(Uuid, String, i32, i64)> = sqlx::query_as(
            "SELECT b.id, b.category, bit_count((b.phash # q.h)::bit(64))::int AS d, q.h
               FROM unnest($1::bigint[]) AS q(h)
               CROSS JOIN banned_hashes b
              WHERE b.layer = 'phash' AND b.revoked_at IS NULL
              ORDER BY d ASC, (b.category = 'csam') DESC
              LIMIT 1",
        )
        .bind(&hashes)
        .fetch_optional(pool)
        .await?;
        if let Some((id, category, d, h)) = row {
            let d = d as u32;
            let t = segments.iter().find(|(_, x)| *x == h).map(|(t, _)| *t).unwrap_or(0.0);
            let m = BannedMatch {
                banned_id: id,
                category,
                layer: "phash",
                score: 1.0 - d as f64 / 64.0,
                detail: json!({ "distance": d, "t": t }),
            };
            if d <= PHASH_BLOCK {
                out = stronger(out, Outcome::Block(m));
            } else if d <= PHASH_REVIEW {
                out = stronger(out, Outcome::Near(m));
            }
        }
    }

    // chromaprint values
    if let Some(query) = chroma {
        if chroma_usable(query).is_ok() {
            let entries = registry.entries(pool).await?;
            let query = query.to_vec();
            let best = tokio::task::spawn_blocking(move || {
                let mut best: Option<(usize, f64)> = None;
                for (i, e) in entries.iter().enumerate() {
                    if let Some(s) = chroma_similarity(&query, &e.frames) {
                        if best.map_or(true, |(_, b)| s > b) {
                            best = Some((i, s));
                        }
                    }
                }
                best.map(|(i, s)| (entries[i].id, entries[i].category.clone(), s))
            })
            .await
            .map_err(|e| sqlx::Error::Protocol(format!("chromaprint scan failed: {e}")))?;
            if let Some((id, category, s)) = best {
                let m = BannedMatch { banned_id: id, category, layer: "chromaprint", score: s, detail: json!({ "similarity": s }) };
                if s >= CHROMA_BLOCK {
                    out = stronger(out, Outcome::Block(m));
                } else if s >= CHROMA_REVIEW {
                    out = stronger(out, Outcome::Near(m));
                }
            }
        }
    }

    // index candidates that are banned fingerprints
    if !candidates.is_empty() {
        let ids: Vec<Uuid> = candidates.iter().map(|c| c.fingerprint_id).collect();
        let rows: Vec<(Uuid, Uuid, String, String)> = sqlx::query_as(
            "SELECT id, fingerprint_id, layer, category FROM banned_hashes
              WHERE fingerprint_id = ANY($1) AND revoked_at IS NULL",
        )
        .bind(&ids)
        .fetch_all(pool)
        .await?;
        let mut bans: HashMap<(Uuid, String), (Uuid, String)> = HashMap::new();
        for (id, fp, layer, category) in rows {
            bans.insert((fp, layer), (id, category));
        }
        for c in candidates {
            let layer = match c.evidence {
                Evidence::Audio { .. } => "audio_landmarks",
                Evidence::Video { .. } | Evidence::Image { .. } => "video_segments",
            };
            let Some((id, category)) = bans.get(&(c.fingerprint_id, layer.to_string())) else { continue };
            let (verdict, score, detail) = judge_candidate(&c.evidence);
            let m = BannedMatch {
                banned_id: *id,
                category: category.clone(),
                layer,
                score,
                detail: json!({ "fingerprint_id": c.fingerprint_id, "alignment": detail }),
            };
            out = match verdict {
                Some(true) => stronger(out, Outcome::Block(m)),
                Some(false) => stronger(out, Outcome::Near(m)),
                None => out,
            };
        }
    }
    Ok(out)
}

/// Some(true) block, Some(false) review, None no match.
fn judge_candidate(e: &Evidence) -> (Option<bool>, f64, Value) {
    match e {
        Evidence::Audio { kind, alignment: a, density } => {
            let (block, review) = match kind {
                AudioKind::Classic => (AUDIO_BLOCK_ALIGNED, AUDIO_REVIEW_ALIGNED),
                AudioKind::Robust => (ROBUST_BLOCK_ALIGNED, ROBUST_REVIEW_ALIGNED),
            };
            let detail = crate::ownership::alignment_json(a, Some(*density));
            let score = (a.aligned as f64 / (2 * block) as f64).min(1.0);
            if a.aligned >= block && a.q_span() >= AUDIO_BLOCK_SPAN {
                (Some(true), score, detail)
            } else if a.aligned >= review && a.q_span() >= AUDIO_REVIEW_SPAN {
                (Some(false), score, detail)
            } else {
                (None, score, detail)
            }
        }
        Evidence::Video { alignment: a, density, best_distance } => {
            let detail = crate::ownership::alignment_json(a, Some(*density));
            let score = (a.aligned as f64 / 4.0).min(1.0);
            if a.aligned >= SEGMENT_BLOCK_ALIGNED && *best_distance <= PHASH_BLOCK {
                (Some(true), score, detail)
            } else if a.aligned >= SEGMENT_REVIEW_ALIGNED && *best_distance <= PHASH_BLOCK {
                (Some(false), score, detail)
            } else {
                (None, score, detail)
            }
        }
        Evidence::Image { distance, reference_t } => {
            let detail = json!({ "distance": distance, "reference_t": reference_t });
            let score = 1.0 - *distance as f64 / 64.0;
            if *distance <= PHASH_BLOCK {
                (Some(true), score, detail)
            } else if *distance <= PHASH_REVIEW {
                (Some(false), score, detail)
            } else {
                (None, score, detail)
            }
        }
    }
}
