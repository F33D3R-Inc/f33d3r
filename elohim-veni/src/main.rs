mod db;
mod decision;
mod handlers;
mod models;
mod observ;

use axum::{
    routing::{get, post},
    Router,
};
use tracing::info;

use handlers::AppState;
#[allow(unused_imports)]
use reqwest;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("elohim_veni")?;

    let database_url = std::env::var("DATABASE_URL")
        .expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8093".into());

    let verity_url = std::env::var("VERITY_URL")
        .unwrap_or_else(|_| "http://localhost:8095".into());

    let internal_api_key = std::env::var("INTERNAL_API_KEY")
        .unwrap_or_default();

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Elohim Veni schema up to date");

    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(3))
        .build()?;

    let nexus_tier_cache = std::sync::Arc::new(
        std::sync::Mutex::new(std::collections::HashMap::<String, (i16, std::time::Instant)>::new())
    );

    let state = AppState { pool, verity_url, http, internal_api_key, nexus_tier_cache };

    let app = Router::new()
        .route("/metrics",                            get(observ::metrics_handler))
        // Health
        .route("/health",                             get(handlers::health))
        // Stats
        .route("/v1/stats",                           get(handlers::stats))
        // PIAL lifecycle
        .route("/v1/pial/bootstrap",                  post(handlers::pial_bootstrap))
        .route("/v1/pial/capabilities/bulk",          post(handlers::bulk_update_capabilities))
        // PIAL key management — literal-segment routes BEFORE parameterised :pial_id routes
        .route("/v1/pial/signing-key/register",       post(handlers::register_signing_key))
        .route("/v1/pial/ecdh-key/register",          post(handlers::register_ecdh_key))
        .route("/v1/pial/:pial_id/signing-pubkey",    get(handlers::get_signing_pubkey))
        .route("/v1/pial/:pial_id/ecdh-pubkey",       get(handlers::get_ecdh_pubkey))
        .route("/v1/pial/:pial_id/capabilities",      get(handlers::get_capabilities)
                                                          .post(handlers::update_capability))
        .route("/v1/pial/:pial_id/trust",             get(handlers::get_trust))
        .route("/v1/pial/:pial_id/status",            post(handlers::update_pial_status))
        .route("/v1/pial/:pial_id/ban",               post(handlers::ban_pial))
        // Decision engine
        .route("/v1/decisions",                       post(handlers::make_decision))
        .route("/v1/decisions/log",                   get(handlers::decision_log))
        // NEXUS tier proxy — routes to Verity; Nantar calls this instead of Verity directly
        .route("/v1/nexus/tier/:shard",               get(handlers::nexus_tier))
        .layer(tower_http::cors::CorsLayer::permissive())
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Elohim Veni security brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
