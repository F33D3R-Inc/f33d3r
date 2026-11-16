//! The perceptual index: writing an asset's visual segments and audio
//! landmarks, and finding the indexed media a new asset shares material with.
//!
//! Retrieval returns CANDIDATES with their alignment and nothing else. Whether
//! a candidate is banned material, an original someone should be credited
//! for, or noise, is decided by banned.rs and ownership.rs, each with its own
//! thresholds — the two questions have different costs of error and draw
//! against different populations, so they must not share a cut.

use std::collections::{HashMap, HashSet};

use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

use crate::align::{best_alignment, speed_scales, Alignment, Pair, UNIT_SCALE};
use crate::hashing::{hamming, phash_bands};
use crate::worker::{Landmark, Segment};

/// A hash whose posting list is longer than this carries no identity: it is
/// silence, a tone, a click track, a black frame. It is dropped from the
/// query rather than allowed to vote. Fetched with LIMIT STOP+1, so the cost
/// of a stop-hash is bounded however common it becomes.
const AUDIO_STOP_POSTINGS: i64 = 1000;
const BAND_STOP_POSTINGS: i64 = 2000;

/// Hashes per index round trip.
const LOOKUP_CHUNK: usize = 4000;

/// Candidates carried into alignment, by raw hit count. Alignment over the
/// speed grid costs ~|scales|·|hits| per candidate; the top of the hit count
/// is where every true match is.
const MAX_AUDIO_CANDIDATES: usize = 16;
const MAX_VIDEO_CANDIDATES: usize = 32;

/// Offset bin for audio votes: 100 ms. Landmark times are quantised by the
/// worker's hop (~23–32 ms); two copies of the same audio agree to within a
/// hop or two, so 100 ms (with the two-bin window in align.rs) holds a true
/// peak together while spreading unrelated hits over ten bins per second.
const AUDIO_BIN_SECONDS: f64 = 0.1;
/// Offset bin for visual votes: 1 s. Segment boundaries (shot cuts or a fixed
/// cadence) move by up to a frame or a re-encoder's GOP between two copies.
const VIDEO_BIN_SECONDS: f64 = 1.0;
/// A segment pair votes only within this Hamming radius. Wider than the
/// single-image radius: each vote is one of several that must line up.
pub const SEGMENT_VOTE_RADIUS: u32 = 8;
/// A single-segment query (an image) keeps candidates out to the review band.
pub const IMAGE_CANDIDATE_RADIUS: u32 = 12;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum AudioKind {
    /// The worker's classic (f1, f2, dt) hashes. Survive re-encodes, trims,
    /// excerpts, added noise. Aligned on offset alone.
    Classic = 0,
    /// Compound keys over the worker's pitch- and tempo-invariant hashes (see
    /// robust_keys). Aligned on offset AND time scale.
    Robust = 1,
}

/// Family tag for compound robust keys, beside the worker's 1 (classic) and
/// 2 (robust) in bits 60-62, so a compound key can never equal a raw hash.
const COMPOUND_TAG: i64 = 3 << 60;
const ROBUST_PAYLOAD: i64 = (1 << 18) - 1;

/// Indexable keys from the worker's robust landmarks.
///
/// A robust hash carries 18 bits (two frequency ratios and a time ratio,
/// quantised): 262 144 values. Indexed alone, every value collects a posting
/// from every few seconds of audio on the platform, and past a few hundred
/// hours the posting lists are all stop-lists — recall drops to nothing
/// exactly when the index is worth having. So each anchor's robust hashes
/// (the worker emits one per neighbour pair, all stamped with the anchor's
/// time) are combined pairwise: the unordered pair {h_i, h_j} of two hashes
/// from the same anchor. Both members are invariant under pitch shift, tempo
/// change and speed change, so the pair is too, and it carries 36 bits.
///
/// The cost is survival: a key needs both of its triples to survive the
/// transform. With up to 6 triples per anchor there are up to 15 keys per
/// anchor, so an anchor keeps keys while any two of its triples survive.
/// Keys keep the anchor's time, which is what alignment needs.
pub fn robust_keys(robust: &[Landmark]) -> Vec<Landmark> {
    let mut by_anchor: std::collections::BTreeMap<i64, Vec<i64>> = std::collections::BTreeMap::new();
    for l in robust {
        by_anchor.entry((l.t * 1000.0).round() as i64).or_default().push(l.hash & ROBUST_PAYLOAD);
    }
    let mut out = Vec::new();
    for (t_ms, mut hs) in by_anchor {
        hs.sort_unstable();
        hs.dedup();
        for i in 0..hs.len() {
            for j in i + 1..hs.len() {
                out.push(Landmark { hash: COMPOUND_TAG | (hs[i] << 18) | hs[j], t: t_ms as f64 / 1000.0 });
            }
        }
    }
    out
}

