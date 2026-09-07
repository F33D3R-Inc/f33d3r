//! Auralis — F33D3R Frequencies.
//!
//! The brain that owns the Frequency: its lifecycle, its participants and
//! roles, the speaker queue, moderation, the events every other brain hears,
//! and (from Phase 3) the audio SFU. Nantar is the only thing that renders
//! it; the browser never speaks to these routes.

mod api;
mod auth;
mod config;
mod domain;
mod error;
mod events;
mod herald;
mod identity;
mod manhattan_outbox;
mod migrate;
mod observ;
mod repository;
mod security;
mod state;
mod tasks;
mod telemetry;

#[cfg(test)]
mod db_tests;
#[cfg(test)]
mod testdb;

use std::sync::Arc;

use manhattan_client::Manhattan;
use tracing::info;

use auth::InternalApiKey;
use config::Config;
use repository::redis::Hot;
use security::tokens::Signer;
use state::AppState;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    // `auralis --write-schemas <dir>` regenerates events/schemas/frequency.*.json
    // from the code, so the contract on disk can never drift from the wire.
    let args: Vec<String> = std::env::args().collect();
    if args.get(1).map(String::as_str) == Some("--write-schemas") {
        let dir = args
            .get(2)
            .map(String::as_str)
            .unwrap_or("../events/schemas");
        return write_schemas(dir);
    }

    observ::init("auralis")?;
    telemetry::metrics::describe();

    let config = Config::from_env();

    // Two secrets, two refusals. Without the internal key every route is
    // open; without the signal secret no session can be minted. Neither is a
    // degraded mode this brain will run in.
    let internal_api_key = InternalApiKey::from_env();
    if internal_api_key.is_empty() {
        anyhow::bail!("INTERNAL_API_KEY is not set: auralis refuses to start with its control plane unauthenticated");
    }
    let signer = Signer::new(&config.signal_secret)?;

    let pool = sqlx::PgPool::connect(&config.database_url).await?;
    migrate::run(&pool).await?;
    info!("frequencies schema up to date");

    let hot = Hot::connect(
        &config.redis_url,
        config.node_id.clone(),
        config.presence_ttl,
        config.lease_ttl,
    )
    .await?;
    hot.ping().await?;
    info!(node_id = %config.node_id, "redis reachable");
    // The media socket is Phase 3; its address is part of the compose and
    // firewall contract now, so it is read and stated at every boot.
    info!(
        public_host = %config.public_host,
        media_udp_port = config.media_udp_port,
        "media socket contract (bound from Phase 3)"
    );

    // Auralis owns Frequencies. It owns no identity: elohim-veni does, and
    // every host and participant row here references a name this client
    // resolved.
    let manhattan = Manhattan::from_env("auralis")?;
    info!(
        configured = manhattan.configured(),
        "Manhattan naming plane"
    );
    manhattan_outbox::spawn(config.database_url.clone(), manhattan.clone());

    // The event fabric. The drain is the only producer in this brain.
    events::drain::Drain::new(pool.clone(), &config.kafka_brokers, config.drain_interval)?.spawn();
    info!(brokers = %config.kafka_brokers, topic = events::TOPIC, "event drain running");

    let herald_client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()?;

    let state = AppState {
        pool,
        hot,
        manhattan,
        herald_client,
        internal_api_key,
        signer,
        config: Arc::new(config),
    };

    // Look at what was open when the last process died before serving anyone.
    tasks::reconcile_on_boot(&state).await?;
    tasks::spawn_all(state.clone());

    let addr = format!("0.0.0.0:{}", state.config.port);
    let app = api::router(state);
    info!("Auralis frequencies brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}

fn write_schemas(dir: &str) -> anyhow::Result<()> {
    std::fs::create_dir_all(dir)?;
    for t in events::EventType::ALL {
        let path = format!("{dir}/{}.json", t.as_str());
        let body = serde_json::to_string_pretty(&events::schemas::json_schema(t))?;
        std::fs::write(&path, format!("{body}\n"))?;
        println!("wrote {path}");
    }
    Ok(())
}
