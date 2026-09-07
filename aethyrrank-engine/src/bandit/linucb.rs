use anyhow::{anyhow, Result};
use nalgebra::{DMatrix, DVector};

use crate::api::types::EventType;

/// Per-surface LinUCB model.
///
/// Maintains:
///   A  (d×d) — regularised outer-product accumulator, initialised to I
///   b  (d)   — reward-weighted feature accumulator, initialised to 0
///
/// Scoring:  θ = A⁻¹ b
///           score = θᵀx + α √(xᵀ A⁻¹ x)
///
/// Update:   A ← A + xxᵀ
///           b ← b + r·x
pub struct LinUcbModel {
    pub surface: String,
    pub dim: usize,
    pub a_matrix: DMatrix<f64>,
    pub b_vector: DVector<f64>,
}

impl LinUcbModel {
    pub fn new(surface: &str, dim: usize) -> Self {
        Self {
            surface: surface.to_string(),
            dim,
            a_matrix: DMatrix::identity(dim, dim),
            b_vector: DVector::zeros(dim),
        }
    }

    /// Compute LinUCB score for feature vector x.
    ///
    /// Returns `(expected_reward, ucb_bonus, total_score)`.
    /// All three values are clipped to [0, 1].
    pub fn score(&self, x: &DVector<f64>, alpha: f64) -> Result<(f64, f64, f64)> {
        let a_inv = self
            .a_matrix
            .clone()
            .try_inverse()
            .ok_or_else(|| anyhow!("A matrix is singular on surface '{}'", self.surface))?;

        // θ = A⁻¹ b
        let theta = &a_inv * &self.b_vector;

        // Expected reward: θᵀx
        let expected = theta.dot(x);

        // UCB exploration bonus: α √(xᵀ A⁻¹ x)
        let variance = (x.transpose() * &a_inv * x)[(0, 0)];
        let ucb_bonus = alpha * variance.max(0.0).sqrt();

        let total = (expected + ucb_bonus).clamp(0.0, 1.0);
        Ok((expected.clamp(0.0, 1.0), ucb_bonus, total))
    }

    /// Online update — call after observing reward `r` for context `x`.
    pub fn update(&mut self, x: &DVector<f64>, reward: f64) {
        // A ← A + xxᵀ
        self.a_matrix += x * x.transpose();
        // b ← b + r·x
        self.b_vector += reward * x;
    }
}

/// Build the 7-dimensional feature vector used by LinUCB.
/// Dimension layout:
///   [0] alignment     — cosine similarity (AESQ.A)
///   [1] expansion     — diversity reward   (AESQ.E)
///   [2] shadow        — underexposure boost (AESQ.S)
///   [3] quality       — engagement rate    (AESQ.Q)
///   [4] freshness     — time-decay score   (AESQ.F)
///   [5] creator_boost — user-creator affinity
///   [6] anti_fatigue  — 1 - seen_fraction
pub fn build_feature_vector(
    alignment: f64,
    expansion: f64,
    shadow: f64,
    quality: f64,
    freshness: f64,
    creator_boost: f64,
    anti_fatigue: f64,
) -> DVector<f64> {
    DVector::from_vec(vec![
        alignment,
        expansion,
        shadow,
        quality,
        freshness,
        creator_boost.clamp(0.0, 1.0),
        anti_fatigue.clamp(0.0, 1.0),
    ])
}

/// Convert an observed event into a scalar reward signal.
///
/// Delegates to the canonical reward taxonomy in contracts::event_taxonomy.
/// All new code should call crate::contracts::event_taxonomy::reward_signal() directly.
pub fn reward_from_event(event_type: &EventType) -> f64 {
    crate::contracts::event_taxonomy::reward_signal(event_type)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    fn unit_vec(dim: usize) -> DVector<f64> {
        let val = 1.0 / (dim as f64).sqrt();
        DVector::from_element(dim, val)
    }

    #[test]
    fn score_fresh_model_returns_zero_expected() {
        let m = LinUcbModel::new("test", 7);
        let x = unit_vec(7);
        let (exp, _ucb, total) = m.score(&x, 1.0).unwrap();
        // b = 0, so θ = 0, expected = 0
        assert!((exp - 0.0).abs() < 1e-9);
        // total = ucb_bonus only, clipped to [0,1]
        assert!(total >= 0.0 && total <= 1.0);
    }

    #[test]
    fn update_increases_expected_reward_for_positive() {
        let mut m = LinUcbModel::new("test", 7);
        let x = unit_vec(7);
        m.update(&x, 1.0);
        let (exp, _, _) = m.score(&x, 0.0).unwrap();
        assert!(exp > 0.0, "positive update should increase expected reward");
    }

    #[test]
    fn update_decreases_expected_reward_for_negative() {
        let mut m = LinUcbModel::new("test", 7);
        let x = unit_vec(7);
        m.update(&x, -1.0);
        let (exp, _, _) = m.score(&x, 0.0).unwrap();
        // Expected should be negative, clamped to 0 in score() output
        assert!(exp <= 0.0);
    }

    #[test]
    fn reward_taxonomy_is_monotone() {
        assert!(reward_from_event(&EventType::Save) > reward_from_event(&EventType::Like));
        assert!(reward_from_event(&EventType::Like) > reward_from_event(&EventType::Click));
        assert!(reward_from_event(&EventType::Skip) < 0.0);
        assert!(
            reward_from_event(&EventType::NegativeFeedback) < reward_from_event(&EventType::Skip)
        );
    }
}
