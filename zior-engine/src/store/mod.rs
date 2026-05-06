//! In-memory track signal store.
//!
//! Stores the latest computed signal for each track. In production:
//! replace with Postgres + Redis cache.

use chrono::Utc;
use dashmap::DashMap;

use crate::signals::types::{AethyrSignal, BehavioralSignalUpdate};

pub struct TrackStore {
    signals: DashMap<String, AethyrSignal>,
}

impl TrackStore {
    pub fn new() -> Self {
        Self {
            signals: DashMap::new(),
        }
    }

    pub fn upsert_signal(&self, signal: AethyrSignal) {
        self.signals.insert(signal.track_id.clone(), signal);
    }

    pub fn get_signal(&self, track_id: &str) -> Option<AethyrSignal> {
        self.signals.get(track_id).map(|s| s.clone())
    }

    /// Update only the behavioral fields without overwriting audio analysis.
    pub fn update_behavioral(&self, track_id: &str, update: &BehavioralSignalUpdate) {
        if let Some(mut signal) = self.signals.get_mut(track_id) {
            signal.velocity_score = update.velocity_score;
            signal.early_retention = update.early_retention;
            signal.completion_rate = update.completion_rate;
            signal.replay_rate = update.replay_rate;
            signal.share_velocity = update.share_velocity;
            signal.exposure_count = update.exposure_count;
        }
    }

    pub fn all_signals(&self) -> Vec<AethyrSignal> {
        self.signals.iter().map(|e| e.value().clone()).collect()
    }

    pub fn count(&self) -> usize {
        self.signals.len()
    }
}

impl Default for TrackStore {
    fn default() -> Self {
        Self::new()
    }
}
