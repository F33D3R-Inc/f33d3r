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
    pub herald_url: String,

    /// This process, as the owner of media sessions. Defaults to the hostname,
    /// which under compose is the container id — unique per replica.
    pub node_id: String,

    /// Signs the short-lived signaling tokens handed to browsers. Distinct
    /// from INTERNAL_API_KEY: that secret is shared with every brain, this one
    /// is Auralis's alone, so a peer brain cannot mint a media session.
    pub signal_secret: String,
    pub signal_token_ttl: Duration,

    /// Host and UDP port the media socket is advertised on. Phase 3 binds
    /// them; they are read now so the compose contract is settled.
    pub public_host: String,
    pub media_udp_port: u16,

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
    /// Cooldown between identical Herald notifications to one person.
    pub notify_cooldown: Duration,
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
            herald_url: env_or("HERALD_URL", "http://herald:8105"),
            node_id: env_or("AURALIS_NODE_ID", &hostname),
            signal_secret: std::env::var("AURALIS_SIGNAL_SECRET").unwrap_or_default(),
            signal_token_ttl: env_secs("AURALIS_SIGNAL_TOKEN_TTL_SECS", 60),
            public_host: env_or("AURALIS_PUBLIC_HOST", "localhost"),
            media_udp_port: std::env::var("AURALIS_MEDIA_UDP_PORT")
                .ok()
                .and_then(|v| v.parse().ok())
                .unwrap_or(40000),
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
            notify_cooldown: env_secs("AURALIS_NOTIFY_COOLDOWN_SECS", 300),
            starting_timeout: env_secs("AURALIS_STARTING_TIMEOUT_SECS", 60),
        }
    }
}
