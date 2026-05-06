//! Stage 4 — Content Velocity Layer
//!
//! Implements TikTok-style content testing: content moves through lifecycle
//! phases based on engagement velocity (d(engagement)/dt).
//!
//! ## Lifecycle phases
//!
//!   test_cohort → expanding → viral → decaying
//!
//! High-velocity content receives a score boost. Declining content is
//! penalised. This creates a self-evolving distribution without manual curation.
//!
//! ## Signal source
//!
//! `velocity_score` on `ContentItem` is pre-computed by the content-service
//! (normalised d(engagement)/dt over a sliding window). AethyrRank consumes
//! it as a feature — we do not compute the derivative here.
//!
//! `early_retention` and `completion_rate` are strong independent signals
//! that complement the velocity score.

use crate::api::types::ContentItem;
use crate::config::VelocityConfig;

/// Compute the velocity boost adjustment for a single item.
/// Returns a delta in [−boost_max, +boost_max] to be added to the base score.
pub fn velocity_boost(item: &ContentItem, config: &VelocityConfig) -> f64 {
    let v = composite_velocity(item);

    if v >= config.promotion_threshold {
        // High velocity → positive boost
        let intensity =
            (v - config.promotion_threshold) / (1.0 - config.promotion_threshold).max(1e-9);
        intensity.clamp(0.0, 1.0) * config.boost_multiplier
    } else if v <= config.decay_threshold {
        // Declining content → negative adjustment
        let decay_intensity = (config.decay_threshold - v) / config.decay_threshold.max(1e-9);
        -decay_intensity.clamp(0.0, 1.0) * (config.boost_multiplier * 0.5)
    } else {
        // Mid-range: no boost, no penalty
        0.0
    }
}

/// Composite velocity from three signals.
/// `velocity_score` carries most of the signal; retention and completion
/// are strong independent predictors of sustained growth.
fn composite_velocity(item: &ContentItem) -> f64 {
    (0.50 * item.velocity_score + 0.25 * item.early_retention + 0.25 * item.completion_rate)
        .clamp(0.0, 1.0)
}

/// Content lifecycle phase — informational, included in explanation string.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum LifecyclePhase {
    TestCohort,
    Expanding,
    Viral,
    Stable,
    Decaying,
}

pub fn lifecycle_phase(item: &ContentItem, config: &VelocityConfig) -> LifecyclePhase {
    let v = composite_velocity(item);
    if item.exposure_count < 500 {
        LifecyclePhase::TestCohort
    } else if v >= 0.90 {
        LifecyclePhase::Viral
    } else if v >= config.promotion_threshold {
        LifecyclePhase::Expanding
    } else if v <= config.decay_threshold {
        LifecyclePhase::Decaying
    } else {
        LifecyclePhase::Stable
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::types::EngagementSignals;
    use chrono::Utc;

    fn make_item(velocity: f64, retention: f64, completion: f64, exposure: u64) -> ContentItem {
        ContentItem {
            content_id: "c".to_string(),
            creator_id: "cr".to_string(),
            topic_vector: vec![0.5; 4],
            published_at: Utc::now(),
            engagement: EngagementSignals {
                likes: 10.0,
                shares: 2.0,
                comments: 3.0,
                saves: 1.0,
                view_time_seconds: 60.0,
                impressions: 100.0,
            },
            exposure_count: exposure,
            creator_exposure: 1000,
            tags: vec![],
            content_type: "video".to_string(),
            velocity_score: velocity,
            early_retention: retention,
            completion_rate: completion,
            conversion_probability: 0.1,
            creator_revenue_rate: 0.1,
            ltv_estimate: 0.1,
            adult_probability: 0.0,
        }
    }

    fn cfg() -> VelocityConfig {
        VelocityConfig {
            boost_multiplier: 0.20,
            promotion_threshold: 0.70,
            decay_threshold: 0.20,
        }
    }

    #[test]
    fn high_velocity_gives_positive_boost() {
        let item = make_item(0.95, 0.90, 0.88, 10_000);
        let boost = velocity_boost(&item, &cfg());
        assert!(boost > 0.0, "high velocity should boost: {}", boost);
    }

    #[test]
    fn low_velocity_gives_negative_boost() {
        let item = make_item(0.05, 0.10, 0.08, 50_000);
        let boost = velocity_boost(&item, &cfg());
        assert!(boost < 0.0, "decaying content should penalise: {}", boost);
    }

    #[test]
    fn mid_velocity_is_neutral() {
        let item = make_item(0.50, 0.50, 0.50, 5_000);
        let boost = velocity_boost(&item, &cfg());
        assert!(
            (boost - 0.0).abs() < 1e-9,
            "mid velocity should be 0: {}",
            boost
        );
    }

    #[test]
    fn new_content_is_test_cohort() {
        let item = make_item(0.8, 0.8, 0.8, 100);
        let phase = lifecycle_phase(&item, &cfg());
        assert_eq!(phase, LifecyclePhase::TestCohort);
    }
}
