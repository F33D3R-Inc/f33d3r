mod api;
mod audio;
mod auth;
mod config;
mod jung;
mod observ;

use std::sync::Arc;
use tracing::info;

use crate::api::handlers::ZiorState;
use crate::config::AppConfig;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("zior")?;

    let cfg = AppConfig::load()?;
    let state = Arc::new(ZiorState {
        config: Arc::new(cfg.clone()),
        internal_api_key: config::internal_api_key()?,
    });

    let app = api::router(state);
    let addr = format!("{}:{}", cfg.server.host, cfg.server.port);
    info!("Zior engine listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
