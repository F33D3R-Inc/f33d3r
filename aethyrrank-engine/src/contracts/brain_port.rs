//! BrainPort — the trait every F33D3R brain implements.
//!
//! Implementing this trait is the entire contract for connecting a new brain
//! region to AethyrRank. Three methods: register, heartbeat, emit.
//!
//! Design:
//!   - Registration is idempotent (safe to call multiple times)
//!   - Heartbeat failure is logged but never panics the brain
//!   - Signal emission is fire-and-forget (async, non-blocking)

use anyhow::Result;
use async_trait::async_trait;

use crate::contracts::brain_id::BrainId;
use crate::contracts::signal::{BrainRegistration, BrainSignal, SignalType};

/// The contract every brain region implements to join the F33D3R system.
#[async_trait]
pub trait BrainPort: Send + Sync {
    /// This brain's identifier.
    fn brain_id(&self) -> BrainId;

    /// Current schema version this brain is emitting.
    fn schema_version(&self) -> u32;

    /// Signal types this brain emits (registered with Schema Registry).
    fn supported_signals(&self) -> Vec<SignalType>;

    /// Human-readable description of what this brain does.
    fn description(&self) -> &'static str {
        self.brain_id().description()
    }

    /// Build the registration payload.
    fn registration(&self, host: &str, port: u16) -> BrainRegistration {
        BrainRegistration {
            brain_id: self.brain_id(),
            schema_version: self.schema_version(),
            port,
            host: host.to_string(),
            signal_types: self.supported_signals(),
            description: self.description().to_string(),
            registered_at: chrono::Utc::now(),
        }
    }
}

/// Default HTTP-based BrainPort client — used by all brain regions
/// to communicate with AethyrRank's /brain/* endpoints.
pub struct BrainPortClient {
    pub brain_id: BrainId,
    pub aethyrrank_url: String,
    pub http: reqwest::Client,
    pub schema_version: u32,
    pub signals: Vec<SignalType>,
    pub port: u16,
    pub host: String,
}

impl BrainPortClient {
    pub fn new(
        brain_id: BrainId,
        aethyrrank_url: String,
        port: u16,
        schema_version: u32,
        signals: Vec<SignalType>,
    ) -> Self {
        let http = reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(5))
            .build()
            .expect("http client");
        Self {
            brain_id,
            aethyrrank_url,
            http,
            schema_version,
            signals,
            port,
            host: "localhost".to_string(),
        }
    }

    /// Register with AethyrRank. Safe to call on every startup.
    pub async fn register(&self) -> Result<()> {
        let payload = BrainRegistration {
            brain_id: self.brain_id,
            schema_version: self.schema_version,
            port: self.port,
            host: self.host.clone(),
            signal_types: self.signals.clone(),
            description: self.brain_id.description().to_string(),
            registered_at: chrono::Utc::now(),
        };

        let url = format!("{}/brain/register", self.aethyrrank_url);
        let resp = self.http.post(&url).json(&payload).send().await?;
        if !resp.status().is_success() {
            anyhow::bail!("registration rejected: {}", resp.status());
        }
        tracing::info!(brain = %self.brain_id, "registered with AethyrRank");
        Ok(())
    }

    /// Send a heartbeat. Called every 30s by background task.
    pub async fn heartbeat(&self) -> Result<()> {
        let url = format!("{}/brain/heartbeat", self.aethyrrank_url);
        let body = serde_json::json!({
            "brain_id":  self.brain_id,
            "ts":        chrono::Utc::now(),
        });
        self.http.post(&url).json(&body).send().await?;
        Ok(())
    }

    /// Emit a signal to AethyrRank. Fire-and-forget.
    pub fn emit_signal(&self, signal: BrainSignal) {
        let http = self.http.clone();
        let url = format!("{}/brain/signal", self.aethyrrank_url);
        tokio::spawn(async move {
            if let Err(e) = http.post(&url).json(&signal).send().await {
                tracing::warn!(error = %e, "brain signal delivery failed");
            }
        });
    }

    /// Start a background heartbeat task. Runs every 30s until process exits.
    /// Registration is retried on failure — brain never crashes due to AethyrRank being down.
    pub fn start_heartbeat_loop(self: std::sync::Arc<Self>) {
        tokio::spawn(async move {
            let mut registered = false;
            loop {
                if !registered {
                    match self.register().await {
                        Ok(_) => {
                            registered = true;
                        }
                        Err(e) => {
                            tracing::warn!(
                                brain = %self.brain_id,
                                error = %e,
                                "registration failed, will retry in 30s"
                            );
                        }
                    }
                } else {
                    if let Err(e) = self.heartbeat().await {
                        tracing::warn!(brain = %self.brain_id, error = %e, "heartbeat failed");
                        registered = false; // will re-register next iteration
                    }
                }
                tokio::time::sleep(std::time::Duration::from_secs(30)).await;
            }
        });
    }
}
