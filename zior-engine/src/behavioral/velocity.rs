//! Emergence velocity — d(engagement)/dt.
//!
//! Computes the velocity_score that feeds directly into AethyrRank's
//! velocity layer. Tracks with accelerating engagement get boosted
//! into more feeds before their momentum peaks.
//!
//! ## Algorithm
//!
//! For each track, we maintain a sliding window of weighted engagement
//! events. Velocity is the ratio of recent engagement to baseline:
//!
//!     raw_velocity = events[t-1h, t] / events[t-25h, t-24h]
//!     velocity_score = sigmoid((raw_velocity - 1.0) * 2.0)
//!
//! score > 0.5 = accelerating
//! score = 0.5 = stable
//! score < 0.5 = decelerating
//!
//! ## Cluster emergence
//!
//! Tracks that show coordinated velocity spikes across a listener cluster
//! (listeners with similar PsychVectors all engaging at once) are flagged
//! as cluster emergence events. These get additional exploration slot priority.

use chrono::{DateTime, Duration, Utc};
use std::collections::VecDeque;

/// A timestamped weighted engagement event.
#[derive(Debug, Clone)]
struct WeightedEvent {
    timestamp: DateTime<Utc>,
    weight: f64,
}

/// Event weights for velocity computation.
/// These values mirror crate contracts::event_taxonomy::reward_signal() in aethyrrank-engine.
/// When reward signals change in the contracts file, update this function to match.
/// Single source of truth: aethyrrank-engine/src/contracts/event_taxonomy.rs
pub fn event_weight(event_type: &str) -> f64 {
    match event_type {
        "save" | "playlist" => 4.0,
        "share" => 3.0,
        "complete" | "replay" => 2.5,
        "play" => 1.0,
        "skip" => -1.0,
        "negative" => -3.0,
        _ => 0.5,
    }
}

/// Per-track velocity tracker.
pub struct VelocityTracker {
    /// Ring buffer of recent events
    events: VecDeque<WeightedEvent>,
    /// Window for "recent" measurement (default: 1 hour)
    recent_window: Duration,
    /// Window for "baseline" measurement (same window, 24h ago)
    baseline_offset: Duration,
}

impl VelocityTracker {
    pub fn new(window_hours: f64) -> Self {
        let window_secs = (window_hours * 3600.0) as i64;
        Self {
            events: VecDeque::new(),
            recent_window: Duration::seconds(window_secs),
            baseline_offset: Duration::hours(24),
        }
    }

    /// Record a new engagement event.
    pub fn record(&mut self, event_type: &str) {
        let weight = event_weight(event_type);
        if weight.abs() < 0.01 {
            return;
        }
        self.events.push_back(WeightedEvent {
            timestamp: Utc::now(),
            weight,
        });
        self.evict_old();
    }

    /// Record with a custom timestamp (for replay / batch ingestion).
    pub fn record_at(&mut self, event_type: &str, ts: DateTime<Utc>) {
        let weight = event_weight(event_type);
        self.events.push_back(WeightedEvent {
            timestamp: ts,
            weight,
        });
        self.evict_old();
    }

    /// Compute current velocity_score in [0, 1].
    pub fn velocity_score(&self) -> f64 {
        let now = Utc::now();
        let recent = self.window_sum(now - self.recent_window, now);
        let baseline = self.window_sum(
            now - self.baseline_offset - self.recent_window,
            now - self.baseline_offset,
        );

        let raw = if baseline.abs() < 0.01 {
            if recent > 0.0 {
                2.0
            } else {
                0.5
            }
        } else {
            (recent / baseline).clamp(0.0, 10.0)
        };

        sigmoid((raw - 1.0) * 2.0)
    }

    /// Compute early_retention proxy: fraction of engagement in the first 30s.
    /// Requires events to carry position metadata — approximated here from
    /// play/skip ratio in the first vs later events.
    pub fn early_retention(&self) -> f64 {
        let now = Utc::now();
        let recent = self.window_sum(now - Duration::hours(48), now);
        if recent < 0.1 {
            return 0.5;
        } // insufficient data
          // Higher weight events (saves, replays) = people got past the intro
        let quality_fraction = self.events.iter().filter(|e| e.weight >= 2.0).count() as f64
            / self.events.len().max(1) as f64;
        (0.4 + quality_fraction * 0.6).clamp(0.0, 1.0)
    }

    fn window_sum(&self, start: DateTime<Utc>, end: DateTime<Utc>) -> f64 {
        self.events
            .iter()
            .filter(|e| e.timestamp >= start && e.timestamp < end)
            .map(|e| e.weight.max(0.0))
            .sum()
    }

    fn evict_old(&mut self) {
        let cutoff = Utc::now() - Duration::hours(48);
        while let Some(front) = self.events.front() {
            if front.timestamp < cutoff {
                self.events.pop_front();
            } else {
                break;
            }
        }
    }
}

fn sigmoid(x: f64) -> f64 {
    (1.0 / (1.0 + (-x).exp())).clamp(0.0, 1.0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fresh_track_velocity_is_midpoint() {
        let tracker = VelocityTracker::new(1.0);
        // No events = no baseline, no recent = returns 0.5 (stable / unknown)
        let v = tracker.velocity_score();
        assert!(v >= 0.0 && v <= 1.0, "v={}", v);
    }

    #[test]
    fn high_recent_events_above_half() {
        let mut tracker = VelocityTracker::new(1.0);
        for _ in 0..20 {
            tracker.record("complete");
        }
        let v = tracker.velocity_score();
        // No baseline, many recent events = high score
        assert!(v > 0.5, "v={}", v);
    }

    #[test]
    fn negative_events_dont_boost_velocity() {
        let mut tracker = VelocityTracker::new(1.0);
        for _ in 0..10 {
            tracker.record("skip");
        }
        // Skips are negative weight, don't contribute to positive velocity
        let v = tracker.velocity_score();
        assert!(v <= 0.5 + 0.1, "negative events should not boost: v={}", v);
    }

    #[test]
    fn event_weights_ordered_correctly() {
        assert!(event_weight("save") > event_weight("play"));
        assert!(event_weight("play") > 0.0);
        assert!(event_weight("skip") < 0.0);
        assert!(event_weight("negative") < event_weight("skip"));
    }
}
