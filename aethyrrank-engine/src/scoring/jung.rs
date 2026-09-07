//! The Jung axes — AethyrRank's copy of the shared coordinate system.
//!
//! Zior defines the eight axes (zior-engine/src/jung/axes.rs) and writes
//! topic vectors for audio in this order; feed-engine (internal/jung) writes
//! them for every other work and keeps each person's interest vector in the
//! same space. This module names the axes by position so the scoring layer
//! can reason about *which* axis a person or a work leans on, not only how
//! close two vectors are.
//!
//! Dimension 8 is the contract. A vector of any other length is not a
//! psychological vector and the request handler rejects it before scoring.

/// The dimensionality of the psychological space.
pub const PSYCH_DIM: usize = 8;

/// Canonical axis names, in canonical order. Must match Zior's `AXIS_NAMES`.
pub const AXIS_NAMES: [&str; PSYCH_DIM] = [
    "persona",
    "shadow",
    "agency",
    "integration",
    "attachment",
    "disruption",
    "tension",
    "release",
];

pub const PERSONA: usize = 0;
pub const SHADOW: usize = 1;
pub const AGENCY: usize = 2;
pub const INTEGRATION: usize = 3;
pub const ATTACHMENT: usize = 4;
pub const DISRUPTION: usize = 5;
pub const TENSION: usize = 6;
pub const RELEASE: usize = 7;

/// True when `v` is a well-formed psych vector: exactly `PSYCH_DIM` finite
/// coordinates.
pub fn is_psych_vector(v: &[f32]) -> bool {
    v.len() == PSYCH_DIM && v.iter().all(|x| x.is_finite())
}

/// The axis with the largest coordinate, or `None` when no coordinate is
/// strictly positive — a vector with nothing on it has no dominant axis.
pub fn dominant_axis(v: &[f32]) -> Option<usize> {
    let mut best: Option<(usize, f32)> = None;
    for (i, &x) in v.iter().enumerate() {
        if x > 0.0 && best.map_or(true, |(_, b)| x > b) {
            best = Some((i, x));
        }
    }
    best.map(|(i, _)| i)
}

/// The two axes with the largest coordinates, largest first. Only strictly
/// positive coordinates qualify, so a vector with one non-zero axis yields
/// one entry and a zero vector yields none.
pub fn top_two_axes(v: &[f32]) -> [Option<usize>; 2] {
    let first = dominant_axis(v);
    let second = first.and_then(|f| {
        let mut best: Option<(usize, f32)> = None;
        for (i, &x) in v.iter().enumerate() {
            if i != f && x > 0.0 && best.map_or(true, |(_, b)| x > b) {
                best = Some((i, x));
            }
        }
        best.map(|(i, _)| i)
    });
    [first, second]
}

/// The coordinate on `axis` as a share of the vector's largest coordinate,
/// in [0, 1]. Stored vectors are unit length, so a raw coordinate says
/// little on its own; the share says how much of this vector's character
/// the axis carries.
pub fn axis_share(v: &[f32], axis: usize) -> f64 {
    if axis >= v.len() {
        return 0.0;
    }
    let max = v.iter().cloned().fold(0.0_f32, f32::max);
    if max <= 0.0 {
        return 0.0;
    }
    (v[axis].max(0.0) / max) as f64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dominant_axis_of_zero_vector_is_none() {
        assert_eq!(dominant_axis(&[0.0; PSYCH_DIM]), None);
    }

    #[test]
    fn top_two_orders_by_magnitude() {
        let mut v = [0.1_f32; PSYCH_DIM];
        v[TENSION] = 0.9;
        v[RELEASE] = 0.5;
        assert_eq!(top_two_axes(&v), [Some(TENSION), Some(RELEASE)]);
    }

    #[test]
    fn single_axis_vector_has_one_top_axis() {
        let mut v = [0.0_f32; PSYCH_DIM];
        v[AGENCY] = 1.0;
        assert_eq!(top_two_axes(&v), [Some(AGENCY), None]);
    }

    #[test]
    fn axis_share_is_relative_to_max() {
        let mut v = [0.0_f32; PSYCH_DIM];
        v[SHADOW] = 0.4;
        v[PERSONA] = 0.2;
        assert!((axis_share(&v, SHADOW) - 1.0).abs() < 1e-6);
        assert!((axis_share(&v, PERSONA) - 0.5).abs() < 1e-6);
        assert_eq!(axis_share(&v, RELEASE), 0.0);
    }
}
