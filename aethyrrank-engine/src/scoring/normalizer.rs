/// Min-max normalise a mutable slice to [0, 1].
/// No-op if all values are identical (avoids division by zero).
pub fn normalize_slice(values: &mut [f64]) {
    let min = values.iter().cloned().fold(f64::INFINITY, f64::min);
    let max = values.iter().cloned().fold(f64::NEG_INFINITY, f64::max);
    let range = max - min;
    if range == 0.0 {
        return;
    }
    for v in values.iter_mut() {
        *v = (*v - min) / range;
    }
}

/// Per-dimension normalisation of a feature vector given pre-computed bounds.
/// `bounds` is a slice of (min, max) pairs, one per feature dimension.
pub fn normalize_features(x: &mut [f64], bounds: &[(f64, f64)]) {
    debug_assert_eq!(x.len(), bounds.len(), "feature/bounds dimension mismatch");
    for (xi, (lo, hi)) in x.iter_mut().zip(bounds.iter()) {
        let range = hi - lo;
        if range > 0.0 {
            *xi = ((*xi - lo) / range).clamp(0.0, 1.0);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalize_slice_basic() {
        let mut v = vec![0.0, 5.0, 10.0];
        normalize_slice(&mut v);
        assert!((v[0] - 0.0).abs() < 1e-9);
        assert!((v[1] - 0.5).abs() < 1e-9);
        assert!((v[2] - 1.0).abs() < 1e-9);
    }

    #[test]
    fn normalize_slice_uniform_noop() {
        let mut v = vec![3.0, 3.0, 3.0];
        normalize_slice(&mut v);
        assert_eq!(v, vec![3.0, 3.0, 3.0]);
    }

    #[test]
    fn normalize_features_clamps() {
        let mut x = vec![5.0, -1.0];
        normalize_features(&mut x, &[(0.0, 1.0), (0.0, 1.0)]);
        assert_eq!(x[0], 1.0);
        assert_eq!(x[1], 0.0);
    }
}
