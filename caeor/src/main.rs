//! Caeor — the media-asset authority.
//!
//!   caeor           serve the API, run the job and integrity reconcilers and the Manhattan drain
//!   caeor serve     the same
//!   caeor import    one-shot import of feed-engine's media registry (import.rs)
//!   caeor remark    watermark imported media in place (remark.rs)

mod api;
mod assets;
mod auth;
mod config;
mod db;
mod ids;
mod imaging;
mod import;
mod ingest;
mod integrity;
mod jobs;
mod layout;
mod mark;
mod migrate;
mod observ;
mod outbox;
mod remark;
mod withdraw;

use std::sync::Arc;
use std::time::Duration;

use anyhow::{bail, Context};
use axum::extract::DefaultBodyLimit;
use axum::routing::{get, post};
use axum::Router;
use manhattan_client::Manhattan;
use tokio::sync::{Notify, Semaphore};
use tracing::info;

/// Image mark passes and forensic encodes allowed at once.
const ENCODE_SLOTS: usize = 4;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("caeor")?;
    let cfg = config::Config::from_env()?;
    let scratch = cfg.layout.scratch_dir();
    if !scratch
        .to_string_lossy()
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '/' | '-' | '_' | '.'))
    {
        bail!(
            "PRIVATE_DIR must be a plain path ([A-Za-z0-9/._-]); scratch files are named inside ffmpeg filters"
        );
    }
    cfg.layout.prepare()?;

    let command = std::env::args().nth(1).unwrap_or_else(|| "serve".into());
    let pool = db::connect(&cfg.database_url, 16).await?;
    migrate::run(&pool).await?;

    match command.as_str() {
        "serve" => serve(cfg, pool).await,
        "import" => import::run(&pool, &cfg.layout).await,
        "remark" => remark::run(&pool, &cfg.layout).await,
        other => bail!("unknown command {other:?}; expected serve, import or remark"),
    }
}

async fn serve(cfg: config::Config, pool: sqlx::PgPool) -> anyhow::Result<()> {
    let manhattan = Manhattan::from_env("caeor")?;
    outbox::spawn(cfg.database_url.clone(), manhattan);

    let http = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(30))
        .build()
        .context("building the transcoding client")?;
    let wake = Arc::new(Notify::new());
    jobs::Reconciler {
        pool: pool.clone(),
        layout: cfg.layout.clone(),
        http,
        transcoding_url: cfg.transcoding_url.clone(),
        service_key: cfg.service_key.clone(),
        wake: wake.clone(),
    }
    .spawn();

    let abraxas = integrity::Abraxas::new(&cfg.abraxas_url, &cfg.service_key)?;
    let integrity_wake = Arc::new(Notify::new());
    integrity::Reconciler {
        pool: pool.clone(),
        layout: cfg.layout.clone(),
        abraxas: abraxas.clone(),
        wake: integrity_wake.clone(),
    }
    .spawn();

    let port = cfg.port.clone();
    let state = api::AppState {
        pool,
        layout: cfg.layout.clone(),
        cfg: Arc::new(cfg),
        jobs_wake: wake,
        encode_slots: Arc::new(Semaphore::new(ENCODE_SLOTS)),
        abraxas,
        integrity_wake,
    };

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(api::health))
        .route(
            "/v1/assets",
            post(api::upload).layer(DefaultBodyLimit::disable()),
        )
        .route("/v1/assets/clip", post(api::clip))
        .route("/v1/assets/resolve", get(api::resolve))
        .route("/v1/assets/:id", get(api::get_asset).delete(api::withdraw_delete))
        .route("/v1/assets/:id/withdraw", post(api::withdraw_post))
        .route("/v1/assets/:id/recheck", post(api::recheck))
        .route("/v1/assets/:id/download", get(api::download))
        .layer(axum::middleware::from_fn_with_state(state.clone(), auth::require_service_key))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{port}");
    info!(addr = %addr, "caeor listening");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