/// The id of a landmark parameter set, registered on first sight.
pub async fn landmark_version_id(pool: &PgPool, name: &str) -> sqlx::Result<i16> {
    sqlx::query("INSERT INTO landmark_versions (name) VALUES ($1) ON CONFLICT (name) DO NOTHING")
        .bind(name)
        .execute(pool)
        .await?;
    sqlx::query_scalar("SELECT id FROM landmark_versions WHERE name = $1").bind(name).fetch_one(pool).await
}

/// One indexed asset that shares material with the query.
#[derive(Debug, Clone)]
pub struct Candidate {
    pub fingerprint_id: Uuid,
    pub evidence: Evidence,
}

#[derive(Debug, Clone)]
pub enum Evidence {
    /// Audio landmark alignment. `density` is aligned / query landmarks inside
    /// the matched query span: the share of the query's material in that
    /// stretch that the reference explains.
    Audio { kind: AudioKind, alignment: Alignment, density: f64 },
    /// Visual segment alignment across a multi-segment query.
    Video { alignment: Alignment, density: f64, best_distance: u32 },
    /// A single-segment query (an image) against any indexed segment.
    Image { distance: u32, reference_t: f64 },
}

// ── Writes ───────────────────────────────────────────────────────────────────

/// Writes an image or video's visual segments and their band keys. An image is
/// one segment at t = 0.
pub async fn write_segments(
    tx: &mut Transaction<'_, Postgres>,
    fingerprint_id: Uuid,
    asset_id: Option<Uuid>,
    segments: &[(f64, i64)],
) -> sqlx::Result<()> {
    for chunk in segments.chunks(2000) {
        let ts: Vec<f32> = chunk.iter().map(|(t, _)| *t as f32).collect();
        let hs: Vec<i64> = chunk.iter().map(|(_, h)| *h).collect();
        // Each returned row carries its own phash, so a band key can never be
        // attached to the wrong segment whatever order RETURNING produces.
        let rows: Vec<(i64, i64)> = sqlx::query_as(
            "INSERT INTO video_segments (fingerprint_id, asset_id, t, phash)
             SELECT $1, $2, t, h FROM unnest($3::real[], $4::bigint[]) AS s(t, h)
             RETURNING id, phash",
        )
        .bind(fingerprint_id)
        .bind(asset_id)
        .bind(&ts)
        .bind(&hs)
        .fetch_all(&mut **tx)
        .await?;
        let mut band_vals: Vec<i32> = Vec::with_capacity(rows.len() * 7);
        let mut band_ids: Vec<i64> = Vec::with_capacity(rows.len() * 7);
        for (id, h) in rows {
            for b in phash_bands(h) {
                band_vals.push(b);
                band_ids.push(id);
            }
        }
        sqlx::query("INSERT INTO segment_bands (band, segment_id) SELECT * FROM unnest($1::int[], $2::bigint[])")
            .bind(&band_vals)
            .bind(&band_ids)
            .execute(&mut **tx)
            .await?;
    }
    Ok(())
}

