//! Jungian Psychological Axes — the shared language between Zior and AethyrRank.
//!
//! This is the key architectural decision: Zior maps audio features onto the
//! same 8-dimensional psychological space that AethyrRank uses for user interest
//! vectors. This means a track's "topic_vector" and a user's "interest_vector"
//! live in the same space — dot product = direct compatibility.
//!
//! ## The 8 axes
//!
//! ```text
//! [0] Persona      — Social performance, craft visibility, production polish
//! [1] Shadow       — Raw depth, darkness, unresolved tension, emotional truth
//! [2] Agency       — Energy, drive, momentum, hero arc, BPM intensity
//! [3] Integration  — Resolution, harmony, tonal completeness, self-coherence
//! [4] Attachment   — Intimacy, vulnerability, vocal warmth, anima/animus
//! [5] Disruption   — Chaos, surprise, genre-fluidity, trickster energy
//! [6] Tension      — Dissonance, dynamic contrast, unresolved harmonic space
//! [7] Release      — Catharsis, drop payoff, major resolution, euphoria
//! ```
//!
//! ## Why this works for music
//!
//! A 140 BPM minor key bass-heavy track with distorted vocals maps to:
//! high Shadow [1], high Agency [2], high Tension [6], low Attachment [4].
//!
//! A slow major-key piano ballad maps to:
//! low Agency [2], high Attachment [4], high Integration [3], high Release [7].
//!
//! A user who engages deeply with introspective indie maps to high Shadow [1],
//! high Attachment [4]. The dot product with the ballad is high. The ranker
//! surfaces it correctly — not because of genre labels, but because the
//! psychological signatures align.

/// The 8 axis names in canonical order.
pub const AXIS_NAMES: [&str; 8] = [
    "persona",
    "shadow",
    "agency",
    "integration",
    "attachment",
    "disruption",
    "tension",
    "release",
];

/// A normalised 8-dimensional psychological vector.
/// All values are in [0, 1].
#[derive(Debug, Clone)]
pub struct PsychVector(pub [f32; 8]);

impl PsychVector {
    pub fn zero() -> Self {
        Self([0.0; 8])
    }

    /// Normalise to unit length.
    pub fn normalise(&mut self) {
        let norm: f32 = self.0.iter().map(|x| x * x).sum::<f32>().sqrt();
        if norm > 1e-8 {
            self.0.iter_mut().for_each(|x| *x /= norm);
        }
    }

    /// Weighted blend of two vectors.
    pub fn blend(a: &Self, b: &Self, weight_b: f32) -> Self {
        let wa = 1.0 - weight_b;
        let mut out = [0.0_f32; 8];
        for i in 0..8 {
            out[i] = (a.0[i] * wa + b.0[i] * weight_b).clamp(0.0, 1.0);
        }
        Self(out)
    }

    pub fn as_slice(&self) -> &[f32; 8] {
        &self.0
    }

    pub fn to_vec(&self) -> Vec<f32> {
        self.0.to_vec()
    }
}

impl Default for PsychVector {
    fn default() -> Self {
        Self::zero()
    }
}
