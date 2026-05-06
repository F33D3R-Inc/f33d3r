//! Stage 2 — Neural Ranking Model
//!
//! This module is the **architectural slot** for the learned ranking model.
//!
//! ## Production target
//!
//! The neural model predicts:
//!
//!     P(meaningful_engagement | user, content, context)
//!
//! Architecture (Wide & Deep, session-aware):
//!
//!   Embedding layer (user + content, dim 128–256)
//!       ↓
//!   Feature cross (wide component — memorisation)
//!       ↓
//!   Deep MLP (3–4 layers, ReLU, dropout 0.3)
//!       ↓
//!   Optional Transformer (last-20 session items, causal attention)
//!       ↓
//!   Output: sigmoid → score ∈ [0, 1]
//!
//! ## Current implementation
//!
//! The neural model is **stubbed** with a high-quality heuristic that uses
//! the same signals the real model would consume as features. This stub is
//! production-safe — it degrades gracefully and provides a realistic score
//! distribution until the trained model is wired in.
//!
//! To replace: swap `score_item` to call a TorchScript / ONNX inference
//! endpoint (e.g. Triton, BentoML, or a local ONNX runtime binding).
//!
//! ## Latency contract
//!
//! This stage must complete in < 15ms for a batch of 100 items.
//! The stub easily meets this. The trained model should be quantised
//! (int8 or fp16) to meet this budget at the target device.

use crate::api::types::{ContentItem, UserState};

/// Output of the neural stage for a single item.
#[derive(Debug, Clone)]
pub struct NeuralScore {
    pub content_id: String,
    /// P(engagement | u, i, ctx) — primary neural estimate ∈ [0,1]
    pub score: f64,
    /// Model confidence / uncertainty estimate ∈ [0,1]
    pub confidence: f64,
}

/// Score a batch of items. Returns one `NeuralScore` per item, in input order.
///
/// Replace the body of this function with your ONNX / Triton call when ready.
pub fn score_batch(user: &UserState, items: &[&ContentItem]) -> Vec<NeuralScore> {
    items.iter().map(|item| score_item(user, item)).collect()
}

fn score_item(user: &UserState, item: &ContentItem) -> NeuralScore {
    // ── Stub heuristic (replaces trained model) ───────────────────────────
    //
    // Combines signals the real model would learn from:
    //   - interest alignment (cosine-like proxy)
    //   - engagement quality
    //   - freshness
    //   - creator affinity
    //   - completion + early retention (strong watch signals)
    //
    // This is NOT the same as AESQ scoring — it mimics a learned combination
    // of user-item features without the constraint structure.

    let alignment = vector_dot_norm(&user.interest_vector, &item.topic_vector);

    let eng = &item.engagement;
    let quality = if eng.impressions > 0.0 {
        let raw = eng.likes + 3.0 * eng.shares + 2.0 * eng.comments + 4.0 * eng.saves;
        sigmoid(raw / eng.impressions * 20.0)
    } else {
        0.0
    };

    let creator_boost = user
        .creator_affinities
        .get(&item.creator_id)
        .copied()
        .unwrap_or(0.0) as f64;

    // Session signal: penalise repeated exposure (fatigue)
    let fatigue = {
        let seen = user
            .interaction_history
            .iter()
            .filter(|id| *id == &item.content_id)
            .count() as f64;
        let window = user.interaction_history.len().max(1) as f64;
        (seen / window).clamp(0.0, 1.0)
    };

    // Weighted combination (mimics a learned Wide & Deep output)
    let raw = 0.35 * alignment
        + 0.25 * quality
        + 0.15 * item.completion_rate
        + 0.10 * item.early_retention
        + 0.10 * creator_boost
        - 0.15 * fatigue;

    let score = raw.clamp(0.0, 1.0);
    // Confidence: higher for items with more engagement data
    let confidence = sigmoid((eng.impressions / 1000.0).ln().max(0.0));

    NeuralScore {
        content_id: item.content_id.clone(),
        score,
        confidence,
    }
}

// ── Helpers ───────────────────────────────────────────────────────────────────

fn vector_dot_norm(u: &[f32], c: &[f32]) -> f64 {
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

fn sigmoid(x: f64) -> f64 {
    1.0 / (1.0 + (-x).exp())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::types::EngagementSignals;
    use chrono::Utc;
    use std::collections::HashMap;

    fn make_user(v: Vec<f32>) -> UserState {
        UserState {
            user_id: "u1".to_string(),
            interest_vector: v,
            interaction_history: vec![],
            is_cold_start: false,
            creator_affinities: HashMap::new(),
            safety_epsilon: 0.1,
            user_tier: "free".to_string(),
        }
    }

    fn make_item(v: Vec<f32>, impressions: f64) -> ContentItem {
        ContentItem {
            content_id: "c1".to_string(),
            creator_id: "cr1".to_string(),
            topic_vector: v,
            published_at: Utc::now(),
            engagement: EngagementSignals {
                likes: 100.0,
                shares: 20.0,
                comments: 30.0,
                saves: 15.0,
                view_time_seconds: 300.0,
                impressions,
            },
            exposure_count: 500,
            creator_exposure: 10000,
            tags: vec![],
            content_type: "video".to_string(),
            velocity_score: 0.6,
            early_retention: 0.7,
            completion_rate: 0.65,
            conversion_probability: 0.05,
            creator_revenue_rate: 0.3,
            ltv_estimate: 0.2,
            adult_probability: 0.0,
        }
    }

    #[test]
    fn score_in_unit_interval() {
        let user = make_user(vec![0.5; 4]);
        let item = make_item(vec![0.5; 4], 1000.0);
        let score = score_item(&user, &item);
        assert!(score.score >= 0.0 && score.score <= 1.0);
    }

    #[test]
    fn aligned_user_scores_higher() {
        let user_aligned = make_user(vec![1.0, 0.0, 0.0]);
        let user_opposite = make_user(vec![-1.0, 0.0, 0.0]);
        let item = make_item(vec![1.0, 0.0, 0.0], 500.0);
        let s_aligned = score_item(&user_aligned, &item).score;
        let s_opposite = score_item(&user_opposite, &item).score;
        assert!(s_aligned > s_opposite);
    }

    #[test]
    fn batch_returns_one_score_per_item() {
        let user = make_user(vec![0.5; 4]);
        let item1 = make_item(vec![0.5; 4], 100.0);
        let item2 = make_item(vec![0.2; 4], 200.0);
        let batch = score_batch(&user, &[&item1, &item2]);
        assert_eq!(batch.len(), 2);
    }
}
