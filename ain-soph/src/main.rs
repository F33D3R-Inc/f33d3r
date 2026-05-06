mod db;
mod error;
mod handlers;
mod models;

use axum::{
    routing::{get, post},
    Router,
};
use sqlx::PgPool;
use tracing::info;
use tracing_subscriber::{layer::SubscriberExt, util::SubscriberInitExt, EnvFilter};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::registry()
        .with(EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()))
        .with(tracing_subscriber::fmt::layer())
        .init();

    let database_url = std::env::var("DATABASE_URL")
        .expect("DATABASE_URL must be set for Ain Soph");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8089".into());

    let pool = PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Ain Soph (ETHRA/AET) schema up to date");

    let app = Router::new()
        // Health
        .route("/health",                    get(handlers::health))
        // Legacy routes (backward compat)
        .route("/balance/:user_id",          get(handlers::get_balance))
        .route("/deposit",                   post(handlers::deposit))
        .route("/transfer",                  post(handlers::transfer))
        .route("/withdraw",                  post(handlers::withdraw))
        // AET v1 API
        .route("/v1/supply",                 get(handlers::get_supply))
        .route("/v1/balance/:user_id",       get(handlers::get_balance))
        .route("/v1/transactions/:user_id",  get(handlers::list_transactions))
        .route("/v1/deposit",                post(handlers::deposit))
        .route("/v1/transfer",               post(handlers::transfer))
        .route("/v1/tip",                    post(handlers::tip))
        .route("/v1/withdraw",               post(handlers::withdraw))
        // Governance
        .route("/v1/proposals",              get(handlers::list_proposals)
                                             .post(handlers::create_proposal))
        .route("/v1/proposals/:id",          get(handlers::get_proposal))
        .route("/v1/proposals/:id/vote",     post(handlers::cast_vote))
        .with_state(pool);

    let addr = format!("0.0.0.0:{}", port);
    info!("Ain Soph listening on {} — ETHRA/AET network active", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
