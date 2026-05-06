mod handler;
mod storage;
mod transcode;
mod video;

use std::sync::Arc;

use axum::{
    extract::DefaultBodyLimit,
    routing::{get, post},
    Router,
};
use tracing::info;
use tracing_subscriber::{layer::SubscriberExt, util::SubscriberInitExt, EnvFilter};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    // JSON logs by default; console in DEV_MODE for readability.
    let env = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let dev = std::env::var("DEV_MODE").map(|v| v == "true").unwrap_or(false);
    if dev {
        tracing_subscriber::registry()
            .with(env)
            .with(tracing_subscriber::fmt::layer())
            .init();
    } else {
        tracing_subscriber::registry()
            .with(env)
            .with(tracing_subscriber::fmt::layer().json().flatten_event(true))
            .init();
    }

    let media_dir = std::env::var("MEDIA_DIR").unwrap_or_else(|_| "/data/media".into());
    let port = std::env::var("PORT").unwrap_or_else(|_| "8086".into());

    std::fs::create_dir_all(format!("{media_dir}/avatars"))?;
    std::fs::create_dir_all(format!("{media_dir}/headers"))?;
    std::fs::create_dir_all(format!("{media_dir}/posts"))?;
    info!(media_dir = %media_dir, "filesystem ready");

    // S3-compatible storage (MinIO at T0–T2).
    let storage = Arc::new(storage::Storage::from_env().await?);
    info!(
        bucket_raw = %storage.bucket_raw,
        bucket_derived = %storage.bucket_derived,
        "object storage connected"
    );

    let img_state = handler::AppState { media_dir };
    let video_state = video::VideoState {
        storage: storage.clone(),
    };

    // Image routes (legacy): keep on its own state.
    let image_routes = Router::new()
        .route("/v1/media/upload", post(handler::upload))
        .with_state(img_state);

    // Video routes: new in v1.1.
    let video_routes = Router::new()
        .route("/v1/video/upload", post(video::upload))
        .route("/v1/video/job/:job_id", get(video::job_status))
        // 10 GB upload ceiling — see video.rs MAX_UPLOAD_BYTES
        .layer(DefaultBodyLimit::max(10 * 1024 * 1024 * 1024))
        .with_state(video_state);

    let app = Router::new()
        .route("/health", get(handler::health))
        .merge(image_routes)
        .merge(video_routes)
        .layer(tower_http::cors::CorsLayer::permissive());

    let addr = format!("0.0.0.0:{}", port);
    info!(addr = %addr, "caeor listening");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
