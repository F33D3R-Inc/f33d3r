//! BrainSignal — the universal envelope for all inter-brain communication.
//!
//! Every brain that emits data to AethyrRank wraps it in a BrainSignal.
//! AethyrRank's /brain/signal endpoint accepts any BrainSignal.
//! The schema_version field enables backward-compatible evolution.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::contracts::brain_id::BrainId;

/// The universal signal envelope.
/// Every brain-to-brain message is a BrainSignal.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrainSignal {
    /// Unique ID for this signal — used for deduplication and audit trails
    pub signal_id: Uuid,
    /// Which brain emitted this signal
    pub source_brain: BrainId,
    /// What type of signal this is
    pub signal_type: SignalType,
    /// The signal payload — schema defined by source_brain + signal_type + schema_version
    pub payload: serde_json::Value,
    /// Schema version — consumers check this for compatibility
    pub schema_version: u32,
    pub emitted_at: DateTime<Utc>,
    /// Trace ID for distributed tracing (Elohim Veni audit trail)
    pub trace_id: Uuid,
}

impl BrainSignal {
    pub fn new(
        source: BrainId,
        sig_type: SignalType,
        payload: serde_json::Value,
        version: u32,
    ) -> Self {
        Self {
            signal_id: Uuid::new_v4(),
            source_brain: source,
            signal_type: sig_type,
            payload,
            schema_version: version,
            emitted_at: Utc::now(),
            trace_id: Uuid::new_v4(),
        }
    }
}

/// Signal types — what kind of data is in the payload.
/// Adding a new signal type requires adding a schema to the Schema Registry.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SignalType {
    // ── Music / Zior ──────────────────────────────────────────────────────
    /// Full audio analysis result — topic_vector + all AethyrRank fields
    AudioAnalysis,
    /// Behavioral update — velocity, completion, replay updated
    VelocityUpdate,
    /// Cluster emergence — micro-community forming around a signature
    ClusterEmergence,

    // ── Feed / Nantar (future) ────────────────────────────────────────────
    /// Post created and indexed
    PostCreated,
    /// Engagement update for a post
    PostEngagement,
    /// Trending topic signal
    TrendingTopic,

    // ── Commerce / Thessalon (future) ─────────────────────────────────────
    /// Product conversion signal
    Conversion,
    /// Creator revenue update
    RevenueUpdate,

    // ── Streaming / Caeor (future) ────────────────────────────────────────
    /// Live stream started
    StreamStarted,
    /// Stream engagement burst
    StreamEngagement,
    /// Stream ended with metrics
    StreamEnded,

    // ── Video / Astraon (future) ──────────────────────────────────────────
    /// Video indexed and ready for ranking
    VideoIndexed,
    /// Video engagement update
    VideoEngagement,

    // ── Wallet / Ain Soph (future) ────────────────────────────────────────
    /// Creator payout processed
    PayoutProcessed,
    /// Tip received — strong positive signal for creator ranking
    TipReceived,

    // ── Security / Elohim Veni (future) ───────────────────────────────────
    /// Content flagged — may affect ranking
    ContentFlagged,
    /// Account risk update
    AccountRiskUpdate,

    // ── System ────────────────────────────────────────────────────────────
    /// Brain registration heartbeat
    Heartbeat,
    /// Brain is shutting down gracefully
    GracefulShutdown,
}

/// Registration payload sent by each brain on startup.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrainRegistration {
    pub brain_id: BrainId,
    pub schema_version: u32,
    pub port: u16,
    pub host: String,
    pub signal_types: Vec<SignalType>,
    pub description: String,
    pub registered_at: DateTime<Utc>,
}

/// Status of a registered brain in the registry.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BrainStatus {
    /// Brain is online and sending heartbeats
    Online,
    /// Brain missed last heartbeat — may be temporarily down
    Degraded,
    /// Brain has not sent a heartbeat in >60s
    Offline,
    /// Brain is known but not yet implemented (stub)
    Planned,
}
