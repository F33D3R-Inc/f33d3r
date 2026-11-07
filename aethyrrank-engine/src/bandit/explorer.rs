use rand::Rng;
use std::collections::HashSet;

use crate::api::types::{ContentItem, RankedItem, ScoreBreakdown};

/// Place `explore_count` exploration items on the first page, in its lower half, so
/// they are actually seen without leading the feed.
///
/// Candidates are anything in the pool not already on that page and not in `excluded`,
/// which holds whatever the safety barrier refused. New creators and low-exposure
/// content come first. Anything picked moves up rather than appearing twice. Each is
/// flagged with `exploration_slot: true`.
pub fn inject_exploration_slots(
    mut ranked: Vec<RankedItem>,
    content_pool: &[ContentItem],
    explore_count: usize,
    excluded: &HashSet<String>,
    page_size: usize,
) -> Vec<RankedItem> {
    if explore_count == 0 {
        return ranked;
    }

    let mut rng = rand::thread_rng();
    let page = page_size.min(ranked.len());
    let on_page: HashSet<&str> = ranked[..page]
        .iter()
        .map(|i| i.content_id.as_str())
        .collect();

    let mut candidates: Vec<&ContentItem> = content_pool
        .iter()
        .filter(|c| !excluded.contains(&c.content_id) && !on_page.contains(c.content_id.as_str()))
        .collect();
    candidates.sort_by(|a, b| {
        exploration_priority(b)
            .partial_cmp(&exploration_priority(a))
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    let picked: Vec<&ContentItem> = candidates.into_iter().take(explore_count).collect();
    drop(on_page);
    if picked.is_empty() {
        return ranked;
    }

    ranked.retain(|i| !picked.iter().any(|p| p.content_id == i.content_id));

    // Each pick gets its own stretch of the lower half, in ascending order, so a later
    // insert never pushes an earlier one off the page.
    let limit = page_size.min(ranked.len() + picked.len());
    let low = limit / 2;
    let stretch = ((limit - low) / picked.len()).max(1);
    let mut next_free = 0;
    for (n, item) in picked.iter().enumerate() {
        let wanted = low + n * stretch + rng.gen_range(0..stretch);
        let insert_pos = wanted.max(next_free).min(ranked.len());
        next_free = insert_pos + 1;
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
            topic_vector: vec![0.5; 8],
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
            posts_last_24h: 0,
            self_reply_cadence: 0.0,
            char_count: 0,
            is_in_network: false,
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

        let result = inject_exploration_slots(ranked, &pool, 2, &HashSet::new(), 10);

        let explore_count = result.iter().filter(|i| i.exploration_slot).count();
        assert_eq!(explore_count, 2);
        assert!(
            result
                .iter()
                .take(10)
                .filter(|i| i.exploration_slot)
                .count()
                == 2
        );
    }

    #[test]
    fn what_the_barrier_refused_is_never_explored() {
        let ranked: Vec<RankedItem> = vec![];
        let pool = vec![make_item("blocked", 0, 0)];
        let excluded: HashSet<String> = ["blocked".to_string()].into_iter().collect();
        let result = inject_exploration_slots(ranked, &pool, 1, &excluded, 10);
        assert!(result.is_empty());
    }

    #[test]
    fn an_item_moved_up_does_not_appear_twice() {
        let ranked: Vec<RankedItem> = (0..5)
            .map(|i| RankedItem {
                content_id: format!("r{}", i),
                rank: i + 1,
                ..Default::default()
            })
            .collect();
        let pool = vec![make_item("r4", 0, 0)];
        let result = inject_exploration_slots(ranked, &pool, 1, &HashSet::new(), 2);
        assert_eq!(result.iter().filter(|i| i.content_id == "r4").count(), 1);
        assert!(result.iter().take(2).any(|i| i.content_id == "r4"));
    }

    #[test]
    fn new_creator_gets_higher_priority_than_established() {
        let new_creator = make_item("new", 0, 0);
        let established_creator = make_item("est", 1_000_000, 500_000);
        assert!(exploration_priority(&new_creator) > exploration_priority(&established_creator));
    }
}
