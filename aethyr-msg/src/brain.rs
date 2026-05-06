/// F33D3R Brain integration for AethyrMsg.
///
/// AethyrMsg registers as a brain module in the F33D3R multi-brain system:
///   Brain ID:  "aethyr-msg"
///   Port:      8092
///   Role:      Private/group communication, token signaling, trust signal emitter
///
/// Signals emitted to AethyrRank:
///   - message_sent      → small positive signal (active communicator)
///   - group_active      → creator trust signal (running active group = credibility)
///   - token_transfer    → strong positive signal (tip=0.8, subscription=2.0)
///   - message_volume_24h → daily activity signal for velocity scoring
///
/// Signals used from AethyrRank:
///   - realm_level       → higher realm = relaxed rate limits + more prekeys
///   - creator_status    → can create groups > 150 members
///
/// The brain heartbeat registers us with aethyr-schema-registry every 15 seconds.
/// If we miss 3 heartbeats, we're marked DEGRADED and feed-engine falls back.

use std::sync::Arc;
use anyhow::Result;
use reqwest::Client;
use serde_json::json;
use tracing::{info, warn};

pub struct BrainClient {
    registry_url:   String,
    aethyrrank_url: String,
    http:           Client,
    brain_id:       String,
    port:           u16,
}

impl BrainClient {
    pub fn new(registry_url: String, aethyrrank_url: String, port: u16) -> Arc<Self> {
        Arc::new(Self {
            registry_url,
            aethyrrank_url,
            http: Client::builder()
                .timeout(std::time::Duration::from_secs(3))
                .build()
                .expect("reqwest client"),
            brain_id: "aethyr-msg".into(),
            port,
        })
    }

    /// Register with schema registry on startup.
    pub async fn register(&self) -> Result<()> {
        let res = self.http
            .post(format!("{}/brain/register", self.registry_url))
            .json(&json!({
                "brain_id":    self.brain_id,
                "version":     "1.0.0",
                "protocol":    "AMP_v1",
                "port":        self.port,
                "capabilities": ["messaging", "groups", "token_transfer", "e2ee"],
                "status":      "online"
            }))
            .send()
            .await;

        match res {
            Ok(r) if r.status().is_success() => {
                info!("registered with schema registry as '{}'", self.brain_id);
                Ok(())
            }
            Ok(r) => {
                warn!("registry registration returned {}", r.status());
                Ok(()) // non-fatal
            }
            Err(e) => {
                warn!("registry unreachable: {e} — running standalone");
                Ok(())
            }
        }
    }

    /// Heartbeat loop — runs forever, pings registry every 15s.
    pub async fn run_heartbeat(self: Arc<Self>) {
        let mut ticker = tokio::time::interval(std::time::Duration::from_secs(15));
        loop {
            ticker.tick().await;
            let res = self.http
                .post(format!("{}/brain/heartbeat", self.registry_url))
                .json(&json!({
                    "brain_id": self.brain_id,
                    "status":   "online",
                }))
                .send()
                .await;
            if let Err(e) = res {
                warn!("heartbeat failed: {e}");
            }
        }
    }

    /// Emit a signal to AethyrRank feedback endpoint.
    ///
    /// `user_id`:    The acting user's F33D3R user ID (not their AMP identity).
    /// `content_id`: The creator's user ID being tipped/subscribed to.
    /// `event_type`: One of "tip", "subscription", "message_sent", "group_active".
    pub async fn emit_signal(&self, user_id: &str, content_id: &str, event_type: &str) {
        let reward = match event_type {
            "subscription"   => 2.0_f64,
            "tip"            => 0.8,
            "group_active"   => 0.3,
            "message_sent"   => 0.05,
            _                => 0.1,
        };

        let payload = json!({
            "user_id":    user_id,
            "session_id": uuid::Uuid::new_v4().to_string(),
            "surface":    "messaging",
            "events": [{
                "content_id":          content_id,
                "event_type":          event_type,
                "position_at_display": 0,
                "timestamp":           chrono::Utc::now(),
                "exploration_slot":    false,
                "reward_override":     reward,
            }]
        });

        if let Err(e) = self.http
            .post(format!("{}/feedback", self.aethyrrank_url))
            .json(&payload)
            .send()
            .await
        {
            warn!("AethyrRank signal emit failed: {e}");
        }
    }
}
