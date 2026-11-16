mod api;
mod auth;
mod bandit;
mod config;
mod contracts;
mod db;
mod import;
mod learning;
mod migrate;
mod model_store;
mod observ;
mod persist;
mod pipeline;
mod safety;
mod scoring;

use std::sync::Arc;
use std::time::Duration;

use tracing::info;

use crate::api::handlers::AppState;
use crate::config::{AppConfig, Runtime};
use crate::learning::consumer::{self, Intake};
use crate::learning::features::FeatureCache;
use crate::model_store::ModelStore;

const FEATURE_SWEEP: Duration = Duration::from_secs(300);

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("aethyrrank")?;

    // `aethyrrank-engine import-feed-checkpoints` — the one-shot move of the
    // old checkpoints out of f33d3r_feed (src/import.rs).
    if let Some(cmd) = std::env::args().nth(1) {
        return match cmd.as_str() {
            "import-feed-checkpoints" => import::run().await,
            other => anyhow::bail!("unknown command {other:?}; the only command is import-feed-checkpoints"),
        };
    }

    let cfg = AppConfig::load()?;
    let rt = Runtime::from_env()?;

    // f33d3r_rank: created if missing, migrated, restored. A process that
    // cannot keep the ranking state does not serve.
    let pool = db::connect(&rt.database_url, 8).await?;
    migrate::run(&pool).await?;

    let model_store = Arc::new(ModelStore::new(cfg.bandit.feature_dim));
    let restored = persist::restore_all(&pool, &model_store).await?;
    info!(models = restored, "LinUCB models restored from f33d3r_rank");

    let features = Arc::new(FeatureCache::new());
    {
        let features = Arc::clone(&features);
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(FEATURE_SWEEP);
            loop {
                tick.tick().await;
                features.sweep();
            }
        });
    }

    consumer::spawn(Intake {
        pool: pool.clone(),
        brokers: rt.kafka_brokers.clone(),
        store: Arc::clone(&model_store),
        cache: Arc::clone(&features),
    });

    let state = Arc::new(AppState {
        config: Arc::new(cfg.clone()),
        model_store,
        features,
        internal_api_key: rt.internal_api_key,
    });

    let app = api::router(state);
    let addr = format!("{}:{}", cfg.server.host, cfg.server.port);
    info!("AethyrRank engine listening on {}", addr);
    let listener = tokio::net::TcpListener::bind(&addr).await?;

    // Every learned change is committed with its batch, so shutdown has
    // nothing to flush: an uncommitted batch is replayed from its offsets.
    axum::serve(listener, app)
        .with_graceful_shutdown(async {
            let _ = tokio::signal::ctrl_c().await;
            info!("shutdown signal received");
        })
        .await?;
    Ok(())
}