/// Writes landmarks of one kind. Robust landmarks must already be compound
/// keys (robust_keys).
pub async fn write_landmarks(
    tx: &mut Transaction<'_, Postgres>,
    asset_id: Uuid,
    kind: AudioKind,
    version: i16,
    landmarks: &[Landmark],
) -> sqlx::Result<()> {
    for chunk in landmarks.chunks(20_000) {
        let hs: Vec<i64> = chunk.iter().map(|l| l.hash).collect();
        let ts: Vec<i32> = chunk.iter().map(|l| (l.t * 1000.0).round() as i32).collect();
        sqlx::query(
            "INSERT INTO audio_landmarks (hash, asset_id, t_offset, kind, version)
             SELECT h, $1, t, $2, $5 FROM unnest($3::bigint[], $4::int[]) AS s(h, t)",
        )
        .bind(asset_id)
        .bind(kind as i16)
        .bind(&hs)
        .bind(&ts)
        .bind(version)
        .execute(&mut **tx)
        .await?;
    }
    Ok(())
}

// ── Retrieval: audio ─────────────────────────────────────────────────────────

/// Candidates sharing audio with the query, aligned. Classic hits must line
/// up on one offset; robust hits on one offset and one time scale — a hit
/// count alone is never a match. `exclude` is the query's own asset (a
/// recheck finds itself in the index otherwise). Robust `landmarks` must
/// already be compound keys.
pub async fn audio_candidates(
    pool: &PgPool,
    landmarks: &[Landmark],
    kind: AudioKind,
    version: i16,
    exclude: Option<Uuid>,
) -> sqlx::Result<Vec<(Uuid, Alignment, f64)>> {
    if landmarks.is_empty() {
        return Ok(Vec::new());
    }
    // hash → indices of query landmarks carrying it
    let mut by_hash: HashMap<i64, Vec<u32>> = HashMap::new();
    for (i, l) in landmarks.iter().enumerate() {
        by_hash.entry(l.hash).or_default().push(i as u32);
    }
    let hashes: Vec<i64> = by_hash.keys().copied().collect();

    let mut postings: HashMap<i64, Vec<(Uuid, i32)>> = HashMap::new();
    for chunk in hashes.chunks(LOOKUP_CHUNK) {
        let rows: Vec<(i64, Uuid, i32)> = sqlx::query_as(
            "SELECT q.hash, p.asset_id, p.t_offset
               FROM unnest($1::bigint[]) AS q(hash)
               CROSS JOIN LATERAL (
                    SELECT a.asset_id, a.t_offset
                      FROM audio_landmarks a
                     WHERE a.hash = q.hash AND a.kind = $2 AND a.version = $4
                     LIMIT $3) p",
        )
        .bind(chunk)
        .bind(kind as i16)
        .bind(AUDIO_STOP_POSTINGS + 1)
        .bind(version)
        .fetch_all(pool)
        .await?;
        for (h, a, t) in rows {
            postings.entry(h).or_default().push((a, t));
        }
    }
    postings.retain(|_, v| v.len() as i64 <= AUDIO_STOP_POSTINGS);

    let mut pairs: HashMap<Uuid, Vec<Pair>> = HashMap::new();
    for (h, list) in &postings {
        let Some(qids) = by_hash.get(h) else { continue };
        for (asset, t_ref) in list {
            if Some(*asset) == exclude {
                continue;
            }
            let entry = pairs.entry(*asset).or_default();
            for &qid in qids {
                entry.push(Pair { tq: landmarks[qid as usize].t, tr: *t_ref as f64 / 1000.0, qid });
            }
        }
    }

    let mut ranked: Vec<(Uuid, Vec<Pair>)> = pairs.into_iter().collect();
    ranked.sort_by(|a, b| b.1.len().cmp(&a.1.len()));
    ranked.truncate(MAX_AUDIO_CANDIDATES);

    let scales = match kind {
        AudioKind::Classic => UNIT_SCALE.to_vec(),
        AudioKind::Robust => speed_scales(),
    };
    let mut times: Vec<f64> = landmarks.iter().map(|l| l.t).collect();
    times.sort_by(|a, b| a.total_cmp(b));

    let mut out = Vec::new();
    for (asset, ps) in ranked {
        let scales = scales.clone();
        // Alignment over the speed grid is CPU work; keep it off the reactor.
        let aligned = tokio::task::spawn_blocking(move || best_alignment(&ps, &scales, AUDIO_BIN_SECONDS))
            .await
            .map_err(|e| sqlx::Error::Protocol(format!("alignment task failed: {e}")))?;
        if let Some(a) = aligned {
            let inside = count_within(&times, a.q_start, a.q_end).max(1);
            out.push((asset, a, a.aligned as f64 / inside as f64));
        }
    }
    Ok(out)
}

