mod handler;
mod observ;
mod transcode;

use std::sync::Arc;

use axum::{
    extract::DefaultBodyLimit,
    routing::{get, post},
    Router,
};
use tracing::info;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("transcoding")?;

    let media_dir = std::env::var("MEDIA_DIR").unwrap_or_else(|_| "/data/media".into());
    let port = std::env::var("PORT").unwrap_or_else(|_| "8085".into());

    tokio::fs::create_dir_all(format!("{media_dir}/posts")).await?;
    info!(media_dir = %media_dir, port = %port, "transcoding brain ready");

    let state = handler::AppState {
        media_dir: Arc::new(media_dir),
    };

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(handler::health))
        .route("/v1/video/upload", post(handler::upload))
        .route("/v1/video/job/:job_id", get(handler::job_status))
        .layer(DefaultBodyLimit::max(10 * 1024 * 1024 * 1024))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{port}");
    info!(addr = %addr, "transcoding brain listening");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
