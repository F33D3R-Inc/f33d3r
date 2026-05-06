//! Zior BrainPort client — registers with AethyrRank on startup
//! and sends behavioral signals as they arrive.
//!
//! ## Registration sequence
//! 1. Zior starts on port 8082
//! 2. After 2s delay, attempts POST /brain/register to AethyrRank
//! 3. If AethyrRank unreachable → retry every 30s (never crash)
//! 4. Once registered → heartbeat every 30s
//! 5. On heartbeat failure → re-register on next tick
//! 6. Signal buffer: if AethyrRank is down, queue up to 10k signals in memory
//!    and replay when connection is restored

use anyhow::Result;
use chrono::Utc;
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use tokio::sync::Mutex;
use tracing::{info, warn};
use uuid::Uuid;

/// Signal types Zior emits (mirrors contracts::signal::SignalType).
/// We define them locally here to avoid a circular crate dependency.
/// The Schema Registry enforces they match the contract.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ZiorSignalType {
    AudioAnalysis,
    VelocityUpdate,
    ClusterEmergence,
}

/// A signal envelope from Zior to AethyrRank.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ZiorSignal {
    pub signal_id: Uuid,
    pub source_brain: String, // "zior"
    pub signal_type: ZiorSignalType,
    pub payload: serde_json::Value,
    pub schema_version: u32,
    pub emitted_at: chrono::DateTime<Utc>,
    pub trace_id: Uuid,
}

impl ZiorSignal {
    pub fn new(signal_type: ZiorSignalType, payload: serde_json::Value) -> Self {
        Self {
            signal_id: Uuid::new_v4(),
            source_brain: "zior".to_string(),
            signal_type,
            payload,
            schema_version: 1,
            emitted_at: Utc::now(),
            trace_id: Uuid::new_v4(),
        }
    }
}

/// Registration payload Zior sends to AethyrRank.
#[derive(Debug, Serialize)]
struct RegistrationPayload {
    brain_id: &'static str,
    schema_version: u32,
    port: u16,
    host: String,
    signal_types: Vec<&'static str>,
    description: &'static str,
    registered_at: chrono::DateTime<Utc>,
}

/// Zior's BrainPort client.
pub struct ZiorBrainClient {
    aethyrrank_url: String,
    port: u16,
    http: reqwest::Client,
    /// In-memory signal buffer for when AethyrRank is temporarily down.
    /// Capacity: 10,000 signals. FIFO drop when full.
    signal_buffer: Arc<Mutex<Vec<ZiorSignal>>>,
}

const BUFFER_CAPACITY: usize = 10_000;

impl ZiorBrainClient {
    pub fn new(aethyrrank_url: String, port: u16) -> Arc<Self> {
        let http = reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(5))
            .build()
            .expect("http client");
        Arc::new(Self {
            aethyrrank_url,
            port,
            http,
            signal_buffer: Arc::new(Mutex::new(Vec::new())),
        })
    }

    /// Register Zior with AethyrRank.
    async fn register(&self) -> Result<()> {
        let payload = RegistrationPayload {
            brain_id: "zior",
            schema_version: 1,
            port: self.port,
            host: std::env::var("POD_IP").unwrap_or_else(|_| "localhost".to_string()),
            signal_types: vec!["audio_analysis", "velocity_update", "cluster_emergence"],
            description: "Music cortex — audio intelligence and psychological signal extraction",
            registered_at: Utc::now(),
        };
        let url = format!("{}/brain/register", self.aethyrrank_url);
        let resp = self.http.post(&url).json(&payload).send().await?;
        if !resp.status().is_success() {
            anyhow::bail!("AethyrRank rejected registration: {}", resp.status());
        }
        info!("registered with AethyrRank at {}", self.aethyrrank_url);
        Ok(())
    }

    /// Send a heartbeat to AethyrRank.
    async fn heartbeat(&self) -> Result<()> {
        let url = format!("{}/brain/heartbeat", self.aethyrrank_url);
        let body = serde_json::json!({"brain_id": "zior", "ts": Utc::now()});
        self.http.post(&url).json(&body).send().await?;
        Ok(())
    }

    /// Emit a signal to AethyrRank. Buffers if unavailable.
    pub async fn emit(&self, signal: ZiorSignal) {
        let url = format!("{}/brain/signal", self.aethyrrank_url);
        match self.http.post(&url).json(&signal).send().await {
            Ok(resp) if resp.status().is_success() => {
                // Flush buffer if we have queued signals
                self.flush_buffer().await;
            }
            Ok(resp) => {
                warn!(status = %resp.status(), "AethyrRank rejected signal");
            }
            Err(e) => {
                warn!(error = %e, "AethyrRank unreachable — buffering signal");
                self.buffer_signal(signal).await;
            }
        }
    }

    /// Buffer a signal for later replay.
    async fn buffer_signal(&self, signal: ZiorSignal) {
        let mut buf = self.signal_buffer.lock().await;
        if buf.len() >= BUFFER_CAPACITY {
            buf.remove(0); // FIFO drop
        }
        buf.push(signal);
    }

    /// Replay buffered signals. Called after successful reconnect.
    async fn flush_buffer(&self) {
        let mut buf = self.signal_buffer.lock().await;
        if buf.is_empty() {
            return;
        }
        let signals: Vec<ZiorSignal> = buf.drain(..).collect();
        drop(buf);

        info!("flushing {} buffered signals to AethyrRank", signals.len());
        let url = format!("{}/brain/signal", self.aethyrrank_url);
        for signal in signals {
            if let Err(e) = self.http.post(&url).json(&signal).send().await {
                warn!(error = %e, "flush failed — signals lost");
                break;
            }
        }
    }

    /// Start the registration + heartbeat background loop.
    /// Runs forever. Never panics. Retries on all failures.
    pub fn start_loop(self: Arc<Self>) {
        tokio::spawn(async move {
            // Wait 2s for AethyrRank to start first
            tokio::time::sleep(std::time::Duration::from_secs(2)).await;

            let mut registered = false;
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(30));

            loop {
                interval.tick().await;

                if !registered {
                    match self.register().await {
                        Ok(()) => {
                            registered = true;
                        }
                        Err(e) => {
                            warn!(error = %e, "registration failed, will retry in 30s");
                        }
                    }
                } else {
                    match self.heartbeat().await {
                        Ok(()) => {}
                        Err(e) => {
                            warn!(error = %e, "heartbeat failed — will re-register next tick");
                            registered = false;
                        }
                    }
                }
            }
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn signal_has_valid_uuid() {
        let s = ZiorSignal::new(
            ZiorSignalType::AudioAnalysis,
            serde_json::json!({"track_id": "t1"}),
        );
        assert!(!s.signal_id.is_nil());
        assert_eq!(s.source_brain, "zior");
        assert_eq!(s.schema_version, 1);
    }

    #[test]
    fn signal_serialises_snake_case() {
        let s = ZiorSignal::new(ZiorSignalType::ClusterEmergence, serde_json::json!({}));
        let json = serde_json::to_string(&s).unwrap();
        assert!(
            json.contains("cluster_emergence"),
            "expected snake_case: {}",
            json
        );
    }
}