fn count_within(sorted: &[f64], lo: f64, hi: f64) -> usize {
    let start = sorted.partition_point(|t| *t < lo);
    let end = sorted.partition_point(|t| *t <= hi);
    end.saturating_sub(start)
}

/// The fingerprint ids behind audio candidates (landmarks are keyed by asset).
pub async fn fingerprints_of_assets(pool: &PgPool, assets: &[Uuid]) -> sqlx::Result<HashMap<Uuid, Uuid>> {
    if assets.is_empty() {
        return Ok(HashMap::new());
    }
    let rows: Vec<(Uuid, Uuid)> = sqlx::query_as("SELECT asset_id, id FROM fingerprints WHERE asset_id = ANY($1)")
        .bind(assets)
        .fetch_all(pool)
        .await?;
    Ok(rows.into_iter().collect())
}

// ── Retrieval: visual ────────────────────────────────────────────────────────

struct SegmentHit {
    segment_id: i64,
    fingerprint_id: Uuid,
    t: f64,
    phash: i64,
}

async fn band_lookup(pool: &PgPool, bands: &[i32]) -> sqlx::Result<Vec<(i32, SegmentHit)>> {
    let mut out = Vec::new();
    for chunk in bands.chunks(LOOKUP_CHUNK) {
        let rows: Vec<(i32, i64, Uuid, f32, i64)> = sqlx::query_as(
            "SELECT q.band, s.id, s.fingerprint_id, s.t, s.phash
               FROM unnest($1::int[]) AS q(band)
               CROSS JOIN LATERAL (
                    SELECT v.id, v.fingerprint_id, v.t, v.phash
                      FROM segment_bands b
                      JOIN video_segments v ON v.id = b.segment_id
                     WHERE b.band = q.band
                     LIMIT $2) s",
        )
        .bind(chunk)
        .bind(BAND_STOP_POSTINGS + 1)
        .fetch_all(pool)
        .await?;
        out.extend(rows.into_iter().map(|(band, id, fp, t, h)| {
            (band, SegmentHit { segment_id: id, fingerprint_id: fp, t: t as f64, phash: h })
        }));
    }
    // Drop stop-bands (a black frame's band is in every video).
    let mut per_band: HashMap<i32, usize> = HashMap::new();
    for (b, _) in &out {
        *per_band.entry(*b).or_insert(0) += 1;
    }
    out.retain(|(b, _)| per_band[b] as i64 <= BAND_STOP_POSTINGS);
    Ok(out)
}

