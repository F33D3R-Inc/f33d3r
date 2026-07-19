mod api;
mod config;
mod db;
mod observ;
mod validator;

use std::net::SocketAddr;
use std::sync::Arc;
use tracing::info;

use api::handlers::AppState;
use config::AppConfig;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("schema_registry")?;

    let cfg = AppConfig::load()?;

    // Database
    let pool = db::connect(&cfg.database.url, cfg.database.max_connections).await?;
    db::run_migrations(&pool).await?;

    // Staleness checker — marks offline brains in Postgres every 60s
    {
        let pool_clone = pool.clone();
        let threshold = cfg.registry.stale_threshold_secs;
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(std::time::Duration::from_secs(60));
            loop {
                interval.tick().await;
                match db::mark_stale_brains(&pool_clone, threshold).await {
                    Ok(n) if n > 0 => tracing::warn!("marked {} brains stale", n),
                    Ok(_) => {}
                    Err(e) => tracing::warn!("staleness check error: {}", e),
                }
            }
        });
    }

    let state = Arc::new(AppState {
        pool,
        config: cfg.registry.clone(),
    });

    let app = api::router(state);
    let addr = SocketAddr::from((
        cfg.server
            .host
            .parse::<std::net::IpAddr>()
            .unwrap_or(std::net::IpAddr::V4(std::net::Ipv4Addr::new(0, 0, 0, 0))),
        cfg.server.port,
    ));

    info!("aethyr-schema-registry listening on {}", addr);
    info!(
        "strict compatibility: {}",
        cfg.registry.strict_compatibility
    );

    let listener = tokio::net::TcpListener::bind(addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
