// tests/merger_tests.rs — tests for score utility functions
use aethyrrank_engine::merger::{capped_add, clamp_score};

#[test]
fn clamp_score_within_bounds() {
    assert_eq!(clamp_score(1.5), 1.0);
    assert_eq!(clamp_score(-0.5), 0.0);
    assert!((clamp_score(0.7) - 0.7).abs() < 1e-9);
}

#[test]
fn capped_add_respects_cap() {
    // adjustment of 0.5 with cap 0.3 → only 0.3 is applied
    let result = capped_add(0.5, 0.5, 0.3);
    assert!((result - 0.8).abs() < 1e-9);
}

#[test]
fn capped_add_clamps_to_unit_interval() {
    assert!(capped_add(0.9, 0.5, 0.5) <= 1.0);
    assert!(capped_add(0.1, -0.5, 0.5) >= 0.0);
}
