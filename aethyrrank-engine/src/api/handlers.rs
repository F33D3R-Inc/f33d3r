//! API handlers — POST /rank, GET /health
//!
//! ## Pipeline stages (executed in order)
//!
//!   0. Contract        — psych vectors, and the safety barrier's invariant
//!                        (safety::barrier): a pool that breaks it is refused whole
//!   1. Pre-rank        — fast quality×freshness filter, pool → top_k
//!   2. Neural score    — P(engagement | u, i, ctx) for top_k items
//!   3. AESQ constraint — structural multiplier
//!   4. Velocity boost  — d(engagement)/dt adjustment
//!   5. Revenue layer   — conversion/LTV adjustment (capped), in-network boost,
//!                        and the viewer's learned LinUCB preference
//!   6. Re-rank         — diversity spacing → exploration inject → trim
//!
//! Every work of the pool is then recorded in the feature cache with the
//! context it was ranked in, which is what a later ranking signal learns from.

use axum::{extract::State, http::StatusCode, Json};
use chrono::Utc;
use nalgebra::DVector;
use std::collections::HashMap;
use std::sync::Arc;
use std::time::Instant;
use tracing::{error, info, instrument, warn};

use crate::api::types::*;
use crate::bandit::explorer::inject_exploration_slots;
use crate::bandit::linucb::build_feature_vector;
use crate::config::AppConfig;
use crate::learning::features::{FeatureCache, RankedContext};
use crate::model_store::ModelStore;
use crate::pipeline::{neural, preranker, reranker, revenue, velocity};
use crate::safety::barrier;
use crate::scoring::aesq::{compute_aesq, AesqScore};
use crate::scoring::jung::{is_psych_vector, PSYCH_DIM};

pub struct AppState {
    pub config: Arc<AppConfig>,
    pub model_store: Arc<ModelStore>,
    pub features: Arc<FeatureCache>,
    pub internal_api_key: String,
}

/// The key a viewer's model lives under on one surface. A NEXUS shard, when
/// present, joins every persona of one person into one model.
pub fn model_key(user: &UserState, surface: &str) -> String {
    let who = user.nexus_shard_id.as_deref().unwrap_or(&user.user_id);
    format!("{who}:{surface}")
}

/// AESQ and the LinUCB feature vector of one item for one viewer.
fn features_of(user: &UserState, item: &ContentItem, cfg: &AppConfig) -> (AesqScore, DVector<f64>) {
    let aesq = compute_aesq(user, item, &cfg.scoring);
    let fatigue = {
        let seen = user.interaction_history.iter().filter(|id| *id == &item.content_id).count() as f64;
        let w = user.interaction_history.len().max(1) as f64;
        (seen / w).clamp(0.0, 1.0)
    };
    let affinity = user.creator_affinities.get(&item.creator_id).copied().unwrap_or(0.0) as f64;
    // Realm trust: 0.0 at Realm 1, 1.0 at Realm 5 — blended with creator affinity.
    let realm_trust = ((user.realm_level.saturating_sub(1)) as f64) / 4.0;
    let creator_boost = (affinity * 0.7 + realm_trust * 0.3).clamp(0.0, 1.0);
    let fv = build_feature_vector(
        aesq.alignment, aesq.expansion, aesq.shadow,
        aesq.quality, aesq.freshness, creator_boost, 1.0 - fatigue,
    );
    (aesq, fv)
}

// ── POST /rank ────────────────────────────────────────────────────────────────

