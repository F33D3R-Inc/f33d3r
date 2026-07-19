use crate::api::types::{ContentItem, EngagementSignals, UserState};
use crate::config::ScoringConfig;
use crate::scoring::freshness::compute_freshness;

/// All five AESQ component scores plus the weighted total.
#[derive(Debug, Clone)]
pub struct AesqScore {
    pub alignment: f64,
    pub expansion: f64,
    pub shadow: f64,
    pub quality: f64,
    pub freshness: f64,
    pub total: f64,
}

/// Entry point: compute the full AESQ score for a (user, item) pair.
pub fn compute_aesq(user: &UserState, item: &ContentItem, config: &ScoringConfig) -> AesqScore {
    let a = alignment(&user.interest_vector, &item.topic_vector);
    let e = expansion(&user.interest_vector, &item.topic_vector);
    let s = shadow(item.exposure_count, item.creator_exposure);
    let q_raw = quality(&item.engagement, config);

    // Media multiplier: video/audio/image carries stronger engagement signal.
    let media_mult = match item.content_type.as_str() {
        "video" | "audio" | "image" => 1.5_f64,
        _ => 1.0_f64,
    };

    // Long-form multiplier: effort signal — longer posts get more weight.
    let longform_mult = if item.char_count >= 1000 {
        1.25_f64
    } else if item.char_count >= 280 {
        1.10_f64
    } else {
        1.0_f64
    };

    // Author dilution: burst posting decays per-post signal weight.
    // No penalty up to 4 posts/day; 10% penalty per additional post, floor 0.5.
    let dilution = if item.posts_last_24h > 4 {
        (1.0 - 0.10 * (item.posts_last_24h - 4) as f64).max(0.5)
    } else {
        1.0_f64
    };

    let q = (q_raw * media_mult * longform_mult * dilution).clamp(0.0, 1.0);

    let f = compute_freshness(item.published_at, config.freshness.half_life_hours);

    let w = &config.weights;
    let total = w.w_alignment * a
        + w.w_expansion * e
        + w.w_shadow * s
        + w.w_quality * q
        + w.w_freshness * f;

    AesqScore {
        alignment: a,
        expansion: e,
        shadow: s,
        quality: q,
        freshness: f,
        total: total.clamp(0.0, 1.0),
    }
}

// ── A — Alignment ─────────────────────────────────────────────────────────────
// Cosine similarity between user interest vector and content topic vector.
// Normalised from [-1, 1] to [0, 1].
fn alignment(u: &[f32], c: &[f32]) -> f64 {
    if u.len() != c.len() || u.is_empty() {
        return 0.0;
    }
    let dot: f64 = u
        .iter()
        .zip(c)
        .map(|(a, b)| (*a as f64) * (*b as f64))
        .sum();
    let nu: f64 = u.iter().map(|x| (*x as f64).powi(2)).sum::<f64>().sqrt();
    let nc: f64 = c.iter().map(|x| (*x as f64).powi(2)).sum::<f64>().sqrt();
    if nu == 0.0 || nc == 0.0 {
        return 0.0;
    }
    ((dot / (nu * nc)) + 1.0) / 2.0
}

// ── E — Expansion ─────────────────────────────────────────────────────────────
// Bounded distance reward: Gaussian bump centred at moderate divergence (~0.4).
// Rewards content that is novel but not completely alien to the user's interests.
fn expansion(u: &[f32], c: &[f32]) -> f64 {
    if u.len() != c.len() || u.is_empty() {
        return 0.0;
    }
    let dist: f64 = u
        .iter()
        .zip(c)
        .map(|(a, b)| ((*a as f64) - (*b as f64)).powi(2))
        .sum::<f64>()
        .sqrt();

    // Gaussian with μ=0.4, σ=0.35
    let mu = 0.40_f64;
    let sigma = 0.35_f64;
    (-(dist - mu).powi(2) / (2.0 * sigma.powi(2))).exp()
}

// ── S — Shadow ────────────────────────────────────────────────────────────────
// Boost for underrepresented content and creators.
// Hard-capped at 0.15 per specification.
fn shadow(content_exposure: u64, creator_exposure: u64) -> f64 {
    let content_factor = 1.0 / (1.0 + (content_exposure as f64).ln().max(0.0));
    let creator_factor = 1.0 / (1.0 + (creator_exposure as f64).ln().max(0.0));
    let raw = content_factor * 0.6 + creator_factor * 0.4;
    // Hard cap
    raw.clamp(0.0, 1.0) * 0.15
}

// ── Q — Quality ───────────────────────────────────────────────────────────────
// Weighted engagement rate, normalised to [0, 1] via sigmoid.
fn quality(eng: &EngagementSignals, config: &ScoringConfig) -> f64 {
    if eng.impressions == 0.0 {
        return 0.0;
    }
    let qw = &config.quality;
    let raw = qw.w_like * eng.likes
        + qw.w_share * eng.shares
        + qw.w_comment * eng.comments
        + qw.w_save * eng.saves
        + qw.w_view_time * (eng.view_time_seconds / 60.0);
    // Rate per impression, scaled into sigmoid range
    let rate = raw / eng.impressions;
    sigmoid(rate * 20.0)
}

#[inline]
fn sigmoid(x: f64) -> f64 {
    1.0 / (1.0 + (-x).exp())
}

// ── Unit tests ────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn alignment_identical_vectors() {
        let v = vec![1.0_f32, 0.0, 0.0];
        assert!((alignment(&v, &v) - 1.0).abs() < 1e-6);
    }

    #[test]
    fn alignment_orthogonal_vectors() {
        let a = vec![1.0_f32, 0.0];
        let b = vec![0.0_f32, 1.0];
        assert!((alignment(&a, &b) - 0.5).abs() < 1e-6);
    }

    #[test]
    fn shadow_hard_cap() {
        let s = shadow(0, 0);
        assert!(s <= 0.15 + 1e-9, "shadow exceeded 0.15 cap: {}", s);
    }

    #[test]
    fn shadow_high_exposure_is_low() {
        let s = shadow(1_000_000, 1_000_000);
        assert!(
            s < 0.02,
            "expected near-zero shadow for high exposure: {}",
            s
        );
    }

    #[test]
    fn expansion_zero_distance() {
        let v = vec![0.5_f32; 4];
        let e = expansion(&v, &v);
        // At distance=0, Gaussian peak is at 0.4, so value < 1.0
        assert!(e > 0.0 && e <= 1.0);
    }

    #[test]
    fn quality_no_impressions_is_zero() {
        use crate::config::{FreshnessConfig, QualityConfig, ScoringConfig, WeightConfig};
        let cfg = ScoringConfig {
            weights: WeightConfig {
                w_alignment: 0.3,
                w_expansion: 0.2,
                w_shadow: 0.15,
                w_quality: 0.25,
                w_freshness: 0.1,
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
        };
        let eng = EngagementSignals {
            likes: 0.0,
            shares: 0.0,
            comments: 0.0,
            saves: 0.0,
            view_time_seconds: 0.0,
            impressions: 0.0,
        };
        assert_eq!(quality(&eng, &cfg), 0.0);
    }
}
