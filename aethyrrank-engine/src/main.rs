mod api;
mod bandit;
mod config;
mod contracts;
mod middleware;
mod model_store;
mod persist;
mod pipeline;
mod safety;
mod scoring;
mod session;

use std::sync::Arc;
use tracing::{info, warn};
use tracing_subscriber::{layer::SubscriberExt, util::SubscriberInitExt, EnvFilter};

use crate::api::handlers::AppState;
use crate::config::AppConfig;
use crate::contracts::brain_registry::BrainRegistry;
use crate::model_store::ModelStore;
use crate::session::cache::SessionFeatureCache;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::registry()
        .with(EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()))
        .with(tracing_subscriber::fmt::layer())
        .init();

    let cfg = AppConfig::load()?;
    let surfaces: Vec<&str> = cfg
        .model_store
        .surfaces
        .iter()
        .map(String::as_str)
        .collect();
    let model_store = Arc::new(ModelStore::new(&surfaces, cfg.bandit.feature_dim));
    let session_cache = Arc::new(SessionFeatureCache::new());

    // ── LinUCB persistence ────────────────────────────────────────────────────
    // Connect to Postgres and restore any previously checkpointed model state.
    // If DATABASE_URL is absent (e.g. local dev without DB) we degrade gracefully
    // to ephemeral in-memory learning rather than crashing.
    let db_pool = match std::env::var("DATABASE_URL") {
        Ok(url) => match persist::create_pool(&url).await {
            Ok(pool) => {
                if let Err(e) = persist::ensure_schema(&pool).await {
                    warn!("failed to create linucb_checkpoints table: {e}");
                    None
                } else {
                    match persist::restore_all(&pool, &model_store).await {
                        Ok(n) => info!("restored {n} LinUCB surface(s) from Postgres"),
                        Err(e) => warn!("LinUCB restore failed (starting fresh): {e}"),
                    }
                    Some(pool)
                }
            }
            Err(e) => {
                warn!("cannot connect to Postgres ({e}) — running without LinUCB persistence");
                None
            }
        },
        Err(_) => {
            warn!("DATABASE_URL not set — LinUCB state is ephemeral (survives only until restart)");
            None
        }
    };

    // Start the 60-second checkpoint loop if we have a DB connection.
    if let Some(pool) = db_pool.clone() {
        persist::start_checkpoint_loop(pool, Arc::clone(&model_store), 60);
    }

    let brain_registry = BrainRegistry::new();

    // Start staleness checker — marks offline brains after missed heartbeats
    {
        let reg = Arc::clone(&brain_registry);
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(30));
            loop {
                interval.tick().await;
                reg.check_staleness();
            }
        });
    }

    let state = Arc::new(AppState {
        config: Arc::new(cfg.clone()),
        model_store: Arc::clone(&model_store),
        session_cache,
        brain_registry,
    });

    let app = api::router(state);
    let addr = format!("{}:{}", cfg.server.host, cfg.server.port);
    info!("AethyrRank engine listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;

    // Graceful shutdown: flush LinUCB state one final time before exit.
    let shutdown_store = Arc::clone(&model_store);
    let shutdown_pool = db_pool.clone();
    tokio::spawn(async move {
        tokio::signal::ctrl_c().await.ok();
        info!("shutdown signal received — flushing LinUCB checkpoints");
        if let Some(pool) = shutdown_pool {
            if let Err(e) = persist::flush_all(&pool, &shutdown_store).await {
                warn!("final LinUCB flush failed: {e}");
            } else {
                info!("LinUCB state saved to Postgres");
            }
        }
        std::process::exit(0);
    });

    axum::serve(listener, app).await?;
    Ok(())
}
