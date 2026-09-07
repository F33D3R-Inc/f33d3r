//! Stage 6 — Re-ranker
//!
//! Final pass after all scoring layers. Responsibilities:
//!
//! 1. Apply diversity spacing (no two consecutive items from same creator)
//! 2. Re-index ranks
//! 3. Trim to max_items
//!
//! Exploration injection (from `bandit::explorer`) happens here, after
//! diversity spacing, so exploration slots are distributed naturally.

use crate::api::types::RankedItem;

/// Apply creator-diversity spacing: no two consecutive items from same creator.
/// Uses a greedy insertion approach — maintains original rank ordering as much
/// as possible, only displacing items to avoid runs.
pub fn apply_diversity(
    items: Vec<RankedItem>,
    content_creators: &[(String, String)],
) -> Vec<RankedItem> {
    if items.len() < 2 {
        return items;
    }

    // Build a content_id → creator_id lookup from the provided pairs
    let creator_map: std::collections::HashMap<&str, &str> = content_creators
        .iter()
        .map(|(cid, cr)| (cid.as_str(), cr.as_str()))
        .collect();

    let mut result: Vec<RankedItem> = Vec::with_capacity(items.len());
    let mut remaining: Vec<RankedItem> = items;

    while !remaining.is_empty() {
        let last_creator = result
            .last()
            .and_then(|i| creator_map.get(i.content_id.as_str()).copied());

        // Find the first item in `remaining` that differs from last_creator
        let pos = remaining
            .iter()
            .position(|item| {
                let this_creator = creator_map.get(item.content_id.as_str()).copied();
                last_creator.is_none() || this_creator != last_creator
            })
            .unwrap_or(0); // fallback: if all same creator, just take the head

        result.push(remaining.remove(pos));
    }

    // Re-index ranks
    for (i, item) in result.iter_mut().enumerate() {
        item.rank = i + 1;
    }

    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::types::ScoreBreakdown;

    fn make_ranked(id: &str, score: f64) -> RankedItem {
        RankedItem {
            content_id: id.to_string(),
            rank: 0,
            final_score: score,
            score_breakdown: ScoreBreakdown::default(),
            explanation: String::new(),
            exploration_slot: false,
            safety_blocked: false,
        }
    }

    #[test]
    fn no_consecutive_same_creator() {
        let items = vec![
            make_ranked("a1", 0.9),
            make_ranked("a2", 0.85),
            make_ranked("b1", 0.8),
        ];
        let pairs = vec![
            ("a1".to_string(), "creator_a".to_string()),
            ("a2".to_string(), "creator_a".to_string()),
            ("b1".to_string(), "creator_b".to_string()),
        ];
        let result = apply_diversity(items, &pairs);
        // a1 and a2 should not be adjacent
        let positions: Vec<usize> = result
            .iter()
            .enumerate()
            .filter(|(_, i)| i.content_id.starts_with('a'))
            .map(|(idx, _)| idx)
            .collect();
        if positions.len() == 2 {
            assert!(
                positions[1] - positions[0] > 1,
                "same-creator items should not be adjacent"
            );
        }
    }
}
