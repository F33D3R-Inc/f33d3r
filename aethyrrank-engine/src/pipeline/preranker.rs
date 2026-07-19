//! Stage 1 — Pre-ranker
//!
//! Fast filter that reduces 100–500 candidates to `top_k` (default 100)
//! before the expensive neural scoring pass.
//!
//! Fast score = quality_signal * freshness_score
//!
//! This runs in sub-millisecond time. It uses only pre-computed signals
//! from the content-service (no embedding math, no matrix ops).

use crate::api::types::ContentItem;
use crate::config::PrerankConfig;
use crate::scoring::freshness::compute_freshness;

pub struct PrerankedItem<'a> {
    pub item: &'a ContentItem,
    pub fast_score: f64,
}

/// Filter and sort candidates, returning at most `top_k` items.
/// Items below `min_fast_score` are dropped entirely.
pub fn prerank<'a>(
    pool: &'a [ContentItem],
    config: &PrerankConfig,
    _now: chrono::DateTime<chrono::Utc>,
) -> Vec<PrerankedItem<'a>> {
    let half_life = 24.0_f64; // use freshness default; full config wired in caller

    let mut scored: Vec<PrerankedItem<'a>> = pool
        .iter()
        .filter_map(|item| {
            let q = quick_quality(&item.engagement);
            let f = compute_freshness(item.published_at, half_life);
            let fast_score = q * f;

            if fast_score < config.min_fast_score {
                None
            } else {
                Some(PrerankedItem { item, fast_score })
            }
        })
        .collect();

    // Sort descending by fast_score
    scored.sort_by(|a, b| {
        b.fast_score
            .partial_cmp(&a.fast_score)
            .unwrap_or(std::cmp::Ordering::Equal)
    });

    scored.truncate(config.top_k);
    scored
}

/// Quick quality signal using only engagement counts (no config weights needed).
/// This is intentionally cheap — it runs on every item in the pool.
fn quick_quality(eng: &crate::api::types::EngagementSignals) -> f64 {
    if eng.impressions == 0.0 {
        return 0.0;
    }
    // Weighted engagement per impression
    let raw = eng.likes + 3.0 * eng.shares + 2.0 * eng.comments + 4.0 * eng.saves;
    let rate = raw / eng.impressions;
    // Sigmoid squeeze to [0,1]
    1.0 / (1.0 + (-rate * 20.0).exp())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::types::EngagementSignals;
    use chrono::Utc;

    fn make_item(id: &str, impressions: f64, likes: f64) -> ContentItem {
        ContentItem {
            content_id: id.to_string(),
            creator_id: "c".to_string(),
            topic_vector: vec![0.5; 4],
            published_at: Utc::now(),
            engagement: EngagementSignals {
                likes,
                shares: 0.0,
                comments: 0.0,
                saves: 0.0,
                view_time_seconds: 0.0,
                impressions,
            },
            exposure_count: 100,
            creator_exposure: 1000,
            tags: vec![],
            content_type: "post".to_string(),
            velocity_score: 0.5,
            early_retention: 0.5,
            completion_rate: 0.5,
            conversion_probability: 0.1,
            creator_revenue_rate: 0.1,
            ltv_estimate: 0.1,
            adult_probability: 0.0,
            posts_last_24h: 1,
            self_reply_cadence: 0.0,
            char_count: 0,
        }
    }

    #[test]
    fn drops_low_quality_items() {
        let cfg = PrerankConfig {
            top_k: 10,
            min_fast_score: 0.5,
        };
        let pool = vec![make_item("good", 100.0, 50.0), make_item("bad", 100.0, 0.0)];
        let result = prerank(&pool, &cfg, Utc::now());
        assert!(
            !result.iter().any(|r| r.item.content_id == "bad"),
            "low quality item should be filtered"
        );
    }

    #[test]
    fn respects_top_k() {
        let cfg = PrerankConfig {
            top_k: 2,
            min_fast_score: 0.0,
        };
        let pool: Vec<ContentItem> = (0..10)
            .map(|i| make_item(&i.to_string(), 100.0, 10.0))
            .collect();
        let result = prerank(&pool, &cfg, Utc::now());
        assert!(result.len() <= 2);
    }

    #[test]
    fn sorted_by_fast_score_descending() {
        let cfg = PrerankConfig {
            top_k: 100,
            min_fast_score: 0.0,
        };
        let pool = vec![make_item("low", 100.0, 1.0), make_item("high", 100.0, 99.0)];
        let result = prerank(&pool, &cfg, Utc::now());
        assert_eq!(result[0].item.content_id, "high");
    }
}
