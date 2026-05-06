// tests/scoring_tests.rs
//
// Integration-level tests for the AESQ pipeline.
// Run with: cargo test --test scoring_tests

use chrono::Utc;
use std::collections::HashMap;

use aethyrrank_engine::api::types::{ContentItem, EngagementSignals, UserState};
use aethyrrank_engine::config::{FreshnessConfig, QualityConfig, ScoringConfig, WeightConfig};
use aethyrrank_engine::scoring::aesq::compute_aesq;

fn default_config() -> ScoringConfig {
    ScoringConfig {
        weights: WeightConfig {
            w_alignment: 0.30,
            w_expansion: 0.20,
            w_shadow: 0.15,
            w_quality: 0.25,
            w_freshness: 0.10,
        },
        freshness: FreshnessConfig {
            half_life_hours: 24.0,
        },
        quality: QualityConfig {
            w_like: 1.0,
            w_share: 3.0,
            w_comment: 2.0,
            w_save: 4.0,
            w_view_time: 0.5,
        },
    }
}

fn make_user(interest: Vec<f32>) -> UserState {
    UserState {
        user_id: "u1".to_string(),
        interest_vector: interest,
        interaction_history: vec![],
        is_cold_start: false,
        creator_affinities: HashMap::new(),
    }
}

fn make_item(topic: Vec<f32>, exposure: u64) -> ContentItem {
    ContentItem {
        content_id: "c1".to_string(),
        creator_id: "cr1".to_string(),
        topic_vector: topic,
        published_at: Utc::now(),
        engagement: EngagementSignals {
            likes: 100.0,
            shares: 20.0,
            comments: 50.0,
            saves: 30.0,
            view_time_seconds: 120.0,
            impressions: 1000.0,
        },
        exposure_count: exposure,
        creator_exposure: exposure,
        tags: vec![],
        content_type: "article".to_string(),
    }
}

#[test]
fn perfect_alignment_produces_high_score() {
    let cfg = default_config();
    let user = make_user(vec![1.0, 0.0, 0.0, 0.0]);
    let item = make_item(vec![1.0, 0.0, 0.0, 0.0], 100);
    let s = compute_aesq(&user, &item, &cfg);
    assert!(
        s.alignment > 0.95,
        "identical vectors should yield near-1 alignment: {}",
        s.alignment
    );
    assert!(
        s.total > 0.5,
        "total score should be well above 0.5 for perfect alignment"
    );
}

#[test]
fn opposing_vectors_produce_low_alignment() {
    let cfg = default_config();
    let user = make_user(vec![1.0, 0.0]);
    let item = make_item(vec![-1.0, 0.0], 100);
    let s = compute_aesq(&user, &item, &cfg);
    assert!(
        s.alignment < 0.05,
        "opposing vectors should yield near-0 alignment: {}",
        s.alignment
    );
}

#[test]
fn shadow_never_exceeds_cap() {
    let cfg = default_config();
    let user = make_user(vec![0.5; 4]);
    // Zero exposure: maximum possible shadow score
    let item = make_item(vec![0.5; 4], 0);
    let s = compute_aesq(&user, &item, &cfg);
    assert!(
        s.shadow <= 0.15 + 1e-9,
        "shadow exceeded hard cap 0.15: {}",
        s.shadow
    );
}

#[test]
fn fresh_content_beats_stale_content() {
    use chrono::Duration;
    let cfg = default_config();
    let user = make_user(vec![0.5; 4]);

    let mut fresh_item = make_item(vec![0.5; 4], 100);
    fresh_item.published_at = Utc::now();

    let mut stale_item = make_item(vec![0.5; 4], 100);
    stale_item.published_at = Utc::now() - Duration::days(7);

    let fresh_score = compute_aesq(&user, &fresh_item, &cfg);
    let stale_score = compute_aesq(&user, &stale_item, &cfg);

    assert!(
        fresh_score.freshness > stale_score.freshness,
        "fresh content freshness={} should beat stale freshness={}",
        fresh_score.freshness,
        stale_score.freshness,
    );
}

#[test]
fn total_score_within_unit_interval() {
    let cfg = default_config();
    let user = make_user(vec![0.3, 0.7, 0.1, 0.9]);
    let item = make_item(vec![0.9, 0.1, 0.5, 0.2], 50_000);
    let s = compute_aesq(&user, &item, &cfg);
    assert!(
        s.total >= 0.0 && s.total <= 1.0,
        "total out of [0,1]: {}",
        s.total
    );
}
