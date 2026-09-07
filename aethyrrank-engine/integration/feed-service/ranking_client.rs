// feed-service/src/ranking_client.rs
//
// Drop this file into F33D3R's feed-service crate.
// Add to feed-service/Cargo.toml:
//   reqwest = { version = "0.12", features = ["json"] }

use std::time::Duration;
use tracing::{instrument, warn};

/// Full content candidate shape required by AethyrRank v2.
/// Map from your internal content model before calling rank().
#[derive(serde::Serialize, Clone)]
pub struct ContentCandidate {
    pub content_id:              String,
    pub creator_id:              String,
    pub topic_vector:            Vec<f32>,
    pub published_at:            chrono::DateTime<chrono::Utc>,
    pub engagement:              EngagementPayload,
    pub exposure_count:          u64,
    pub creator_exposure:        u64,
    pub tags:                    Vec<String>,
    pub content_type:            String,

    // ── v2 fields ─────────────────────────────────────────────────────────
    // Velocity (computed by content-service, see docs/velocity_compute.md)
    pub velocity_score:          f64,
    pub early_retention:         f64,
    pub completion_rate:         f64,

    // Revenue (from monetisation service)
    pub conversion_probability:  f64,
    pub creator_revenue_rate:    f64,
    pub ltv_estimate:            f64,

    // Safety (from content classifier — MUST be set before calling rank)
    pub adult_probability:       f64,

    // Fallback fields (not forwarded to AethyrRank)
    #[serde(skip)]
    pub engagement_rate:         f64,
    #[serde(skip)]
    pub freshness_score:         f64,
}

#[derive(serde::Serialize, Clone)]
pub struct EngagementPayload {
    pub likes:             f64,
    pub shares:            f64,
    pub comments:          f64,
    pub saves:             f64,
    pub view_time_seconds: f64,
    pub impressions:       f64,
}

/// UserState shape required by AethyrRank v2.
#[derive(serde::Serialize)]
pub struct UserPayload {
    pub user_id:             String,
    pub interest_vector:     Vec<f32>,
    pub interaction_history: Vec<String>,
    pub is_cold_start:       bool,
    pub creator_affinities:  std::collections::HashMap<String, f32>,
    /// Set from account settings:
    ///   safe_mode=true  → 1e-6
    ///   default         → 0.10
    ///   adult_enabled   → 1.0
    pub safety_epsilon:      f64,
    /// "free" | "subscriber" | "creator"
    pub user_tier:           String,
}

pub struct RankingClient {
    http: reqwest::Client,
    base: String,
}

impl RankingClient {
    pub fn new(base_url: impl Into<String>) -> Self {
        let http = reqwest::Client::builder()
            .timeout(Duration::from_millis(200))
            .build()
            .expect("failed to build HTTP client");
        Self { http, base: base_url.into() }
    }

    /// Call POST /rank and return ordered content_ids.
    /// On any error (timeout, 5xx, parse failure) returns Err.
    /// Caller should fall back to fallback_rank() and log.
    #[instrument(skip(self, candidates, user, session_ctx))]
    pub async fn rank(
        &self,
        candidates:  Vec<ContentCandidate>,
        user:        UserPayload,
        session_ctx: serde_json::Value,
    ) -> anyhow::Result<Vec<RankedResult>> {
        let payload = serde_json::json!({
            "user_state":      user,
            "content_pool":    candidates,
            "session_context": session_ctx,
        });

        let resp = self.http
            .post(format!("{}/rank", self.base))
            .json(&payload)
            .send()
            .await?
            .error_for_status()?;

        let body: serde_json::Value = resp.json().await?;

        let results = body["ranked_items"]
            .as_array()
            .ok_or_else(|| anyhow::anyhow!("ranked_items missing"))?
            .iter()
            .filter_map(|item| {
                Some(RankedResult {
                    content_id:       item["content_id"].as_str()?.to_owned(),
                    rank:             item["rank"].as_u64()? as usize,
                    final_score:      item["final_score"].as_f64()?,
                    exploration_slot: item["exploration_slot"].as_bool().unwrap_or(false),
                    explanation:      item["explanation"].as_str()
                                        .unwrap_or("").to_owned(),
                })
            })
            .collect();

        Ok(results)
    }

    /// POST /feedback — fire-and-forget, errors are logged not surfaced.
    pub async fn send_feedback(&self, payload: serde_json::Value) {
        if let Err(e) = self.http
            .post(format!("{}/feedback", self.base))
            .json(&payload)
            .send()
            .await
        {
            warn!(error = %e, "AethyrRank feedback delivery failed");
        }
    }

    /// Fallback ranker — used when AethyrRank is unavailable.
    /// Simple heuristic: engagement_rate × freshness_score.
    pub fn fallback_rank(mut candidates: Vec<ContentCandidate>) -> Vec<RankedResult> {
        candidates.sort_by(|a, b| {
            let sa = a.engagement_rate * a.freshness_score;
            let sb = b.engagement_rate * b.freshness_score;
            sb.partial_cmp(&sa).unwrap_or(std::cmp::Ordering::Equal)
        });
        candidates.into_iter().enumerate().map(|(i, c)| RankedResult {
            content_id:       c.content_id,
            rank:             i + 1,
            final_score:      0.0,
            exploration_slot: false,
            explanation:      "fallback: engagement×freshness".to_string(),
        }).collect()
    }
}

/// Slim ranked result returned to the feed handler.
#[derive(Debug, Clone)]
pub struct RankedResult {
    pub content_id:       String,
    pub rank:             usize,
    pub final_score:      f64,
    pub exploration_slot: bool,
    pub explanation:      String,
}
