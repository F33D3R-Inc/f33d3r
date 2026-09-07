mod db;
mod directory;
mod handlers;
mod models;
mod observ;
mod outbox;

use axum::{
    routing::{delete, get, patch, post},
    Router,
};
use sqlx::PgPool;
use std::sync::Arc;

use manhattan_client::Manhattan;

pub struct AppState {
    pub pool: PgPool,
    /// The naming plane. alexandria references identities and works by NAME and
    /// resolves them here; it holds no copy of another brain's rows.
    pub manhattan: Manhattan,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("alexandria")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL required");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8098".to_string());

    let pool = PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;

    let manhattan = Manhattan::from_env("alexandria")?;
    if !manhattan.configured() {
        tracing::error!(
            "MANHATTAN_URL is not set — registrations will queue in manhattan_outbox and handles will not resolve"
        );
    }

    // The outbox drain. Registrations are already durable at this point: the
    // triggers wrote them in the same transaction as the rows that caused them.
    // This delivers them, in order, retrying until Manhattan accepts each one.
    tokio::spawn(outbox::start_drain(database_url.clone(), manhattan.clone()));

    let state = Arc::new(AppState { pool, manhattan });

    let app = Router::new()
        .route("/health", get(handlers::health))
        .route("/metrics", get(observ::metrics_handler))
        // Authors
        .route("/v1/authors", post(handlers::bootstrap_author))
        .route("/v1/authors/:pial_id", get(handlers::get_author))
        .route("/v1/authors/:pial_id", patch(handlers::update_author))
        // Titles
        .route("/v1/titles", post(handlers::create_title))
        .route("/v1/titles", get(handlers::list_titles))
        .route("/v1/titles/:id", get(handlers::get_title))
        .route("/v1/titles/:id", patch(handlers::update_title))
        .route("/v1/titles/:id", delete(handlers::tombstone_title))
        // Holdings
        .route("/v1/titles/:id/holdings", post(handlers::add_holding))
        // Signals
        .route("/v1/titles/:id/signals", post(handlers::increment_signals))
        // Catalog (read-only)
        .route("/v1/sections", get(handlers::list_sections))
        .route("/v1/genres", get(handlers::list_genres))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    tracing::info!("alexandria listening on 0.0.0.0:{}", port);
    let listener = tokio::net::TcpListener::bind(format!("0.0.0.0:{}", port)).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
