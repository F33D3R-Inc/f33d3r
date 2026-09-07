mod api;
mod audio;
mod behavioral;
mod brain;
mod config;
mod jung;
mod observ;
mod signals;
mod store;
mod vector;

use std::sync::Arc;
use tracing::info;

use crate::api::handlers::ZiorState;
use crate::behavioral::events::BehavioralStore;
use crate::behavioral::velocity::VelocityTracker;
use crate::brain::client::ZiorBrainClient;
use crate::config::AppConfig;
use crate::signals::emitter::SignalEmitter;
use crate::store::TrackStore;
use crate::vector::VectorStore;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("zior")?;

    let cfg = AppConfig::load()?;
    let track_store = Arc::new(TrackStore::new());
    let vector_store = Arc::new(tokio::sync::RwLock::new(VectorStore::new()));
    let behavior_store = Arc::new(BehavioralStore::new());
    let velocity_trackers = Arc::new(dashmap::DashMap::<String, VelocityTracker>::new());

    let emitter = Arc::new(SignalEmitter::new(
        Arc::clone(&track_store),
        cfg.aethyr.clone(),
    ));

    let state = Arc::new(ZiorState {
        config: Arc::new(cfg.clone()),
        track_store,
        vector_store,
        behavior_store,
        velocity_trackers,
        emitter,
    });

    let app = api::router(Arc::clone(&state));
    let addr = format!("{}:{}", cfg.server.host, cfg.server.port);

    info!("Zior engine listening on {}", addr);
    info!("AethyrRank content store: {}", cfg.aethyr.content_store_url);

    // Start BrainPort registration loop — registers with AethyrRank and sends heartbeats
    let brain_client = ZiorBrainClient::new(cfg.aethyr.content_store_url.clone(), cfg.server.port);
    brain_client.start_loop();
    info!(
        "brain registration loop started → target: {}",
        cfg.aethyr.content_store_url
    );

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;

    Ok(())
}
