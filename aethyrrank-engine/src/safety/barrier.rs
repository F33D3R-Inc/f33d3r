//! Safety Barrier System
//!
//! Implements the hard constraint:
//!
//! ```text
//!     B(u, i) < ε_u
//! ```
//!
//! Where:
//!   B(u, i) = P(item belongs to adult content manifold | user context)
//!   ε_u     = user-specific safety bound
//!
//! If the constraint is violated, the item's score is set to hard_block_score
//! (effectively -∞), removing it from the ranked output entirely.
//!
//! ## Two-stage filtering
//!
//! Stage 1 — semantic classifier:
//!   `adult_probability` on ContentItem (set by content-service before calling AethyrRank).
//!   This catches direct adult content.
//!
//! Stage 2 — behavioral drift guard:
//!   `drift_probability`: estimated P(user → adult-state | this content).
//!   This catches "soft drift" — adjacent content that doesn't trigger the
//!   semantic classifier but nudges the user's state toward adult content.
//!   Currently approximated from topic vector distance to known adult manifold.
//!   In production: replace with a trained sequence classifier.
//!
//! ## User safety tiers
//!
//! | User setting        | ε_u         | Meaning                           |
//! |---------------------|-------------|-----------------------------------|
//! | safe_mode / SFW     | 1e-6        | Near-zero accidental exposure     |
//! | default             | 0.10        | Standard content platform norms   |
//! | adult_enabled       | 1.0         | No restriction (creator accounts) |
//!
//! The `safety_epsilon` field on `UserState` carries this value.
//! It is set by the caller (feed-service) based on account settings.
//!
//! ## Critical property
//!
//! This is a HARD BARRIER, not a soft weight. There is no configuration that
//! allows gradual drift into blocked content for protected users.
//! The block is binary: pass or -∞.

use crate::api::types::{ContentItem, UserState};
use crate::config::SafetyConfig;

/// Result of the safety check for a single item.
#[derive(Debug, Clone)]
pub struct SafetyResult {
    /// True if this item is blocked for this user
    pub blocked: bool,
    /// The combined adult probability estimate (max of both stages)
    pub adult_probability: f64,
    /// The drift probability estimate (stage 2)
    pub drift_probability: f64,
}

/// Check whether item `i` passes the safety barrier for user `u`.
///
/// Returns `SafetyResult`. If `blocked = true`, the caller MUST apply
/// `hard_block_score` to the item — do not rank it.
pub fn check(user: &UserState, item: &ContentItem, config: &SafetyConfig) -> SafetyResult {
    // Stage 1: direct semantic classifier score
    let semantic_prob = item.adult_probability;

    // Stage 2: behavioral drift approximation
    // In production: call a trained sequence model that takes the user's
    // recent interaction history and estimates the conditional probability
    // that showing this item shifts the user's latent state toward
    // the adult content manifold.
    //
    // Current stub: uses the semantic probability with a small drift multiplier
    // to model that even "borderline" content can cause cumulative drift.
    let drift_probability = estimate_drift(item, config);

    // Combined barrier estimate: take the maximum (most conservative)
    let combined = semantic_prob.max(drift_probability);

    let blocked = combined >= user.safety_epsilon;

    SafetyResult {
        blocked,
        adult_probability: combined,
        drift_probability,
    }
}

/// Estimate the behavioral drift probability.
///
/// Replace this with a trained model in production. The stub uses
/// the item's semantic adult_probability with a calibrated amplifier —
/// adjacent content (0.1–0.3 adult_prob) is considered to carry non-trivial
/// drift risk and is amplified accordingly to prevent "soft grooming" into
/// adjacent spaces.
fn estimate_drift(item: &ContentItem, _config: &SafetyConfig) -> f64 {
    let p = item.adult_probability;
    if p < 0.05 {
        // Clean content: minimal drift risk
        p * 0.5
    } else if p < 0.30 {
        // Adjacent / borderline: amplify drift estimate
        p * 1.8
    } else {
        // Clearly adult: drift probability essentially equals semantic prob
        p
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::types::EngagementSignals;
    use chrono::Utc;
    use std::collections::HashMap;

    fn cfg() -> SafetyConfig {
        SafetyConfig {
            default_epsilon: 0.10,
            sfw_epsilon: 0.000001,
            hard_block_score: -999999.0,
        }
    }

    fn make_user(epsilon: f64) -> UserState {
        UserState {
            user_id: "u1".to_string(),
            interest_vector: vec![0.5; 8],
            interaction_history: vec![],
            is_cold_start: false,
            creator_affinities: HashMap::new(),
            safety_epsilon: epsilon,
            user_tier: "free".to_string(),
            realm_level: 1,
            nexus_shard_id: None,
        }
    }

    fn make_item(adult_prob: f64) -> ContentItem {
        ContentItem {
            content_id: "c".to_string(),
            creator_id: "cr".to_string(),
            topic_vector: vec![0.5; 8],
            published_at: Utc::now(),
            engagement: EngagementSignals {
                likes: 10.0,
                shares: 1.0,
                comments: 2.0,
                saves: 1.0,
                view_time_seconds: 30.0,
                impressions: 100.0,
            },
            exposure_count: 100,
            creator_exposure: 1000,
            tags: vec![],
            content_type: "image".to_string(),
            velocity_score: 0.5,
            early_retention: 0.5,
            completion_rate: 0.5,
            conversion_probability: 0.1,
            creator_revenue_rate: 0.1,
            ltv_estimate: 0.1,
            adult_probability: adult_prob,
            posts_last_24h: 0,
            self_reply_cadence: 0.0,
            char_count: 0,
            is_in_network: false,
        }
    }

    #[test]
    fn clean_content_passes_sfw_user() {
        let user = make_user(cfg().sfw_epsilon);
        let item = make_item(0.0);
        let result = check(&user, &item, &cfg());
        assert!(!result.blocked, "clean content should pass sfw user");
    }

    #[test]
    fn adult_content_blocked_for_sfw_user() {
        let user = make_user(cfg().sfw_epsilon);
        let item = make_item(0.80);
        let result = check(&user, &item, &cfg());
        assert!(result.blocked, "adult content must be blocked for sfw user");
    }

    #[test]
    fn borderline_content_blocked_for_sfw_user() {
        // 0.15 adult_prob → drift amplification → blocked at epsilon=1e-6
        let user = make_user(cfg().sfw_epsilon);
        let item = make_item(0.15);
        let result = check(&user, &item, &cfg());
        assert!(
            result.blocked,
            "borderline content should be blocked for sfw user via drift"
        );
    }

    #[test]
    fn adult_content_passes_permissive_user() {
        let user = make_user(1.0); // adult_enabled
        let item = make_item(0.95);
        let result = check(&user, &item, &cfg());
        assert!(
            !result.blocked,
            "adult content should pass for permissive user"
        );
    }

    #[test]
    fn default_user_allows_mild_content() {
        let user = make_user(0.10);
        let item = make_item(0.03);
        let result = check(&user, &item, &cfg());
        assert!(!result.blocked, "mild content should pass default user");
    }
}
