mod db;
mod decision;
mod handlers;
mod models;

use axum::{
    routing::{get, post},
    Router,
};
use tracing::info;
use tracing_subscriber::{layer::SubscriberExt, util::SubscriberInitExt, EnvFilter};

use handlers::AppState;
#[allow(unused_imports)]
use reqwest;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::registry()
        .with(EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()))
        .with(tracing_subscriber::fmt::layer())
        .init();

    let database_url = std::env::var("DATABASE_URL")
        .expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8093".into());

    let verity_url = std::env::var("VERITY_URL")
        .unwrap_or_else(|_| "http://localhost:8095".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Elohim Veni schema up to date");

    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(3))
        .build()?;

    let state = AppState { pool, verity_url, http };

    let app = Router::new()
        // Health
        .route("/health",                             get(handlers::health))
        // Stats
        .route("/v1/stats",                           get(handlers::stats))
        // PIAL lifecycle
        .route("/v1/pial/bootstrap",                  post(handlers::pial_bootstrap))
        .route("/v1/pial/capabilities/bulk",          post(handlers::bulk_update_capabilities))
        .route("/v1/pial/:pial_id/capabilities",      get(handlers::get_capabilities)
                                                          .post(handlers::update_capability))
        .route("/v1/pial/:pial_id/trust",             get(handlers::get_trust))
        .route("/v1/pial/:pial_id/status",            post(handlers::update_pial_status))
        .route("/v1/pial/:pial_id/ban",               post(handlers::ban_pial))
        // Decision engine
        .route("/v1/decisions",                       post(handlers::make_decision))
        .route("/v1/decisions/log",                   get(handlers::decision_log))
        .layer(tower_http::cors::CorsLayer::permissive())
        .with_state(state);

    let addr = format!("0.0.0.0:{}", port);
    info!("Elohim Veni security brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