#[instrument(skip_all, fields(
    request_id = %req.session_context.request_id,
    surface    = %req.session_context.surface,
    pool_size  = req.content_pool.len(),
))]
pub async fn rank_handler(
    State(state): State<Arc<AppState>>,
    Json(req): Json<RankRequest>,
) -> Result<Json<RankResponse>, StatusCode> {
    let start = Instant::now();
    let request_id = req.session_context.request_id.clone();
    let surface = req.session_context.surface.clone();
    let cfg = &state.config;

    info!("rank request received");

    // ── Stage 0: Contract ─────────────────────────────────────────────────────
    // Every vector in the request lives in the eight-axis Jung space shared
    // with Zior and feed-engine. A vector of any other length is not a
    // psychological vector, and scoring it would be arithmetic on nothing;
    // the request is refused whole rather than partly ranked.
    if !is_psych_vector(&req.user_state.interest_vector) {
        warn!(
            dims = req.user_state.interest_vector.len(),
            expected = PSYCH_DIM,
            "interest_vector is not a psych vector — rejecting"
        );
        return Err(StatusCode::BAD_REQUEST);
    }
    if let Some(bad) = req
        .content_pool
        .iter()
        .find(|c| !is_psych_vector(&c.topic_vector))
    {
        warn!(
            content_id = %bad.content_id,
            dims = bad.topic_vector.len(),
            expected = PSYCH_DIM,
            "topic_vector is not a psych vector — rejecting"
        );
        return Err(StatusCode::BAD_REQUEST);
    }
    if let Some(bad) = barrier::first_violation(&req.user_state, &req.content_pool) {
        // feed-engine's withoutBarrierRefused is the one authority, and it let
        // through a work it must refuse. Not repaired here: refused, loudly.
        error!(
            content_id = %bad.content_id,
            user_id = %req.user_state.user_id,
            safety_epsilon = req.user_state.safety_epsilon,
            adult_probability = bad.adult_probability,
            "SAFETY BARRIER VIOLATED: the pool carries a work the viewer's barrier refuses — rejecting the request"
        );
        metrics::counter!(barrier::METRIC_BARRIER_VIOLATIONS).increment(1);
        return Err(StatusCode::UNPROCESSABLE_ENTITY);
    }
    let key = model_key(&req.user_state, &surface);

    // ── Stage 1: Pre-rank ─────────────────────────────────────────────────────
    let preranked = preranker::prerank(&req.content_pool, &cfg.prerank, Utc::now());
    let candidate_items: Vec<&ContentItem> = preranked.iter().map(|p| p.item).collect();

    // Build fast_score lookup
    let fast_scores: std::collections::HashMap<&str, f64> = preranked
        .iter()
        .map(|p| (p.item.content_id.as_str(), p.fast_score))
        .collect();

    // ── Stage 2: Neural scoring ───────────────────────────────────────────────
    let neural_scores = neural::score_batch(&req.user_state, &candidate_items);
    let neural_lookup: std::collections::HashMap<&str, f64> = neural_scores
        .iter()
        .map(|s| (s.content_id.as_str(), s.score))
        .collect();

    let explore_count = if req.user_state.is_cold_start {
        cfg.bandit.cold_start_explore_count
    } else {
        cfg.bandit.exploration_count
    };

    // ── Stages 3–5: Per-item scoring pass ────────────────────────────────────
    let mut features: HashMap<String, DVector<f64>> = HashMap::with_capacity(req.content_pool.len());
    let mut scored_items: Vec<RankedItem> = candidate_items
        .iter()
        .map(|item| {
            let neural_score = neural_lookup.get(item.content_id.as_str()).copied().unwrap_or(0.0);

            // Stage 3: AESQ constraint multiplier
            let (aesq, fv) = features_of(&req.user_state, item, cfg);
            let aesq_mult    = (aesq.total * cfg.pipeline.aesq_constraint_weight
                              + (1.0 - cfg.pipeline.aesq_constraint_weight))
                              .clamp(0.0, 1.0);
            let after_aesq   = neural_score * aesq_mult;

            // Stage 4: Velocity boost
            let v_boost      = velocity::velocity_boost(item, &cfg.velocity);
            let phase        = velocity::lifecycle_phase(item, &cfg.velocity);
            let after_vel    = (after_aesq + v_boost).clamp(0.0, 1.0);

            // Stage 5: Revenue adjustment
            let rev_adj      = revenue::revenue_adjustment(item, &req.user_state, &cfg.revenue);

            // Stage 5b: In-network boost — social graph content scores higher
            let network_adj  = if item.is_in_network { cfg.network.network_boost } else { 0.0 };

            // Stage 5c: Learned preference — learned_weight × θᵀx of the
            // viewer's own LinUCB model (config/default.toml says why 0.15).
            // A viewer with no model yet has learned nothing: 0. A model that
            // cannot be read is logged and contributes nothing to this request.
            let learned_adj = match state.model_store.expected(&key, &fv) {
                None => 0.0,
                Some(Ok(expected)) => cfg.learning.learned_weight * expected,
                Some(Err(e)) => {
                    error!(model = %key, error = %e, "learned model unreadable; ranking without it");
                    metrics::counter!("aethyrrank_learned_model_errors_total").increment(1);
                    0.0
                }
            };

            let final_score  = (after_vel + rev_adj + network_adj + learned_adj).clamp(0.0, 1.0);
            features.insert(item.content_id.clone(), fv);

            let fast_score = *fast_scores.get(item.content_id.as_str()).unwrap_or(&0.0);

            RankedItem {
                content_id:   item.content_id.clone(),
                rank:         0,
                final_score,
                score_breakdown: ScoreBreakdown {
                    fast_score,
                    neural_score,
                    aesq_alignment:  aesq.alignment,
                    aesq_expansion:  aesq.expansion,
                    aesq_shadow:     aesq.shadow,
                    aesq_quality:    aesq.quality,
                    aesq_freshness:  aesq.freshness,
                    aesq_total:      aesq.total,
                    aesq_multiplier: aesq_mult,
                    velocity_boost:  v_boost,
                    revenue_adj:     rev_adj,
                    learned_adj,
                    final_score,
                },
                explanation: format!(
                    "neural={:.3} × aesq_mult={:.3}={:.3} | vel={:+.3} | rev={:+.3} | net={:+.3} | learned={:+.3} | {:?} | final={:.3}",
                    neural_score, aesq_mult, after_aesq, v_boost, rev_adj, network_adj, learned_adj, phase, final_score,
                ),
                exploration_slot: false,
                safety_blocked:   false,
            }
        })
        .collect();

    // Sort descending
    scored_items.sort_by(|a, b| {
        b.final_score
            .partial_cmp(&a.final_score)
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    for (i, item) in scored_items.iter_mut().enumerate() {
        item.rank = i + 1;
    }

    // ── Stage 6a: Diversity spacing ────────────────────────────────────────────
    let creator_pairs: Vec<(String, String)> = req
        .content_pool
        .iter()
        .map(|c| (c.content_id.clone(), c.creator_id.clone()))
        .collect();
    let scored_items = reranker::apply_diversity(scored_items, &creator_pairs);

    // ── Stage 6b: Exploration injection ──────────────────────────────────────
    // May pick items the pre-ranker dropped; the barrier invariant already
    // holds for the whole pool.
    let page_size = req
        .session_context
        .page_size
        .unwrap_or(req.session_context.max_items);
    let with_exploration = inject_exploration_slots(
        scored_items,
        &req.content_pool,
        explore_count,
        page_size,
    );

    let final_items: Vec<RankedItem> = with_exploration
        .into_iter()
        .take(req.session_context.max_items)
        .collect();

    record_contexts(&state, &req, &surface, &key, &final_items, features);

    let confidence = if final_items.is_empty() {
        0.0
    } else {
        final_items
            .iter()
            .map(|i| i.final_score.max(0.0))
            .sum::<f64>()
            / final_items.len() as f64
    };

    let latency_ms = start.elapsed().as_millis() as u64;
    info!(
        latency_ms,
        items_returned = final_items.len(),
        "rank complete"
    );

    Ok(Json(RankResponse {
        request_id,
        ranked_items: final_items,
        confidence,
        latency_ms,
    }))
}

/// Record the context every work of the pool was ranked in, for learning.
/// Returned works carry their rank; the rest follow them in feed-engine's
/// output (it appends whatever was not returned), so they take the position
/// after the last returned one.
fn record_contexts(
    state: &AppState,
    req: &RankRequest,
    surface: &str,
    key: &str,
    final_items: &[RankedItem],
    mut features: HashMap<String, DVector<f64>>,
) {
    let rank_of: HashMap<&str, usize> = final_items
        .iter()
        .enumerate()
        .map(|(i, r)| (r.content_id.as_str(), i + 1))
        .collect();
    let after = final_items.len() + 1;
    let now = Instant::now();
    let contexts = req
        .content_pool
        .iter()
        .map(|item| {
            let fv = features
                .remove(&item.content_id)
                .unwrap_or_else(|| features_of(&req.user_state, item, &state.config).1);
            RankedContext {
                work_id: item.content_id.clone(),
                surface: surface.to_string(),
                model_key: key.to_string(),
                creator_account: item.creator_id.clone(),
                creator_pial: item.creator_pial.clone(),
                features: fv.as_slice().to_vec(),
                position: rank_of.get(item.content_id.as_str()).copied().unwrap_or(after),
                ranked_at: now,
            }
        })
        .collect();
    state
        .features
        .record(&req.user_state.user_id, req.user_state.viewer_pial.as_deref(), contexts);
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health_handler(State(state): State<Arc<AppState>>) -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "status":        "ok",
        "service":       "aethyrrank-engine",
        "version":       env!("CARGO_PKG_VERSION"),
        "ts":            Utc::now().to_rfc3339(),
        "feature_cache_viewers": state.features.viewers(),
        "learned_models": state.model_store.len(),
    }))
}
