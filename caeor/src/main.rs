mod handler;
mod observ;

use axum::{
    routing::{delete, get, post},
    Router,
};
use tracing::info;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("caeor")?;

    let media_dir = std::env::var("MEDIA_DIR").unwrap_or_else(|_| "/data/media".into());
    let port = std::env::var("PORT").unwrap_or_else(|_| "8086".into());

    std::fs::create_dir_all(format!("{media_dir}/avatars"))?;
    std::fs::create_dir_all(format!("{media_dir}/headers"))?;
    std::fs::create_dir_all(format!("{media_dir}/posts"))?;
    info!(media_dir = %media_dir, "filesystem ready");

    let state = handler::AppState { media_dir };

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(handler::health))
        .route("/v1/media/upload", post(handler::upload))
        .route("/v1/media", delete(handler::delete_media))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!(addr = %addr, "caeor listening");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
