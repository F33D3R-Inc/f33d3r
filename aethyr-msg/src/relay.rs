/// AMP v1 relay logic — rate limiting, PoW verification, routing decisions.
///
/// The relay is intentionally stateless with respect to session crypto.
/// It only tracks: identity registrations, message TTLs, rate windows, known contacts.
///
/// Transport abstraction:
///   Current:   HTTP/2 (Axum)
///   Upgrade:   QUIC via quinn (AMP v2) — swap the transport layer, protocol stays same
///   Optional:  WebSocket push for online recipients (reduces polling latency to ~0)

use std::sync::Arc;
use dashmap::DashMap;
use chrono::{Utc, Duration};
use sqlx::PgPool;
use tracing::{info, warn};

use crate::crypto::verify_pow;
use crate::protocol::SecurityMode;

// ── In-memory rate limiter ────────────────────────────────────────────────────
// Uses a sliding window of 1 minute. Backed by in-memory DashMap for speed;
// DB rate_limits table provides persistence across restarts.

const RATE_WINDOW_SECS:      i64 = 60;
const RATE_LIMIT_KNOWN:      u32 = 300; // known contacts: 300 msgs/min
const RATE_LIMIT_UNKNOWN:    u32 = 10;  // first-contact: 10 msgs/min
const RATE_LIMIT_PARANOID:   u32 = 50;  // paranoid mode: 50 msgs/min

#[derive(Default)]
struct Window {
    count: u32,
    reset: chrono::DateTime<Utc>,
}

pub struct RateLimiter {
    windows: DashMap<String, Window>,
}

impl RateLimiter {
    pub fn new() -> Arc<Self> {
        Arc::new(Self { windows: DashMap::new() })
    }

    /// Returns true if the identity is within rate limits for this security mode.
    pub fn allow(&self, identity: &str, is_known: bool, mode: SecurityMode) -> bool {
        let limit = if is_known {
            match mode {
                SecurityMode::Paranoid => RATE_LIMIT_PARANOID,
                _                      => RATE_LIMIT_KNOWN,
            }
        } else {
            RATE_LIMIT_UNKNOWN
        };

        let mut entry = self.windows.entry(identity.to_string()).or_default();
        let now = Utc::now();
        if now > entry.reset {
            entry.count = 0;
            entry.reset = now + Duration::seconds(RATE_WINDOW_SECS);
        }
        if entry.count >= limit {
            warn!("rate limit exceeded for {}", &identity[..8.min(identity.len())]);
            return false;
        }
        entry.count += 1;
        true
    }

    /// Sweep stale windows (call every ~5 minutes).
    pub fn sweep(&self) {
        let now = Utc::now();
        self.windows.retain(|_, w| w.reset > now);
    }
}

// ── PoW verification ──────────────────────────────────────────────────────────

/// Build the PoW challenge for a first-contact message.
/// Challenge = BLAKE3(sender_identity || recipient_identity || day_bucket).
/// Day bucket prevents precomputing PoW across days.
pub fn pow_challenge(sender: &str, recipient: &str) -> Vec<u8> {
    let day = (Utc::now().timestamp() / 86400).to_le_bytes();
    blake3::hash(&[sender.as_bytes(), recipient.as_bytes(), &day].concat())
        .as_bytes()
        .to_vec()
}

/// Verify proof-of-work for a first-contact message.
pub fn verify_first_contact_pow(
    sender:    &str,
    recipient: &str,
    nonce:     u64,
    mode:      SecurityMode,
) -> bool {
    let challenge = pow_challenge(sender, recipient);
    verify_pow(&challenge, nonce, mode.pow_difficulty())
}

// ── Online presence via WebSocket ─────────────────────────────────────────────
// Simple in-memory registry of connected WebSocket sessions.
// When a message arrives for an online recipient, push it immediately
// instead of waiting for the recipient to poll.

use tokio::sync::broadcast;
use std::collections::HashMap;
use std::sync::Mutex;

pub type PushSender = broadcast::Sender<String>; // message_id

pub struct PushRegistry {
    /// identity → broadcast channel for live push
    subs: Mutex<HashMap<String, PushSender>>,
}

impl PushRegistry {
    pub fn new() -> Arc<Self> {
        Arc::new(Self { subs: Mutex::new(HashMap::new()) })
    }

    /// Subscribe an identity to receive push notifications.
    /// Returns a Receiver the WebSocket handler should listen on.
    pub fn subscribe(&self, identity: String) -> broadcast::Receiver<String> {
        let mut map = self.subs.lock().unwrap();
        let tx = map.entry(identity.clone())
            .or_insert_with(|| {
                let (tx, _) = broadcast::channel(256);
                tx
            });
        let rx = tx.subscribe();
        info!("push subscribed: {}", &identity[..8.min(identity.len())]);
        rx
    }

    /// Notify an online recipient that a new message is waiting.
    /// Returns true if the recipient was online (had an active subscription).
    pub fn notify(&self, identity: &str, message_id: &str) -> bool {
        let map = self.subs.lock().unwrap();
        if let Some(tx) = map.get(identity) {
            let _ = tx.send(message_id.to_string());
            return tx.receiver_count() > 0;
        }
        false
    }

    /// Unsubscribe when WebSocket disconnects.
    pub fn unsubscribe(&self, identity: &str) {
        self.subs.lock().unwrap().remove(identity);
    }
}

// ── Background maintenance ────────────────────────────────────────────────────

pub async fn run_maintenance(pool: PgPool, rate_limiter: Arc<RateLimiter>) {
    let mut ticker = tokio::time::interval(std::time::Duration::from_secs(60));
    loop {
        ticker.tick().await;

        // Expire TTL'd messages
        match crate::db::expire_messages(&pool).await {
            Ok(n) if n > 0 => info!("expired {} messages", n),
            Err(e)         => warn!("TTL sweep error: {e}"),
            _              => {}
        }

        // Clean stale rate limit records
        if let Err(e) = crate::db::clean_rate_limits(&pool).await {
            warn!("rate limit cleanup error: {e}");
        }

        // Sweep in-memory rate windows
        rate_limiter.sweep();
    }
}
