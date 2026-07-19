mod db;
mod handlers;
mod models;
mod observ;

use axum::{
    routing::{delete, get, patch, post},
    Router,
};
use sqlx::PgPool;
use std::sync::Arc;

pub struct AppState {
    pub pool: PgPool,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("alexandria")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL required");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8098".to_string());

    let pool = PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;

    let state = Arc::new(AppState { pool });

    let app = Router::new()
        .route("/health",  get(handlers::health))
        .route("/metrics", get(observ::metrics_handler))
        // Authors
        .route("/v1/authors",          post(handlers::bootstrap_author))
        .route("/v1/authors/:pial_id", get(handlers::get_author))
        .route("/v1/authors/:pial_id", patch(handlers::update_author))
        // Titles
        .route("/v1/titles",     post(handlers::create_title))
        .route("/v1/titles",     get(handlers::list_titles))
        .route("/v1/titles/:id", get(handlers::get_title))
        .route("/v1/titles/:id", patch(handlers::update_title))
        .route("/v1/titles/:id", delete(handlers::tombstone_title))
        // Holdings
        .route("/v1/titles/:id/holdings", post(handlers::add_holding))
        // Signals
        .route("/v1/titles/:id/signals", post(handlers::increment_signals))
        // Catalog (read-only)
        .route("/v1/sections", get(handlers::list_sections))
        .route("/v1/genres",   get(handlers::list_genres))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    tracing::info!("alexandria listening on 0.0.0.0:{}", port);
    let listener = tokio::net::TcpListener::bind(format!("0.0.0.0:{}", port)).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
