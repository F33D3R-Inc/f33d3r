//! Score merging utilities.
//!
//! In v2 the final score is computed additively across pipeline layers
//! directly inside the handler. This module provides utility functions
//! used across layers.

/// Clamp a score to [0, 1].
#[inline]
pub fn clamp_score(s: f64) -> f64 {
    s.clamp(0.0, 1.0)
}

/// Apply a weight-capped additive adjustment.
/// `base` is the current score, `adjustment` is the delta, `cap` is the
/// maximum allowed contribution of this layer as a fraction of base.
#[inline]
pub fn capped_add(base: f64, adjustment: f64, cap: f64) -> f64 {
    let bounded = adjustment.clamp(-cap, cap);
    (base + bounded).clamp(0.0, 1.0)
}
