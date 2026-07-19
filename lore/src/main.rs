mod db;
mod handlers;
mod models;
mod observ;

use axum::{
    routing::{get, post},
    Router,
};
use tracing::info;

use handlers::AppState;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("lore")?;

    let database_url = std::env::var("DATABASE_URL")
        .expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8106".into());
    let herald_url = std::env::var("HERALD_URL")
        .unwrap_or_else(|_| "http://herald:8105".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Lore achievement schema up to date");

    let herald_client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()?;

    let state = AppState { pool, herald_url, herald_client };

    let app = Router::new()
        .route("/metrics",                                          get(observ::metrics_handler))
        .route("/health",                                           get(handlers::health))
        .route("/v1/achievements",                                  get(handlers::list_achievements))
        .route("/v1/users/:pial_shard_id/achievements",            get(handlers::get_user_achievements))
        .route("/v1/users/:pial_shard_id/achievements/:ach_id/award", post(handlers::award_achievement))
        .route("/v1/users/:pial_shard_id/xp",                      get(handlers::get_xp))
        .route("/v1/events",                                        post(handlers::receive_event))
        .layer(tower_http::cors::CorsLayer::permissive())
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Lore achievement brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
