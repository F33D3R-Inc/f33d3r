use rand::Rng;
use std::collections::HashSet;

use crate::api::types::{ContentItem, RankedItem, ScoreBreakdown};

/// Inject `explore_count` exploration items into a ranked list.
///
/// Selection criteria (in priority order):
///   1. New creators  (low creator_exposure)
///   2. Low-exposure content (low exposure_count)
///
/// Exploration items are inserted into a random position in the bottom half
/// of the ranked list so they do not dominate the top of the feed.
/// Each injected item is flagged with `exploration_slot: true`.
pub fn inject_exploration_slots(
    mut ranked: Vec<RankedItem>,
    content_pool: &[ContentItem],
    explore_count: usize,
    already_ranked: &HashSet<String>,
) -> Vec<RankedItem> {
    if explore_count == 0 {
        return ranked;
    }

    let mut rng = rand::thread_rng();

    // Collect candidates not already in the ranked list
    let mut candidates: Vec<&ContentItem> = content_pool
        .iter()
        .filter(|c| !already_ranked.contains(&c.content_id))
        .collect();

    // Sort by exploration priority descending (lower exposure = higher priority)
    candidates.sort_by(|a, b| {
        exploration_priority(b)
            .partial_cmp(&exploration_priority(a))
            .unwrap_or(std::cmp::Ordering::Equal)
    });

    let n = explore_count.min(candidates.len());

    for item in &candidates[..n] {
        let list_len = ranked.len();
        let bottom_start = list_len / 2;
        let insert_pos = if list_len > bottom_start {
            rng.gen_range(bottom_start..=list_len)
        } else {
            list_len
        };

        ranked.insert(
            insert_pos,
            RankedItem {
                content_id: item.content_id.clone(),
                rank: insert_pos + 1,
                final_score: 0.0,
                score_breakdown: ScoreBreakdown::default(),
                explanation: format!(
                    "Exploration slot: creator_exposure={} content_exposure={}",
                    item.creator_exposure, item.exposure_count,
                ),
                exploration_slot: true,
                safety_blocked: false,
            },
        );
    }

    // Re-index ranks after insertion
    for (i, item) in ranked.iter_mut().enumerate() {
        item.rank = i + 1;
    }

    ranked
}

/// Higher return value = higher exploration priority.
fn exploration_priority(item: &ContentItem) -> f64 {
    let creator_score = 1.0 / (1.0 + (item.creator_exposure as f64).ln().max(0.0));
    let content_score = 1.0 / (1.0 + (item.exposure_count as f64).ln().max(0.0));
    // Weight creator novelty more heavily (0.6/0.4 split)
    creator_score * 0.6 + content_score * 0.4
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::types::EngagementSignals;
    use chrono::Utc;

    fn make_item(id: &str, creator_exposure: u64, exposure_count: u64) -> ContentItem {
        ContentItem {
            content_id: id.to_string(),
            creator_id: format!("creator_{}", id),
            topic_vector: vec![0.5; 4],
            published_at: Utc::now(),
            engagement: EngagementSignals {
                likes: 0.0,
                shares: 0.0,
                comments: 0.0,
                saves: 0.0,
                view_time_seconds: 0.0,
                impressions: 1.0,
            },
            exposure_count,
            creator_exposure,
            tags: vec![],
            content_type: "article".to_string(),
            velocity_score: 0.5,
            early_retention: 0.5,
            completion_rate: 0.5,
            conversion_probability: 0.1,
            creator_revenue_rate: 0.1,
            ltv_estimate: 0.1,
            adult_probability: 0.0,
        }
    }

    #[test]
    fn injects_correct_count() {
        let ranked: Vec<RankedItem> = (0..10)
            .map(|i| RankedItem {
                content_id: format!("r{}", i),
                rank: i + 1,
                ..Default::default()
            })
            .collect();

        let pool = vec![make_item("e1", 0, 0), make_item("e2", 10, 5)];

        let already: HashSet<String> = ranked.iter().map(|r| r.content_id.clone()).collect();
        let result = inject_exploration_slots(ranked, &pool, 2, &already);

        let explore_count = result.iter().filter(|i| i.exploration_slot).count();
        assert_eq!(explore_count, 2);
    }

    #[test]
    fn new_creator_gets_higher_priority_than_established() {
        let new_creator = make_item("new", 0, 0);
        let established_creator = make_item("est", 1_000_000, 500_000);
        assert!(exploration_priority(&new_creator) > exploration_priority(&established_creator));
    }
}
