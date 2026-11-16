mod handler;
mod job;
mod observ;
mod transcode;

use std::path::PathBuf;
use std::sync::Arc;

use anyhow::bail;
use axum::{routing::{get, put}, Router};
use dashmap::DashMap;
use tokio::sync::Semaphore;
use tracing::info;

fn env_or(name: &str, default: &str) -> String {
    std::env::var(name).ok().filter(|v| !v.trim().is_empty()).unwrap_or_else(|| default.to_string())
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("transcoding")?;

    let roots = job::Roots {
        media_dir: PathBuf::from(env_or("MEDIA_DIR", "/data/media")),
        private_dir: PathBuf::from(env_or("PRIVATE_DIR", "/data/media-private")),
        replay_dir: PathBuf::from(env_or("REPLAY_DIR", "/replays")),
    };
    if roots.private_dir.starts_with(&roots.media_dir) || roots.media_dir.starts_with(&roots.private_dir) {
        bail!("PRIVATE_DIR and MEDIA_DIR must not contain one another: a private source inside the served store is a served source");
    }
    if !roots
        .scratch()
        .to_string_lossy()
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '/' | '-' | '_' | '.'))
    {
        bail!("PRIVATE_DIR must be a plain path ([A-Za-z0-9/._-]); scratch files are named inside ffmpeg filters");
    }
    if std::env::var("INTERNAL_API_KEY").map(|k| k.is_empty()).unwrap_or(true) {
        bail!("INTERNAL_API_KEY must be set: caeor is the only caller and it presents that key");
    }
    tokio::fs::create_dir_all(roots.scratch()).await?;

    let jobs_at_once: usize = env_or("JOBS_AT_ONCE", "2").parse().map_err(|e| anyhow::anyhow!("JOBS_AT_ONCE: {e}"))?;
    if jobs_at_once == 0 {
        bail!("JOBS_AT_ONCE must be at least 1");
    }
    let port = env_or("PORT", "8085");
    info!(media_dir = %roots.media_dir.display(), private_dir = %roots.private_dir.display(),
          jobs_at_once, "transcoding worker ready");

    let state = handler::AppState {
        roots: Arc::new(roots),
        jobs: Arc::new(DashMap::new()),
        slots: Arc::new(Semaphore::new(jobs_at_once)),
    };

    let jobs = state.jobs.clone();
    tokio::spawn(async move {
        let mut tick = tokio::time::interval(std::time::Duration::from_secs(3600));
        loop {
            tick.tick().await;
            handler::evict_finished_jobs(&jobs);
        }
    });

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(handler::health))
        .route(
            "/v1/jobs/:job_id",
            put(handler::put_job).get(handler::get_job).delete(handler::delete_job),
        )
        .with_state(state)
        .layer(axum::middleware::from_fn(handler::require_service_key))
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{port}");
    info!(addr = %addr, "transcoding worker listening");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
