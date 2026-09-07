//! Stage 5 — Revenue-Aware Ranking Layer
//!
//! Integrates monetisation signals into the ranking score without letting
//! revenue considerations dominate user experience.
//!
//! ## Design principles
//!
//! 1. Revenue is additive — it adjusts an existing score, never sets it.
//! 2. Revenue contribution is hard-capped (`max_adjustment` in config).
//! 3. Adjustment is user-tier aware:
//!    - "free" users → lighter revenue weighting (protect UX)
//!    - "subscriber" → balanced
//!    - "creator" → full signal
//!
//! ## Signals (all normalised [0,1] by content-service)
//!
//! - `conversion_probability`: P(paid action | user sees this content)
//! - `creator_revenue_rate`:   creator's historical revenue generation rate
//! - `ltv_estimate`:           expected LTV uplift from engagement

use crate::api::types::{ContentItem, UserState};
use crate::config::RevenueConfig;

/// Compute revenue adjustment ∈ [0, max_adjustment].
/// Always non-negative — revenue never penalises content.
pub fn revenue_adjustment(item: &ContentItem, user: &UserState, config: &RevenueConfig) -> f64 {
    let tier_multiplier = match user.user_tier.as_str() {
        "creator" => 1.0,
        "subscriber" => 0.7,
        _ => 0.4, // "free" or unknown
    };

    let raw = config.w_conversion * item.conversion_probability
        + config.w_creator_rate * item.creator_revenue_rate
        + config.w_ltv * item.ltv_estimate;

    // Normalise (weights sum to 1.0) and cap the full signal first, then apply
    // the tier discount. Discounting before the cap let every tier saturate at
    // the same ceiling on strong signals, which erased the tier distinction the
    // design promises: a free viewer must always see a lighter adjustment than
    // a creator for the same content.
    raw.clamp(0.0, config.max_adjustment) * tier_multiplier
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::types::EngagementSignals;
    use chrono::Utc;
    use std::collections::HashMap;

    fn make_user(tier: &str) -> UserState {
        UserState {
            user_id: "u1".to_string(),
            interest_vector: vec![0.5; 8],
            interaction_history: vec![],
            is_cold_start: false,
            creator_affinities: HashMap::new(),
            safety_epsilon: 0.1,
            user_tier: tier.to_string(),
            realm_level: 1,
            nexus_shard_id: None,
        }
    }

    fn make_item(conv: f64, rate: f64, ltv: f64) -> ContentItem {
        ContentItem {
            content_id: "c".to_string(),
            creator_id: "cr".to_string(),
            topic_vector: vec![0.5; 8],
            published_at: Utc::now(),
            engagement: EngagementSignals {
                likes: 10.0,
                shares: 2.0,
                comments: 3.0,
                saves: 1.0,
                view_time_seconds: 60.0,
                impressions: 100.0,
            },
            exposure_count: 100,
            creator_exposure: 1000,
            tags: vec![],
            content_type: "post".to_string(),
            velocity_score: 0.5,
            early_retention: 0.5,
            completion_rate: 0.5,
            conversion_probability: conv,
            creator_revenue_rate: rate,
            ltv_estimate: ltv,
            adult_probability: 0.0,
            posts_last_24h: 0,
            self_reply_cadence: 0.0,
            char_count: 0,
            is_in_network: false,
        }
    }

    fn cfg() -> RevenueConfig {
        RevenueConfig {
            w_conversion: 0.50,
            w_creator_rate: 0.30,
            w_ltv: 0.20,
            max_adjustment: 0.30,
        }
    }

    #[test]
    fn never_exceeds_cap() {
        let item = make_item(1.0, 1.0, 1.0);
        let user = make_user("creator");
        let adj = revenue_adjustment(&item, &user, &cfg());
        assert!(adj <= 0.30 + 1e-9, "revenue exceeded cap: {}", adj);
    }

    #[test]
    fn free_users_get_lighter_revenue_weight() {
        let item = make_item(1.0, 1.0, 1.0);
        let free = make_user("free");
        let creator = make_user("creator");
        let adj_free = revenue_adjustment(&item, &free, &cfg());
        let adj_creator = revenue_adjustment(&item, &creator, &cfg());
        assert!(
            adj_free < adj_creator,
            "free users should have lower revenue adj"
        );
    }

    #[test]
    fn zero_signals_give_zero_adjustment() {
        let item = make_item(0.0, 0.0, 0.0);
        let user = make_user("creator");
        let adj = revenue_adjustment(&item, &user, &cfg());
        assert!((adj - 0.0).abs() < 1e-9);
    }
}
