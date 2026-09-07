//! Signal emitter — pushes AethyrSignals to the content store.
//!
//! The content-service reads from the store when building /rank payloads.
//! In production, this would write to your content database (Postgres, etc.).
//! For now it uses the in-memory store and optionally POSTs to a webhook.

use anyhow::Result;
use std::sync::Arc;
use std::time::Duration;
use tracing::{info, warn};

use crate::config::AethyrConfig;
use crate::signals::types::{AethyrSignal, BehavioralSignalUpdate};
use crate::store::TrackStore;

pub struct SignalEmitter {
    store: Arc<TrackStore>,
    http: reqwest::Client,
    config: AethyrConfig,
}

impl SignalEmitter {
    pub fn new(store: Arc<TrackStore>, config: AethyrConfig) -> Self {
        let http = reqwest::Client::builder()
            .timeout(Duration::from_millis(config.signal_push_timeout_ms))
            .build()
            .expect("http client init");
        Self {
            store,
            http,
            config,
        }
    }

    /// Emit a full signal after audio analysis completes.
    pub async fn emit(&self, signal: AethyrSignal) -> Result<()> {
        info!(
            track_id = %signal.track_id,
            velocity = signal.velocity_score,
            mood = %signal.mood,
            bpm  = signal.bpm,
            "emitting signal"
        );

        // 1. Write to local store
        self.store.upsert_signal(signal.clone());

        // 2. Push to content-store webhook (fire-and-forget)
        self.push_async(signal);

        Ok(())
    }

    /// Emit a lightweight behavioral update (no audio re-analysis).
    pub async fn emit_behavioral(&self, update: BehavioralSignalUpdate) -> Result<()> {
        info!(
            track_id   = %update.track_id,
            velocity   = update.velocity_score,
            completion = update.completion_rate,
            "behavioral signal update"
        );

        self.store.update_behavioral(&update.track_id, &update);
        self.push_behavioral_async(update);

        Ok(())
    }

    fn push_async(&self, signal: AethyrSignal) {
        let http = self.http.clone();
        let url = format!("{}/internal/signals", self.config.content_store_url);
        tokio::spawn(async move {
            match http.post(&url).json(&signal).send().await {
                Ok(resp) if resp.status().is_success() => {
                    info!(track_id = %signal.track_id, "signal pushed to content store");
                }
                Ok(resp) => {
                    warn!(track_id = %signal.track_id, status = %resp.status(), "content store rejected signal");
                }
                Err(e) => {
                    warn!(track_id = %signal.track_id, error = %e, "content store push failed");
                }
            }
        });
    }

    fn push_behavioral_async(&self, update: BehavioralSignalUpdate) {
        let http = self.http.clone();
        let url = format!(
            "{}/internal/signals/behavioral",
            self.config.content_store_url
        );
        tokio::spawn(async move {
            if let Err(e) = http.post(&url).json(&update).send().await {
                warn!(track_id = %update.track_id, error = %e, "behavioral push failed");
            }
        });
    }
}
