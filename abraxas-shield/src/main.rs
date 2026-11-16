//! Abraxas Shield — the integrity authority.
//!
//!   abraxas-shield            serve: the HTTP gate and admin API, the
//!   abraxas-shield serve      content.events intake, the job runners, the
//!                             safety.events relay, the elohim-veni ban drain
//!                             and the Manhattan outbox drain
//!   abraxas-shield import     one-shot import of zodacare's reports and
//!                             feed-engine's integrity tables (import.rs)
//!   abraxas-shield backfill   put caeor's pre-gate media through the gate
//!                             (backfill.rs)
//!
//! Abraxas owns f33d3r_integrity and writes no other database. content-scan is
//! its private worker: it computes, Abraxas decides.

mod admin;
mod align;
mod api;
mod auth;
mod backfill;
mod banned;
mod check;
mod config;
mod db;
mod enforcement;
mod hashing;
mod import;
mod index;
mod jobs;
mod media;
mod migrate;
mod observ;
mod outbox;
mod ownership;
mod pipeline;
mod policy;
mod reports;
mod republish;
mod review;
mod scan;
mod verdicts;
mod worker;

use std::sync::Arc;

use anyhow::bail;
use manhattan_client::Manhattan;
use tokio::sync::Notify;
use tracing::info;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("abraxas-shield")?;
    let cfg = config::Config::from_env()?;
    let command = std::env::args().nth(1).unwrap_or_else(|| "serve".into());

    let pool = db::connect(&cfg.database_url, 24).await?;
    migrate::run(&pool).await?;

    if command == "import" {
        return import::run(&pool).await;
    }
    if command == "republish" {
        let refs: Vec<String> = std::env::args().skip(2).collect();
        return republish::run(&pool, &refs).await;
    }

    let worker = worker::Worker::new(&cfg.content_scan_url, &cfg.internal_key, cfg.worker_timeout)?;
    let manhattan = Manhattan::from_env("abraxas-shield")?;
    let state = api::AppState {
        pool,
        cfg: Arc::new(cfg),
        worker,
        chroma: banned::ChromaRegistry::default(),
        legacy: ownership::LegacyAudio::default(),
        suppression: review::Suppression::default(),
        jobs_wake: Arc::new(Notify::new()),
        manhattan,
    };

    match command.as_str() {
        "serve" => serve(state).await,
        "backfill" => backfill::run(&state).await,
        other => bail!("unknown command {other:?}; expected serve, import, backfill or republish"),
    }
}

async fn serve(state: api::AppState) -> anyhow::Result<()> {
    spawn_manhattan_drain(state.cfg.database_url.clone(), state.manhattan.clone());
    outbox::spawn_relay(state.pool.clone(), state.cfg.kafka_brokers.clone());
    enforcement::spawn(state.pool.clone(), state.cfg.elohim_veni_url.clone(), state.cfg.internal_key.clone());
    jobs::spawn(state.clone());
    pipeline::spawn(state.clone());

    let addr = format!("0.0.0.0:{}", state.cfg.port);
    let app = api::router(state);
    info!(addr = %addr, "abraxas-shield listening");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}

/// The shared Manhattan outbox drain (libs/manhattan-outbox): registers every
/// identity a report names. If the database is not reachable yet, keeps
/// trying, loudly, rather than serving without a drain.
fn spawn_manhattan_drain(database_url: String, manhattan: Manhattan) {
    tokio::spawn(async move {
        let mut backoff = std::time::Duration::from_secs(1);
        loop {
            match manhattan_outbox::connect("abraxas-shield", &database_url).await {
                Ok(outbox) => {
                    outbox.spawn(manhattan);
                    return;
                }
                Err(e) => {
                    tracing::error!(error = %e, "manhattan outbox drain cannot open its database pool; retrying");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(std::time::Duration::from_secs(30));
                }
            }
        }
    });
}
