// tests/linucb_tests.rs
//
// Integration-level tests for the LinUCB bandit layer.
// Run with: cargo test --test linucb_tests

use nalgebra::DVector;

use aethyrrank_engine::api::types::EventType;
use aethyrrank_engine::bandit::linucb::{build_feature_vector, reward_from_event, LinUcbModel};

fn uniform_vec() -> DVector<f64> {
    let v = 1.0 / 7.0_f64.sqrt();
    DVector::from_element(7, v)
}

// ── Model initialisation ──────────────────────────────────────────────────────

#[test]
fn fresh_model_a_is_identity() {
    let m = LinUcbModel::new("feed", 7);
    let identity = nalgebra::DMatrix::<f64>::identity(7, 7);
    assert!(
        (m.a_matrix - identity).norm() < 1e-9,
        "A should start as identity"
    );
}

#[test]
fn fresh_model_b_is_zero() {
    let m = LinUcbModel::new("feed", 7);
    assert!(m.b_vector.norm() < 1e-9, "b should start as zero vector");
}

// ── Scoring ───────────────────────────────────────────────────────────────────

#[test]
fn score_output_in_unit_interval() {
    let m = LinUcbModel::new("feed", 7);
    let x = uniform_vec();
    let (exp, ucb, total) = m.score(&x, 1.0).unwrap();
    assert!(exp >= 0.0 && exp <= 1.0, "expected out of [0,1]: {}", exp);
    assert!(ucb >= 0.0, "ucb_bonus should be non-negative: {}", ucb);
    assert!(
        total >= 0.0 && total <= 1.0,
        "total out of [0,1]: {}",
        total
    );
}

#[test]
fn higher_alpha_gives_larger_ucb_bonus() {
    let m = LinUcbModel::new("feed", 7);
    let x = uniform_vec();
    let (_, ucb_low, _) = m.score(&x, 0.5).unwrap();
    let (_, ucb_high, _) = m.score(&x, 2.0).unwrap();
    assert!(ucb_high > ucb_low, "higher alpha should increase UCB bonus");
}

// ── Online update ─────────────────────────────────────────────────────────────

#[test]
fn repeated_positive_updates_increase_expected_reward() {
    let mut m = LinUcbModel::new("feed", 7);
    let x = uniform_vec();

    let (exp_before, _, _) = m.score(&x, 0.0).unwrap();
    for _ in 0..10 {
        m.update(&x, 1.0);
    }
    let (exp_after, _, _) = m.score(&x, 0.0).unwrap();
    assert!(
        exp_after > exp_before,
        "expected reward should increase after positive updates"
    );
}

#[test]
fn negative_feedback_decreases_expected_reward() {
    let mut m = LinUcbModel::new("feed", 7);
    let x = uniform_vec();

    // First prime with a positive so we have room to observe a decrease
    m.update(&x, 1.0);
    let (exp_before, _, _) = m.score(&x, 0.0).unwrap();

    for _ in 0..20 {
        m.update(&x, -1.0);
    }
    let (exp_after, _, _) = m.score(&x, 0.0).unwrap();
    assert!(
        exp_after < exp_before,
        "negative updates should reduce expected reward"
    );
}

#[test]
fn exploration_bonus_decreases_with_more_observations() {
    let mut m = LinUcbModel::new("feed", 7);
    let x = uniform_vec();
    let (_, ucb_start, _) = m.score(&x, 1.0).unwrap();

    for _ in 0..50 {
        m.update(&x, 0.5);
    }
    let (_, ucb_end, _) = m.score(&x, 1.0).unwrap();
    assert!(
        ucb_end < ucb_start,
        "UCB bonus should shrink as uncertainty reduces"
    );
}

// ── Feature vector ────────────────────────────────────────────────────────────

#[test]
fn feature_vector_has_correct_dimension() {
    let fv = build_feature_vector(0.5, 0.4, 0.1, 0.7, 0.9, 0.3, 0.8);
    assert_eq!(fv.len(), 7);
}

#[test]
fn feature_vector_clamps_out_of_range_values() {
    let fv = build_feature_vector(0.5, 0.4, 0.1, 0.7, 0.9, 1.5, -0.2);
    // creator_boost (idx 5) should be clamped to 1.0
    assert!(
        fv[5] <= 1.0,
        "creator_boost should be clamped to 1.0, got {}",
        fv[5]
    );
    // anti_fatigue (idx 6) should be clamped to 0.0
    assert!(
        fv[6] >= 0.0,
        "anti_fatigue should be clamped to 0.0, got {}",
        fv[6]
    );
}

// ── Reward taxonomy ───────────────────────────────────────────────────────────

#[test]
fn reward_ordering_is_correct() {
    let save = reward_from_event(&EventType::Save);
    let share = reward_from_event(&EventType::Share);
    let like = reward_from_event(&EventType::Like);
    let click = reward_from_event(&EventType::Click);
    let imp = reward_from_event(&EventType::Impression);
    let skip = reward_from_event(&EventType::Skip);
    let neg = reward_from_event(&EventType::NegativeFeedback);

    assert!(save > share, "save > share");
    assert!(share > like, "share > like");
    assert!(like > click, "like > click");
    assert!(click > imp, "click > impression");
    assert!(imp > skip, "impression > skip");
    assert!(skip > neg, "skip > negative_feedback");
    assert!(neg < 0.0, "negative_feedback should be negative");
}
