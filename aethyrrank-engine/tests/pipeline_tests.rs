// tests/pipeline_tests.rs — integration tests for the full 7-stage pipeline
// Run with: cargo test --test pipeline_tests

use chrono::Utc;
use std::collections::HashMap;

use aethyrrank_engine::api::types::*;
use aethyrrank_engine::config::*;
use aethyrrank_engine::pipeline::{neural, preranker, revenue, velocity};
use aethyrrank_engine::safety::barrier;

// ── Fixtures ─────────────────────────────────────────────────────────────────

fn sfw_user() -> UserState {
    UserState {
        user_id: "sfw_u".to_string(),
        interest_vector: vec![0.5; 8],
        interaction_history: vec![],
        is_cold_start: false,
        creator_affinities: HashMap::new(),
        safety_epsilon: 1e-6,
        user_tier: "free".to_string(),
        realm_level: 1,
        nexus_shard_id: None,
    }
}

fn default_user() -> UserState {
    UserState {
        user_id: "default_u".to_string(),
        interest_vector: vec![0.5; 8],
        interaction_history: vec![],
        is_cold_start: false,
        creator_affinities: HashMap::new(),
        safety_epsilon: 0.10,
        user_tier: "free".to_string(),
        realm_level: 1,
        nexus_shard_id: None,
    }
}

fn make_item(id: &str, adult_prob: f64, velocity: f64) -> ContentItem {
    ContentItem {
        content_id: id.to_string(),
        creator_id: format!("cr_{}", id),
        topic_vector: vec![0.5; 8],
        published_at: Utc::now(),
        engagement: EngagementSignals {
            likes: 100.0,
            shares: 20.0,
            comments: 30.0,
            saves: 15.0,
            view_time_seconds: 300.0,
            impressions: 1000.0,
        },
        exposure_count: 5000,
        creator_exposure: 50000,
        tags: vec![],
        content_type: "video".to_string(),
        velocity_score: velocity,
        early_retention: 0.65,
        completion_rate: 0.60,
        conversion_probability: 0.10,
        creator_revenue_rate: 0.20,
        ltv_estimate: 0.15,
        adult_probability: adult_prob,
        posts_last_24h: 0,
        self_reply_cadence: 0.0,
        char_count: 280,
        is_in_network: false,
    }
}

fn vel_cfg() -> VelocityConfig {
    VelocityConfig {
        boost_multiplier: 0.20,
        promotion_threshold: 0.70,
        decay_threshold: 0.20,
    }
}

fn rev_cfg() -> RevenueConfig {
    RevenueConfig {
        w_conversion: 0.50,
        w_creator_rate: 0.30,
        w_ltv: 0.20,
        max_adjustment: 0.30,
    }
}

fn safety_cfg() -> SafetyConfig {
    SafetyConfig {
        default_epsilon: 0.10,
        sfw_epsilon: 1e-6,
        hard_block_score: -999999.0,
    }
}

fn prerank_cfg() -> PrerankConfig {
    PrerankConfig {
        top_k: 50,
        min_fast_score: 0.0,
    }
}

// ── Safety barrier tests ──────────────────────────────────────────────────────

#[test]
fn sfw_user_sees_zero_adult_content() {
    let user = sfw_user();
    let adult = make_item("adult_item", 0.80, 0.5);
    let safe = make_item("safe_item", 0.00, 0.5);

    let adult_result = barrier::check(&user, &adult, &safety_cfg());
    let safe_result = barrier::check(&user, &safe, &safety_cfg());

    assert!(
        adult_result.blocked,
        "adult content must be blocked for SFW user"
    );
    assert!(!safe_result.blocked, "clean content must pass SFW user");
}

#[test]
fn sfw_user_blocked_by_drift_even_for_borderline_content() {
    let user = sfw_user();
    let borderline = make_item("border", 0.20, 0.5);
    let result = barrier::check(&user, &borderline, &safety_cfg());
    assert!(
        result.blocked,
        "borderline content must be blocked for SFW user via drift amplification"
    );
}

#[test]
fn default_user_passes_mild_content() {
    let user = default_user();
    let mild = make_item("mild", 0.03, 0.5);
    let result = barrier::check(&user, &mild, &safety_cfg());
    assert!(
        !result.blocked,
        "mild content (adult_prob=0.03) should pass default user"
    );
}

// ── Velocity layer tests ──────────────────────────────────────────────────────

#[test]
fn viral_content_gets_positive_boost() {
    let item = make_item("viral", 0.0, 0.95);
    let boost = velocity::velocity_boost(&item, &vel_cfg());
    assert!(
        boost > 0.0,
        "viral content should have positive boost: {}",
        boost
    );
}

#[test]
fn decaying_content_gets_negative_boost() {
    // Decay is a composite verdict: the velocity derivative alone does not
    // condemn a work that viewers still retain and complete. A dead work is
    // one that has stopped moving AND stopped holding anyone.
    let mut item = make_item("dead", 0.0, 0.05);
    item.early_retention = 0.10;
    item.completion_rate = 0.05;
    let boost = velocity::velocity_boost(&item, &vel_cfg());
    assert!(
        boost < 0.0,
        "decaying content should have negative boost: {}",
        boost
    );
}

// ── Revenue layer tests ───────────────────────────────────────────────────────

#[test]
fn revenue_capped_for_free_user() {
    let item = make_item("c", 0.0, 0.5);
    let user = default_user(); // "free" tier
    let adj = revenue::revenue_adjustment(&item, &user, &rev_cfg());
    // free tier multiplier is 0.4, max_adjustment=0.3, so max = 0.12
    assert!(
        adj <= 0.12 + 1e-9,
        "free user revenue adj exceeded expected cap: {}",
        adj
    );
}

// ── Pre-ranker tests ──────────────────────────────────────────────────────────

#[test]
fn preranker_respects_top_k() {
    let pool: Vec<ContentItem> = (0..200)
        .map(|i| make_item(&format!("item_{}", i), 0.0, 0.5))
        .collect();
    let result = preranker::prerank(&pool, &prerank_cfg(), Utc::now());
    assert!(
        result.len() <= 50,
        "pre-ranker must respect top_k=50, got {}",
        result.len()
    );
}

// ── Neural model tests ────────────────────────────────────────────────────────

#[test]
fn neural_scores_in_unit_interval() {
    let user = default_user();
    let items: Vec<ContentItem> = (0..5)
        .map(|i| make_item(&format!("n{}", i), 0.0, 0.5))
        .collect();
    let refs: Vec<&ContentItem> = items.iter().collect();
    let scores = neural::score_batch(&user, &refs);
    for s in &scores {
        assert!(
            s.score >= 0.0 && s.score <= 1.0,
            "neural score out of [0,1]: {}",
            s.score
        );
    }
}
