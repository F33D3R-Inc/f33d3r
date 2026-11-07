//! Process configuration, read once from the environment at boot.
//!
//! Plain `std::env::var`, the way every brain does it (themis/src/config.rs is
//! the model). Required values `expect` so a misconfigured deployment fails
//! at boot with the variable named, not later with a connection error.

use std::time::Duration;

#[derive(Clone, Debug)]
pub struct Config {
    pub port: String,
    pub database_url: String,
    pub redis_url: String,
    pub kafka_brokers: String,

    /// This process, as the owner of media sessions. Defaults to the hostname,
    /// which under compose is the container id — unique per replica.
    pub node_id: String,

    /// LiveKit, reached over the container network. The secret signs join tokens
    /// and is Auralis's alone among the brains.
    pub livekit_url: String,
    /// Where the replay volume is mounted here. Deleting a replay removes its file.
    pub replay_dir: String,
    pub livekit_api_key: String,
    pub livekit_api_secret: String,

    /// Presence is a TTL, not a heartbeat table. A participant whose key has
    /// expired is swept out of the live sets and marked left.
    pub presence_ttl: Duration,
    /// How often the presence sweeper runs.
    pub presence_sweep_interval: Duration,
    /// How long a Frequency keeps waiting for its host before a co-host
    /// continues it, or it ends.
    pub host_grace: Duration,
    /// Media-node lease TTL. Renewed at a third of this.
    pub lease_ttl: Duration,
    /// How often the outbox drain looks for unpublished events.
    pub drain_interval: Duration,
    /// A Frequency stuck in `starting` for longer than this is failed on
    /// reconciliation.
    pub starting_timeout: Duration,
}

fn env_or(name: &str, default: &str) -> String {
    std::env::var(name).unwrap_or_else(|_| default.into())
}

fn env_secs(name: &str, default: u64) -> Duration {
    Duration::from_secs(
        std::env::var(name)
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(default),
    )
}

impl Config {
    pub fn from_env() -> Self {
        let hostname = std::env::var("HOSTNAME").unwrap_or_else(|_| "auralis".into());
        Self {
            port: env_or("PORT", "8108"),
            database_url: std::env::var("DATABASE_URL").expect("DATABASE_URL must be set"),
            redis_url: env_or("REDIS_URL", "redis://redis:6379"),
            // Required, not defaulted: an event fabric this brain cannot reach
            // is a Frequency nobody else on the platform will hear about.
            kafka_brokers: std::env::var("KAFKA_BROKERS").expect("KAFKA_BROKERS must be set"),
            node_id: env_or("AURALIS_NODE_ID", &hostname),
            livekit_url: env_or("LIVEKIT_URL", "http://livekit:7880"),
            replay_dir: env_or("REPLAY_DIR", "/replays"),
            livekit_api_key: std::env::var("LIVEKIT_API_KEY").unwrap_or_default(),
            livekit_api_secret: std::env::var("LIVEKIT_API_SECRET").unwrap_or_default(),
            presence_ttl: env_secs("AURALIS_PRESENCE_TTL_SECS", 45),
            presence_sweep_interval: env_secs("AURALIS_PRESENCE_SWEEP_SECS", 10),
            host_grace: env_secs("AURALIS_HOST_GRACE_SECS", 120),
            lease_ttl: env_secs("AURALIS_LEASE_TTL_SECS", 30),
            drain_interval: Duration::from_millis(
                std::env::var("AURALIS_DRAIN_INTERVAL_MS")
                    .ok()
                    .and_then(|v| v.parse().ok())
                    .unwrap_or(500),
            ),
            starting_timeout: env_secs("AURALIS_STARTING_TIMEOUT_SECS", 60),
        }
    }
}
