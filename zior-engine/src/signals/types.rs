//! Signal types — the structured output Zior emits to AethyrRank.
//!
//! Every field in `AethyrSignal` maps directly to a field that AethyrRank's
//! `/rank` endpoint reads from `ContentItem`.
//!
//! ## Field mapping
//!
//! | AethyrSignal field       | AethyrRank ContentItem field    |
//! |--------------------------|--------------------------------|
//! | topic_vector             | topic_vector                    |
//! | velocity_score           | velocity_score                  |
//! | early_retention          | early_retention                 |
//! | completion_rate          | completion_rate                 |
//! | conversion_probability   | conversion_probability          |
//! | creator_revenue_rate     | creator_revenue_rate            |
//! | ltv_estimate             | ltv_estimate                    |
//! | adult_probability        | adult_probability               |
//! | exposure_count           | exposure_count                  |

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

/// The complete signal package Zior writes to the content store.
/// The content-service reads this and attaches fields to ContentItem
/// before calling AethyrRank /rank.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AethyrSignal {
    pub track_id: String,
    pub creator_id: String,
    pub computed_at: DateTime<Utc>,

    // ── AethyrRank ContentItem fields ─────────────────────────────────────────
    /// 8D psychological vector — same space as user interest_vector
    pub topic_vector: Vec<f32>,
    /// d(engagement)/dt, normalised [0,1]
    pub velocity_score: f64,
    /// Fraction of listeners past first 30s [0,1]
    pub early_retention: f64,
    /// Fraction of plays that completed [0,1]
    pub completion_rate: f64,
    /// P(paid conversion | engagement) [0,1]
    pub conversion_probability: f64,
    /// Creator's historical revenue rate [0,1]
    pub creator_revenue_rate: f64,
    /// Estimated LTV uplift [0,1]
    pub ltv_estimate: f64,
    /// P(explicit/adult content) [0,1]
    pub adult_probability: f64,
    /// Total play count
    pub exposure_count: u64,
    /// Total creator exposure across all tracks
    pub creator_exposure: u64,

    // ── Zior-only metadata (stored in content DB, not forwarded to /rank) ─────
    pub fingerprint: String,
    pub duration_secs: f64,
    pub bpm: f32,
    pub key: u8,
    pub is_major: bool,
    pub mood: String,
    pub context_tags: Vec<String>,
    pub descriptor: String,
    pub tonal_valence: f32,
    pub rms_energy: f32,
    pub bass_energy: f32,
    pub vocal_probability: f32,
    pub has_vocals: bool,
    pub replay_rate: f64,
    pub share_velocity: f64,
}

/// Lightweight update signal — sent when behavioral stats change
/// without requiring a full re-analysis of the audio.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BehavioralSignalUpdate {
    pub track_id: String,
    pub updated_at: DateTime<Utc>,
    pub velocity_score: f64,
    pub early_retention: f64,
    pub completion_rate: f64,
    pub replay_rate: f64,
    pub share_velocity: f64,
    pub exposure_count: u64,
}
