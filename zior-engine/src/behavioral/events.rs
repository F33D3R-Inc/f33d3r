//! Behavioral event ingestion.
//!
//! Consumes real-time user interaction events for a track and derives:
//!   - A behavioral PsychVector (how listeners actually engage with this track)
//!   - Retention score (fraction completing the track)
//!   - Replay rate (loops per unique listener)
//!   - Skip timing (where listeners drop off)
//!
//! The behavioral vector is blended with the audio vector in the Jung mapper
//! to produce the final topic_vector that AethyrRank uses.
//!
//! ## Why behavioral signals change the psychological vector
//!
//! Audio features capture what the track *is*.
//! Behavioral signals capture what the track *does to people*.
//! A track that sounds peaceful but triggers rage-skips at 0:45 has
//! a different effective Shadow score than its audio alone suggests.
//! Behavior is ground truth.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

use crate::jung::axes::PsychVector;

// ── Event types ───────────────────────────────────────────────────────────────

/// Play event types for Zior's behavioral tracking.
/// These mirror EventType variants from aethyrrank-engine/src/contracts/event_taxonomy.rs.
/// When adding new event types, add them to the contracts file first,
/// then mirror the relevant variants here.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PlayEventType {
    /// Track started playing
    Play,
    /// User skipped (timestamp = position when skipped)
    Skip,
    /// Track completed
    Complete,
    /// User manually replayed (looped or restarted)
    Replay,
    /// User shared the track
    Share,
    /// User saved / bookmarked
    Save,
    /// Negative action (hide, dislike)
    Negative,
    /// User added to playlist
    Playlist,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PlayEvent {
    pub track_id: String,
    pub user_id: String,
    pub event_type: PlayEventType,
    /// Position in the track when the event occurred (seconds)
    pub position_secs: f64,
    /// Track duration in seconds (for normalisation)
    pub duration_secs: f64,
    pub timestamp: DateTime<Utc>,
    pub session_id: String,
}

// ── Aggregated behavioral stats ───────────────────────────────────────────────

#[derive(Debug, Clone, Default)]
pub struct TrackBehaviorStats {
    pub total_plays: u64,
    pub total_completes: u64,
    pub total_skips: u64,
    pub total_replays: u64,
    pub total_shares: u64,
    pub total_saves: u64,
    pub total_negatives: u64,
    /// Distribution of skip positions, in 10% buckets [0..=9]
    pub skip_histogram: [u32; 10],
    /// Mean position when skipped (normalised 0–1)
    pub mean_skip_position: f64,
    /// Fraction of plays that completed
    pub retention_rate: f64,
    /// Replays per unique listener
    pub replay_rate: f64,
    /// Share velocity (shares per play)
    pub share_velocity: f64,
}

impl TrackBehaviorStats {
    /// Ingest a single event and update stats in place.
    pub fn ingest(&mut self, event: &PlayEvent) {
        match event.event_type {
            PlayEventType::Play => self.total_plays += 1,
            PlayEventType::Complete => self.total_completes += 1,
            PlayEventType::Replay => self.total_replays += 1,
            PlayEventType::Share => self.total_shares += 1,
            PlayEventType::Save => self.total_saves += 1,
            PlayEventType::Negative => self.total_negatives += 1,
            PlayEventType::Playlist => self.total_saves += 1,
            PlayEventType::Skip => {
                self.total_skips += 1;
                if event.duration_secs > 0.0 {
                    let frac = (event.position_secs / event.duration_secs).clamp(0.0, 1.0);
                    let bucket = (frac * 10.0) as usize;
                    self.skip_histogram[bucket.min(9)] += 1;

                    // Online mean update
                    let n = self.total_skips as f64;
                    self.mean_skip_position += (frac - self.mean_skip_position) / n;
                }
            }
        }

        // Recompute derived rates
        if self.total_plays > 0 {
            self.retention_rate = self.total_completes as f64 / self.total_plays as f64;
            self.replay_rate = self.total_replays as f64 / self.total_plays as f64;
            self.share_velocity = self.total_shares as f64 / self.total_plays as f64;
        }
    }

    /// Derive a behavioral PsychVector from aggregate stats.
    ///
    /// The axes reflect the *listener response* to the track:
    ///
    ///   Shadow:      high skip rate → content not matching persona expectations
    ///   Agency:      high replay + complete → drives re-engagement
    ///   Integration: high completion → structurally satisfying
    ///   Attachment:  high save + playlist → personal connection
    ///   Disruption:  bimodal skips (some skip early, some replay) → divisive
    ///   Release:     high share → emotional release drives social behaviour
    pub fn to_psych_vector(&self) -> PsychVector {
        if self.total_plays == 0 {
            return PsychVector::zero();
        }

        // Skip bimodality: high variance in skip positions = divisive = Disruption
        let skip_variance = histogram_variance(&self.skip_histogram);

        // Early drop: fraction skipping in first 20% of track
        let early_drop = if self.total_skips > 0 {
            (self.skip_histogram[0] + self.skip_histogram[1]) as f64 / self.total_skips as f64
        } else {
            0.0
        };

        let persona = (self.share_velocity * 2.0
            + self.total_saves as f64 / self.total_plays as f64)
            .clamp(0.0, 1.0) as f32;
        let shadow = (1.0 - self.retention_rate + early_drop * 0.5).clamp(0.0, 1.0) as f32;
        let agency = (self.replay_rate * 2.0 + self.retention_rate * 0.5).clamp(0.0, 1.0) as f32;
        let integration = self.retention_rate as f32;
        let attachment =
            ((self.total_saves as f64 + self.total_replays as f64) / self.total_plays as f64 * 1.5)
                .clamp(0.0, 1.0) as f32;
        let disruption = skip_variance as f32;
        let tension = early_drop as f32;
        let release = (self.share_velocity * 3.0).clamp(0.0, 1.0) as f32;

        PsychVector([
            persona,
            shadow,
            agency,
            integration,
            attachment,
            disruption,
            tension,
            release,
        ])
    }
}

