use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

use crate::behavioral::events::PlayEventType;

// ── Upload response ───────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct UploadResponse {
    /// The id the signal is stored under: the caller's `track_id` when one
    /// was supplied (feed-engine sends its work id or track id), otherwise a
    /// fingerprint-derived id.
    pub track_id: String,
    pub fingerprint: String,
    pub status: String,
    pub processing_ms: u64,
    /// The track's position on the 8 Jung axes, in `AXIS_NAMES` order
    /// (`jung::axes`). Exactly 8 floats by type — the contract with
    /// feed-engine's `jung.Vector`, which rejects any other length.
    pub topic_vector: [f32; 8],
    /// Decoded length of the audio.
    pub duration_secs: f64,
    /// Signal summary — available immediately after processing
    pub signal_preview: SignalPreview,
}

#[derive(Debug, Serialize)]
pub struct SignalPreview {
    pub bpm: f32,
    pub key: String,
    pub mood: String,
    pub descriptor: String,
    pub velocity_score: f64,
    pub context_tags: Vec<String>,
}

// ── Behavioral event ingestion ────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct IngestEventsRequest {
    pub events: Vec<IngestEvent>,
}

#[derive(Debug, Deserialize)]
pub struct IngestEvent {
    pub track_id: String,
    pub user_id: String,
    pub event_type: PlayEventType,
    pub position_secs: f64,
    pub duration_secs: f64,
    pub session_id: String,
    pub timestamp: Option<DateTime<Utc>>,
}

#[derive(Debug, Serialize)]
pub struct IngestResponse {
    pub ingested: usize,
    pub track_ids: Vec<String>,
}

// ── Similarity query ──────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct SimilarTracksResponse {
    pub track_id: String,
    pub similar: Vec<SimilarTrack>,
}

#[derive(Debug, Serialize)]
pub struct SimilarTrack {
    pub track_id: String,
    pub similarity: f32,
    pub mood: String,
    pub descriptor: String,
}

// ── Cluster query ─────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct ClustersResponse {
    pub n_clusters: usize,
    pub clusters: Vec<ClusterSummary>,
}

#[derive(Debug, Serialize)]
pub struct ClusterSummary {
    pub cluster_id: u32,
    pub track_count: usize,
    pub track_ids: Vec<String>,
}
