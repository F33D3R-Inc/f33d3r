use crate::api::types::{ContentItem, EngagementSignals, UserState};
use crate::config::ScoringConfig;
use crate::scoring::freshness::compute_freshness;
use crate::scoring::jung::{
    axis_share, dominant_axis, is_psych_vector, top_two_axes, RELEASE, TENSION,
};
use crate::scoring::normalizer::engagement_rate_score;

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
    let s = shadow(
        item.exposure_count,
        item.creator_exposure,
        &user.interest_vector,
        &item.topic_vector,
    );
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
// Cosine similarity between user interest vector and content topic vector,
// normalised from [-1, 1] to [0, 1], plus archetype resonance: a bonus when
// the axis the work is mostly about is one of the two axes the person mostly
// leans on. Cosine measures closeness across the whole space; resonance says
// the work speaks to what this person is chiefly drawn to, which is a
// different — and for a psychological space, the more telling — statement.
// The sum is capped so alignment stays in [0, 1].

/// Bonus applied when the work's dominant axis is one of the person's top two.
const RESONANCE_BONUS: f64 = 0.10;

fn alignment(u: &[f32], c: &[f32]) -> f64 {
    if u.len() != c.len() || u.is_empty() {
        return 0.0;
    }
    let cos = cosine_unit(u, c);
    (cos + archetype_resonance(u, c)).min(1.0)
}

/// Cosine similarity mapped into [0, 1]; 0.0 when either vector is null.
fn cosine_unit(u: &[f32], c: &[f32]) -> f64 {
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

fn archetype_resonance(u: &[f32], c: &[f32]) -> f64 {
    if !is_psych_vector(u) || !is_psych_vector(c) {
        return 0.0;
    }
    let Some(dominant) = dominant_axis(c) else {
        return 0.0;
    };
    if top_two_axes(u).iter().flatten().any(|&a| a == dominant) {
        RESONANCE_BONUS
    } else {
        0.0
    }
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
// Boost for underrepresented content and creators, plus catharsis: Jung's
// compensation principle says the psyche seeks what balances it, so a person
// who leans on Tension more than on Release is given a little of a work that
// is high on Release — the payoff they are short of, not more of the strain
// they already carry. The whole term is hard-capped at 0.15 per specification;
// catharsis lives inside that cap, never on top of it.

/// Hard cap on the shadow term.
const SHADOW_CAP: f64 = 0.15;
/// Share of the raw shadow signal the catharsis bonus may add before the cap.
const CATHARSIS_WEIGHT: f64 = 0.30;

fn shadow(content_exposure: u64, creator_exposure: u64, u: &[f32], c: &[f32]) -> f64 {
    let content_factor = 1.0 / (1.0 + (content_exposure as f64).ln().max(0.0));
    let creator_factor = 1.0 / (1.0 + (creator_exposure as f64).ln().max(0.0));
    let raw = content_factor * 0.6 + creator_factor * 0.4;
    let compensated = raw + CATHARSIS_WEIGHT * catharsis(u, c);
    // Hard cap
    compensated.clamp(0.0, 1.0) * SHADOW_CAP
}

/// How much this work releases what this person is holding: the person's
/// unmet need for release (their Tension share above their Release share)
/// times the work's Release share. Zero for anyone already balanced.
fn catharsis(u: &[f32], c: &[f32]) -> f64 {
    if !is_psych_vector(u) || !is_psych_vector(c) {
        return 0.0;
    }
    let need = (axis_share(u, TENSION) - axis_share(u, RELEASE)).max(0.0);
    need * axis_share(c, RELEASE)
}

// ── Q — Quality ───────────────────────────────────────────────────────────────
// Weighted engagement rate per impression, squashed into [0, 1) with the
// shared engagement curve (zero engagement is exactly 0).
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
    // Rate per impression, through the shared engagement curve
    let rate = raw / eng.impressions;
    engagement_rate_score(rate)
}

// ── Unit tests ────────────────────────────────────────────────────────────────
#[cfg(test)]
mod tests {
    use super::*;

    fn axis(i: usize) -> Vec<f32> {
        let mut v = vec![0.0_f32; 8];
        v[i] = 1.0;
        v
    }

    #[test]
    fn alignment_identical_vectors() {
        let v = axis(0);
        assert!((alignment(&v, &v) - 1.0).abs() < 1e-6);
    }

    #[test]
    fn alignment_orthogonal_vectors() {
        // Orthogonal and non-resonant: the work's dominant axis is not one
        // the person leans on at all, so the score is bare cosine, 0.5.
        let a = axis(0);
        let b = axis(1);
        assert!((alignment(&a, &b) - 0.5).abs() < 1e-6);
    }

    #[test]
    fn alignment_resonance_rewards_dominant_axis_match() {
        // Person: mostly persona, then shadow. Work: mostly shadow. Cosine is
        // partial, resonance adds its bonus on top.
        let mut u = vec![0.1_f32; 8];
        u[0] = 0.9;
        u[1] = 0.6;
        let mut c = vec![0.1_f32; 8];
        c[1] = 0.9;
        let with = alignment(&u, &c);
        let mut c_off = vec![0.1_f32; 8];
        c_off[5] = 0.9;
        let without = alignment(&u, &c_off);
        assert!(
            with > without,
            "resonant work should outscore non-resonant: {with} vs {without}"
        );
        assert!(with <= 1.0);
    }

    #[test]
    fn shadow_hard_cap() {
        let s = shadow(0, 0, &[0.5; 8], &[0.5; 8]);
        assert!(s <= 0.15 + 1e-9, "shadow exceeded 0.15 cap: {}", s);
        // Maximum catharsis on top of maximum novelty still respects the cap.
        let s = shadow(0, 0, &axis(6), &axis(7));
        assert!(
            s <= 0.15 + 1e-9,
            "shadow exceeded 0.15 cap with catharsis: {}",
            s
        );
    }

    #[test]
    fn shadow_high_exposure_is_low() {
        let s = shadow(1_000_000, 1_000_000, &[0.5; 8], &[0.5; 8]);
        assert!(
            s < 0.02,
            "expected near-zero shadow for high exposure: {}",
            s
        );
    }

    #[test]
    fn catharsis_lifts_release_for_tense_user() {
        let tense = axis(6);
        let releasing = axis(7);
        let neutral = vec![0.5_f32; 8];
        let with = shadow(1_000, 10_000, &tense, &releasing);
        let without = shadow(1_000, 10_000, &neutral, &releasing);
        assert!(
            with > without,
            "tense user should get catharsis bonus: {with} vs {without}"
        );
        assert_eq!(catharsis(&neutral, &releasing), 0.0);
    }

    #[test]
    fn expansion_zero_distance() {
        let v = vec![0.5_f32; 8];
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
