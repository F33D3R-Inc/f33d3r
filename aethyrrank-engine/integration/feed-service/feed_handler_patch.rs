// feed-service/src/feed_handler.rs — v2 patch
//
// Lines marked NEW are additions for AethyrRank v2.
// All other feed-service logic is unchanged.
//
// Key changes from v1:
//   - UserPayload now includes safety_epsilon and user_tier
//   - ContentCandidate now includes velocity, revenue, and safety fields
//   - Response uses RankedResult instead of plain Vec<String>

use axum::{extract::State, Json};
use chrono::Utc;
use std::sync::Arc;
use uuid::Uuid;
use tracing::warn;

use crate::ranking_client::{
    ContentCandidate, EngagementPayload, RankingClient, UserPayload,
};

pub async fn get_feed(
    State(state): State<Arc<ServiceState>>,
    user_id: String,
    surface: String,
) -> Json<FeedResponse> {
    let request_id = Uuid::new_v4().to_string();

    // ── Step 1: fetch candidates ───────────────────────────────────────────
    let raw = state.content_service
        .fetch_candidates(&user_id, 200)
        .await
        .unwrap_or_default();

    // ── Step 2: fetch user state ───────────────────────────────────────────
    let profile = state.user_service
        .get_profile(&user_id)
        .await
        .unwrap_or_default();

    // ── Step 3: map to v2 AethyrRank payload — NEW fields ─────────────────
    let candidates: Vec<ContentCandidate> = raw.iter().map(|c| ContentCandidate {
        content_id:             c.id.clone(),
        creator_id:             c.creator_id.clone(),
        topic_vector:           c.topic_embedding.clone(),
        published_at:           c.published_at,
        engagement: EngagementPayload {
            likes:             c.stats.likes     as f64,
            shares:            c.stats.shares    as f64,
            comments:          c.stats.comments  as f64,
            saves:             c.stats.saves     as f64,
            view_time_seconds: c.stats.avg_view_time_secs,
            impressions:       c.stats.impressions as f64,
        },
        exposure_count:         c.stats.impressions,
        creator_exposure:       c.creator_total_impressions,
        tags:                   c.tags.clone(),
        content_type:           c.content_type.clone(),
        // NEW: velocity signals (from Redis velocity cache)
        velocity_score:         c.velocity_score,
        early_retention:        c.early_retention,
        completion_rate:        c.completion_rate,
        // NEW: revenue signals (from monetisation service)
        conversion_probability: c.conversion_probability,
        creator_revenue_rate:   c.creator_revenue_rate,
        ltv_estimate:           c.ltv_estimate,
        // NEW: safety classification (from content classifier)
        // IMPORTANT: this MUST be set before calling AethyrRank.
        // Default to 0.0 only if your classifier has confirmed the content is safe.
        adult_probability:      c.adult_probability,
        // Fallback fields
        engagement_rate:        c.stats.engagement_rate,
        freshness_score:        c.freshness_score,
    }).collect();

    // NEW: safety_epsilon derived from account settings
    let safety_epsilon = match profile.content_setting.as_str() {
        "safe_mode"     => 1e-6_f64,
        "adult_enabled" => 1.0_f64,
        _               => 0.10_f64,
    };

    let user = UserPayload {
        user_id:             user_id.clone(),
        interest_vector:     profile.interest_embedding.clone(),
        interaction_history: profile.recent_content_ids.clone(),
        is_cold_start:       profile.is_cold_start,
        creator_affinities:  profile.creator_affinities.clone(),
        safety_epsilon,                        // NEW
        user_tier:           profile.tier.clone(), // NEW: "free"|"subscriber"|"creator"
    };

    let session_ctx = serde_json::json!({
        "surface":    surface,
        "request_id": request_id,
        "session_id": Uuid::new_v4().to_string(),
        "max_items":  50,
        "timestamp":  Utc::now(),
    });

    // ── Step 4: call AethyrRank with graceful fallback ─────────────────────
    let ranked = match state.ranking_client
        .rank(candidates.clone(), user, session_ctx)
        .await
    {
        Ok(results) => results,
        Err(err) => {
            warn!(
                error      = %err,
                request_id = %request_id,
                "AethyrRank unavailable — using fallback ranker"
            );
            RankingClient::fallback_rank(candidates)
        }
    };

    // ── Step 5: hydrate content by ranked order ────────────────────────────
    let ordered_ids: Vec<String> = ranked.iter().map(|r| r.content_id.clone()).collect();
    let feed = state.content_service
        .fetch_by_ids(&ordered_ids)
        .await
        .unwrap_or_default();

    Json(FeedResponse { request_id, items: feed })
}
