use chrono::{DateTime, Utc};

/// Exponential time-decay freshness score in [0, 1].
///
/// f(age) = e^(-λ * age_hours)
/// where λ = ln(2) / half_life_hours
///
/// At age=0          → 1.0  (brand new)
/// At age=half_life  → 0.5
/// At age=2*half_life → 0.25
pub fn compute_freshness(published_at: DateTime<Utc>, half_life_hours: f64) -> f64 {
    let age_hours = (Utc::now() - published_at).num_seconds().max(0) as f64 / 3600.0;

    let lambda = std::f64::consts::LN_2 / half_life_hours;
    (-lambda * age_hours).exp()
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Duration;

    #[test]
    fn fresh_content_scores_near_one() {
        let now = Utc::now();
        let score = compute_freshness(now, 24.0);
        assert!(
            score > 0.99,
            "just-published content should score ~1.0, got {}",
            score
        );
    }

    #[test]
    fn half_life_halves_score() {
        let published = Utc::now() - Duration::hours(24);
        let score = compute_freshness(published, 24.0);
        assert!(
            (score - 0.5).abs() < 0.01,
            "score at half-life should be ~0.5, got {}",
            score
        );
    }

    #[test]
    fn old_content_approaches_zero() {
        let published = Utc::now() - Duration::days(30);
        let score = compute_freshness(published, 24.0);
        assert!(
            score < 0.001,
            "30-day-old content should be near 0, got {}",
            score
        );
    }
}