/// Candidates sharing pictures with the query. A one-segment query is an
/// image: every indexed segment within IMAGE_CANDIDATE_RADIUS is a candidate,
/// at its best distance. A multi-segment query is aligned in time.
pub async fn visual_candidates(
    pool: &PgPool,
    segments: &[(f64, i64)],
    exclude: Option<Uuid>,
) -> sqlx::Result<Vec<Candidate>> {
    if segments.is_empty() {
        return Ok(Vec::new());
    }
    let mut by_band: HashMap<i32, Vec<u32>> = HashMap::new();
    for (i, (_, h)) in segments.iter().enumerate() {
        for b in phash_bands(*h) {
            by_band.entry(b).or_default().push(i as u32);
        }
    }
    let bands: Vec<i32> = by_band.keys().copied().collect();
    let hits = band_lookup(pool, &bands).await?;

    let single = segments.len() == 1;
    let radius = if single { IMAGE_CANDIDATE_RADIUS } else { SEGMENT_VOTE_RADIUS };
    let mut seen: HashSet<(u32, i64)> = HashSet::new();
    let mut pairs: HashMap<Uuid, Vec<(Pair, u32)>> = HashMap::new();
    for (band, hit) in hits {
        if Some(hit.fingerprint_id) == exclude {
            continue;
        }
        let Some(qids) = by_band.get(&band) else { continue };
        for &qid in qids {
            if !seen.insert((qid, hit.segment_id)) {
                continue;
            }
            let d = hamming(segments[qid as usize].1, hit.phash);
            if d > radius {
                continue;
            }
            pairs
                .entry(hit.fingerprint_id)
                .or_default()
                .push((Pair { tq: segments[qid as usize].0, tr: hit.t, qid }, d));
        }
    }

    let mut out = Vec::new();
    if single {
        for (fp, ps) in pairs {
            if let Some((p, d)) = ps.iter().min_by_key(|(_, d)| *d) {
                out.push(Candidate { fingerprint_id: fp, evidence: Evidence::Image { distance: *d, reference_t: p.tr } });
            }
        }
        out.sort_by_key(|c| match c.evidence {
            Evidence::Image { distance, .. } => distance,
            _ => u32::MAX,
        });
        out.truncate(MAX_VIDEO_CANDIDATES);
        return Ok(out);
    }

    let mut ranked: Vec<(Uuid, Vec<(Pair, u32)>)> = pairs.into_iter().collect();
    ranked.sort_by(|a, b| b.1.len().cmp(&a.1.len()));
    ranked.truncate(MAX_VIDEO_CANDIDATES);
    let mut times: Vec<f64> = segments.iter().map(|(t, _)| *t).collect();
    times.sort_by(|a, b| a.total_cmp(b));
    for (fp, ps) in ranked {
        let best_distance = ps.iter().map(|(_, d)| *d).min().unwrap_or(u32::MAX);
        let only: Vec<Pair> = ps.iter().map(|(p, _)| *p).collect();
        let aligned = tokio::task::spawn_blocking(move || best_alignment(&only, &speed_scales(), VIDEO_BIN_SECONDS))
            .await
            .map_err(|e| sqlx::Error::Protocol(format!("alignment task failed: {e}")))?;
        if let Some(a) = aligned {
            let inside = count_within(&times, a.q_start, a.q_end).max(1);
            out.push(Candidate {
                fingerprint_id: fp,
                evidence: Evidence::Video { alignment: a, density: a.aligned as f64 / inside as f64, best_distance },
            });
        }
    }
    Ok(out)
}

/// Every candidate for one query, audio and visual, keyed by fingerprint.
pub async fn all_candidates(
    pool: &PgPool,
    segments: &[(f64, i64)],
    audio: Option<(i16, &[Landmark], &[Landmark])>,
    exclude_fingerprint: Option<Uuid>,
    exclude_asset: Option<Uuid>,
) -> sqlx::Result<Vec<Candidate>> {
    let mut out = visual_candidates(pool, segments, exclude_fingerprint).await?;

    let (classic, robust) = match audio {
        Some((version, classic, robust_keys)) => (
            audio_candidates(pool, classic, AudioKind::Classic, version, exclude_asset).await?,
            audio_candidates(pool, robust_keys, AudioKind::Robust, version, exclude_asset).await?,
        ),
        None => (Vec::new(), Vec::new()),
    };
    let assets: Vec<Uuid> = classic.iter().chain(robust.iter()).map(|(a, _, _)| *a).collect();
    let fps = fingerprints_of_assets(pool, &assets).await?;
    for (kind, list) in [(AudioKind::Classic, classic), (AudioKind::Robust, robust)] {
        for (asset, alignment, density) in list {
            // An asset with landmarks always has a fingerprint (both are
            // written in one transaction); a miss here would be a broken
            // index, so it is surfaced rather than skipped quietly.
            let Some(fp) = fps.get(&asset) else {
                tracing::error!(asset = %asset, "audio landmarks reference an asset with no fingerprint");
                continue;
            };
            out.push(Candidate { fingerprint_id: *fp, evidence: Evidence::Audio { kind, alignment, density } });
        }
    }
    Ok(out)
}

pub fn segments_of(phash: Option<i64>, segs: &[Segment]) -> Vec<(f64, i64)> {
    if !segs.is_empty() {
        return segs.iter().map(|s| (s.t, s.phash)).collect();
    }
    phash.map(|h| vec![(0.0, h)]).unwrap_or_default()
}
