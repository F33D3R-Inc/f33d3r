//! API handlers — POST /rank, POST /feedback, GET /health
//!
//! ## Pipeline stages (executed in order)
//!
//!   1. Pre-rank        — fast quality×freshness filter, pool → top_k
//!   2. Neural score    — P(engagement | u, i, ctx) for top_k items
//!   3. AESQ constraint — structural multiplier
//!   4. Safety barrier  — hard block B(u,i) ≥ ε_u → score = −∞, item removed
//!   5. Velocity boost  — d(engagement)/dt adjustment
//!   6. Revenue layer   — conversion/LTV adjustment (capped)
//!   7. Re-rank         — diversity spacing → exploration inject → trim

use axum::{extract::State, http::StatusCode, Json};
use chrono::Utc;
use std::collections::HashSet;
use std::sync::Arc;
use std::time::Instant;
use tracing::{info, instrument, warn};

use crate::api::types::*;
use crate::bandit::explorer::inject_exploration_slots;
use crate::bandit::linucb::{build_feature_vector, reward_from_event};
use crate::config::AppConfig;
use crate::contracts::brain_registry::BrainRegistry;
use crate::model_store::ModelStore;
use crate::pipeline::{neural, preranker, reranker, revenue, velocity};
use crate::safety::barrier;
use crate::scoring::aesq::compute_aesq;
use crate::session::cache::SessionFeatureCache;

pub struct AppState {
    pub config: Arc<AppConfig>,
    pub model_store: Arc<ModelStore>,
    pub session_cache: Arc<SessionFeatureCache>,
    pub brain_registry: Arc<BrainRegistry>,
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
    let session_id = req.session_context.session_id.clone();
    let cfg = &state.config;

    info!("rank request received");

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

