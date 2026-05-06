use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

// ── /rank request ─────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct RankRequest {
    pub user_state: UserState,
    pub content_pool: Vec<ContentItem>,
    pub session_context: SessionContext,
}

#[derive(Debug, Deserialize, Clone)]
pub struct UserState {
    pub user_id: String,
    pub interest_vector: Vec<f32>,
    pub interaction_history: Vec<String>,
    pub is_cold_start: bool,
    pub creator_affinities: HashMap<String, f32>,
    /// Hard safety bound for this user. sfw_epsilon (~1e-6) for safe-mode users.
    /// Callers should set this based on user account settings.
    pub safety_epsilon: f64,
    /// "free" | "subscriber" | "creator" — shapes revenue layer weights
    pub user_tier: String,
    /// Realm progression level 1–5. Encodes account trust and activity depth.
    /// Used as `creator_boost` dimension in the LinUCB feature vector.
    /// Absent for legacy clients → defaults to Realm 1.
    #[serde(default = "default_realm")]
    pub realm_level: u8,
}

fn default_realm() -> u8 {
    1
}

#[derive(Debug, Deserialize, Clone)]
pub struct ContentItem {
    pub content_id: String,
    pub creator_id: String,
    pub topic_vector: Vec<f32>,
    pub published_at: DateTime<Utc>,
    pub engagement: EngagementSignals,
    pub exposure_count: u64,
    pub creator_exposure: u64,
    pub tags: Vec<String>,
    pub content_type: String,

    // ── Velocity signals ──────────────────────────────────────────────────
    /// Pre-computed d(engagement)/dt from the content-service, normalised [0,1]
    pub velocity_score: f64,
    /// Watch rate in first 3 seconds (0.0–1.0)
    pub early_retention: f64,
    /// Fraction of viewers who completed the content
    pub completion_rate: f64,

    // ── Revenue signals ───────────────────────────────────────────────────
    /// Predicted probability this content drives a paid conversion (0.0–1.0)
    pub conversion_probability: f64,
    /// Creator's historical revenue rate (normalised 0.0–1.0)
    pub creator_revenue_rate: f64,
    /// Estimated lifetime value uplift if user engages (normalised 0.0–1.0)
    pub ltv_estimate: f64,

    // ── Safety classification ─────────────────────────────────────────────
    /// Probability this item belongs to the adult content manifold (0.0–1.0).
    /// Computed by the content-service classifier before submission to AethyrRank.
    pub adult_probability: f64,
}

#[derive(Debug, Deserialize, Clone)]
pub struct EngagementSignals {
    pub likes: f64,
    pub shares: f64,
    pub comments: f64,
    pub saves: f64,
    pub view_time_seconds: f64,
    pub impressions: f64,
}

#[derive(Debug, Deserialize, Clone)]
pub struct SessionContext {
    pub surface: String,
    pub request_id: String,
    pub session_id: String,
    pub max_items: usize,
    pub timestamp: DateTime<Utc>,
}

// ── /rank response ────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct RankResponse {
    pub request_id: String,
    pub ranked_items: Vec<RankedItem>,
    pub confidence: f64,
    pub latency_ms: u64,
}

#[derive(Debug, Serialize, Default, Clone)]
pub struct RankedItem {
    pub content_id: String,
    pub rank: usize,
    pub final_score: f64,
    pub score_breakdown: ScoreBreakdown,
    pub explanation: String,
    pub exploration_slot: bool,
    pub safety_blocked: bool,
}

/// Full score breakdown across all pipeline stages.
#[derive(Debug, Serialize, Default, Clone)]
pub struct ScoreBreakdown {
    // Stage 1: pre-ranker
    pub fast_score: f64,
    // Stage 2: neural model (stub until model is wired)
    pub neural_score: f64,
    // Stage 3: AESQ constraint layer
    pub aesq_alignment: f64,
    pub aesq_expansion: f64,
    pub aesq_shadow: f64,
    pub aesq_quality: f64,
    pub aesq_freshness: f64,
    pub aesq_total: f64,
    pub aesq_multiplier: f64,
    // Stage 4: velocity layer
    pub velocity_boost: f64,
    // Stage 5: revenue layer
    pub revenue_adj: f64,
    // Final
    pub final_score: f64,
}

// ── /feedback request ─────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct FeedbackRequest {
    pub user_id: String,
    pub session_id: String,
    pub surface: String,
    pub events: Vec<FeedbackEvent>,
}

#[derive(Debug, Deserialize)]
pub struct FeedbackEvent {
    pub content_id: String,
    pub event_type: EventType,
    pub position_at_display: usize,
    pub timestamp: DateTime<Utc>,
    pub dwell_ms: Option<u64>,
    pub exploration_slot: bool,
}

// EventType is defined once in contracts::event_taxonomy and re-exported here.
// All brain regions import EventType from this path.
// DO NOT redefine EventType anywhere else — the compiler will catch it.
pub use crate::contracts::event_taxonomy::EventType;