fn histogram_variance(hist: &[u32; 10]) -> f64 {
    let total: u32 = hist.iter().sum();
    if total == 0 {
        return 0.0;
    }
    let mean = hist
        .iter()
        .enumerate()
        .map(|(i, &c)| i as f64 * c as f64)
        .sum::<f64>()
        / total as f64;
    let var = hist
        .iter()
        .enumerate()
        .map(|(i, &c)| (i as f64 - mean).powi(2) * c as f64)
        .sum::<f64>()
        / total as f64;
    (var / 81.0).clamp(0.0, 1.0) // normalise: max variance with 10 buckets = 81/4
}

// ── In-memory behavioral store ────────────────────────────────────────────────

/// Thread-safe in-memory map of track_id → stats.
/// In production: replace with a time-series event store (ClickHouse, etc.).
pub struct BehavioralStore {
    data: dashmap::DashMap<String, TrackBehaviorStats>,
}

impl BehavioralStore {
    pub fn new() -> Self {
        Self {
            data: dashmap::DashMap::new(),
        }
    }

    pub fn ingest(&self, event: PlayEvent) {
        self.data
            .entry(event.track_id.clone())
            .or_default()
            .ingest(&event);
    }

    pub fn stats(&self, track_id: &str) -> Option<TrackBehaviorStats> {
        self.data.get(track_id).map(|s| s.clone())
    }

    pub fn psych_vector(&self, track_id: &str) -> Option<PsychVector> {
        self.stats(track_id).map(|s| s.to_psych_vector())
    }

    pub fn retention_rate(&self, track_id: &str) -> f64 {
        self.stats(track_id)
            .map(|s| s.retention_rate)
            .unwrap_or(0.5)
    }

    pub fn replay_rate(&self, track_id: &str) -> f64 {
        self.stats(track_id).map(|s| s.replay_rate).unwrap_or(0.0)
    }

    pub fn share_velocity(&self, track_id: &str) -> f64 {
        self.stats(track_id)
            .map(|s| s.share_velocity)
            .unwrap_or(0.0)
    }
}

impl Default for BehavioralStore {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_event(track_id: &str, event_type: PlayEventType, pos: f64, dur: f64) -> PlayEvent {
        PlayEvent {
            track_id: track_id.to_string(),
            user_id: "u1".to_string(),
            event_type,
            position_secs: pos,
            duration_secs: dur,
            timestamp: Utc::now(),
            session_id: "s1".to_string(),
        }
    }

    #[test]
    fn retention_rate_correct() {
        let mut stats = TrackBehaviorStats::default();
        for _ in 0..8 {
            stats.ingest(&make_event("t1", PlayEventType::Play, 0.0, 180.0));
        }
        for _ in 0..5 {
            stats.ingest(&make_event("t1", PlayEventType::Complete, 180.0, 180.0));
        }
        assert!(
            (stats.retention_rate - 0.625).abs() < 0.01,
            "retention={}",
            stats.retention_rate
        );
    }

    #[test]
    fn skip_histogram_populated() {
        let mut stats = TrackBehaviorStats::default();
        stats.ingest(&make_event("t1", PlayEventType::Play, 0.0, 200.0));
        stats.ingest(&make_event("t1", PlayEventType::Skip, 20.0, 200.0)); // 10% mark
        assert!(stats.skip_histogram[1] > 0);
    }

    #[test]
    fn psych_vector_all_in_unit_interval() {
        let store = BehavioralStore::new();
        for _ in 0..10 {
            store.ingest(make_event("t1", PlayEventType::Play, 0.0, 120.0));
        }
        for _ in 0..7 {
            store.ingest(make_event("t1", PlayEventType::Complete, 120.0, 120.0));
        }
        for _ in 0..3 {
            store.ingest(make_event("t1", PlayEventType::Share, 60.0, 120.0));
        }
        let vec = store.psych_vector("t1").unwrap();
        for (i, &x) in vec.0.iter().enumerate() {
            assert!(x >= 0.0 && x <= 1.0, "axis[{}] = {}", i, x);
        }
    }

    #[test]
    fn high_completion_high_integration() {
        let mut stats = TrackBehaviorStats::default();
        for _ in 0..10 {
            stats.ingest(&make_event("t1", PlayEventType::Play, 0.0, 200.0));
        }
        for _ in 0..9 {
            stats.ingest(&make_event("t1", PlayEventType::Complete, 200.0, 200.0));
        }
        let vec = stats.to_psych_vector();
        assert!(vec.0[3] > 0.7, "integration={}", vec.0[3]);
    }
}