    // ── Stages 3–6: Per-item scoring pass ────────────────────────────────────
    let mut scored_items: Vec<RankedItem> = candidate_items
        .iter()
        .map(|item| {
            let neural_score = neural_lookup.get(item.content_id.as_str()).copied().unwrap_or(0.0);

            // Stage 3a: AESQ constraint multiplier
            let aesq         = compute_aesq(&req.user_state, item, &cfg.scoring);
            let aesq_mult    = (aesq.total * cfg.pipeline.aesq_constraint_weight
                              + (1.0 - cfg.pipeline.aesq_constraint_weight))
                              .clamp(0.0, 1.0);
            let after_aesq   = neural_score * aesq_mult;

            // Stage 3b: Safety barrier — hard constraint
            let safety = barrier::check(&req.user_state, item, &cfg.safety);
            if safety.blocked {
                return RankedItem {
                    content_id:      item.content_id.clone(),
                    rank:            0,
                    final_score:     cfg.safety.hard_block_score,
                    score_breakdown: ScoreBreakdown {
                        fast_score:      *fast_scores.get(item.content_id.as_str()).unwrap_or(&0.0),
                        neural_score,
                        aesq_alignment:  aesq.alignment,
                        aesq_expansion:  aesq.expansion,
                        aesq_shadow:     aesq.shadow,
                        aesq_quality:    aesq.quality,
                        aesq_freshness:  aesq.freshness,
                        aesq_total:      aesq.total,
                        aesq_multiplier: aesq_mult,
                        velocity_boost:  0.0,
                        revenue_adj:     0.0,
                        final_score:     cfg.safety.hard_block_score,
                    },
                    explanation:      format!(
                        "BLOCKED safety: adult_prob={:.4} drift_prob={:.4} ε_u={:.2e}",
                        safety.adult_probability, safety.drift_probability,
                        req.user_state.safety_epsilon,
                    ),
                    exploration_slot: false,
                    safety_blocked:   true,
                };
            }

            // Stage 4: Velocity boost
            let v_boost      = velocity::velocity_boost(item, &cfg.velocity);
            let phase        = velocity::lifecycle_phase(item, &cfg.velocity);
            let after_vel    = (after_aesq + v_boost).clamp(0.0, 1.0);

            // Stage 5: Revenue adjustment
            let rev_adj      = revenue::revenue_adjustment(item, &req.user_state, &cfg.revenue);
            let final_score  = (after_vel + rev_adj).clamp(0.0, 1.0);

            // Build and cache feature vector for feedback loop
            let fatigue = {
                let seen = req.user_state.interaction_history.iter()
                    .filter(|id| *id == &item.content_id).count() as f64;
                let w = req.user_state.interaction_history.len().max(1) as f64;
                (seen / w).clamp(0.0, 1.0)
            };
            let affinity    = req.user_state.creator_affinities
                .get(&item.creator_id).copied().unwrap_or(0.0) as f64;
            // Realm trust: 0.0 at Realm 1, 1.0 at Realm 5 — blended with creator affinity.
            // Higher realm users have earned more signal fidelity through consistent engagement.
            let realm_trust = ((req.user_state.realm_level.saturating_sub(1)) as f64) / 4.0;
            let creator_boost = (affinity * 0.7 + realm_trust * 0.3).clamp(0.0, 1.0);

            let fv = build_feature_vector(
                aesq.alignment, aesq.expansion, aesq.shadow,
                aesq.quality,   aesq.freshness, creator_boost, 1.0 - fatigue,
            );
            state.session_cache.put(
                &session_id,
                &item.content_id,
                &surface,
                fv.as_slice().to_vec(),
            );

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
                    final_score,
                },
                explanation: format!(
                    "neural={:.3} × aesq_mult={:.3}={:.3} | vel={:+.3} | rev={:+.3} | {:?} | final={:.3}",
                    neural_score, aesq_mult, after_aesq, v_boost, rev_adj, phase, final_score,
                ),
                exploration_slot: false,
                safety_blocked:   false,
            }
        })
        .collect();

    // Remove hard-blocked items
    scored_items.retain(|i| !i.safety_blocked);

    // Sort descending
    scored_items.sort_by(|a, b| {
        b.final_score
            .partial_cmp(&a.final_score)
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    for (i, item) in scored_items.iter_mut().enumerate() {
        item.rank = i + 1;
    }

    // ── Stage 6: Diversity spacing ────────────────────────────────────────────
    let creator_pairs: Vec<(String, String)> = req
        .content_pool
        .iter()
        .map(|c| (c.content_id.clone(), c.creator_id.clone()))
        .collect();
    let scored_items = reranker::apply_diversity(scored_items, &creator_pairs);

    // ── Stage 7: Exploration injection ───────────────────────────────────────
    let ranked_ids: HashSet<String> = scored_items.iter().map(|i| i.content_id.clone()).collect();
    let with_exploration =
        inject_exploration_slots(scored_items, &req.content_pool, explore_count, &ranked_ids);

    let final_items: Vec<RankedItem> = with_exploration
        .into_iter()
        .take(req.session_context.max_items)
        .collect();

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

// ── POST /feedback ────────────────────────────────────────────────────────────

#[instrument(skip_all, fields(
    user_id = %req.user_id,
    surface = %req.surface,
    events  = req.events.len(),
))]
pub async fn feedback_handler(
    State(state): State<Arc<AppState>>,
    Json(req): Json<FeedbackRequest>,
) -> StatusCode {
    info!("feedback received");

    for event in &req.events {
        let reward = reward_from_event(&event.event_type);
        let position_discount = 1.0 / (1.0 + (event.position_at_display as f64).ln().max(0.0));
        let adjusted = reward * position_discount;

        // Recover original feature vector from session cache
        let fv = if let Some(entry) = state.session_cache.get(&req.session_id, &event.content_id) {
            nalgebra::DVector::from_vec(entry.feature_vector)
        } else {
            warn!(
                session_id = %req.session_id,
                content_id = %event.content_id,
                "feature vector not found in session cache — using stub"
            );
            nalgebra::DVector::from_element(state.config.bandit.feature_dim, 0.1_f64)
        };

        state.model_store.update(&req.surface, &fv, adjusted);
    }

    StatusCode::ACCEPTED
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health_handler(State(state): State<Arc<AppState>>) -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "status":        "ok",
        "service":       "aethyrrank-engine",
        "version":       env!("CARGO_PKG_VERSION"),
        "ts":            Utc::now().to_rfc3339(),
        "session_cache": state.session_cache.len(),
    }))
}
