mod db;
mod handlers;
mod models;
mod observ;

use axum::{
    routing::{delete, get, patch, post},
    Router,
};
use tracing::info;

use handlers::AppState;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("herald")?;

    let database_url      = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port              = std::env::var("PORT").unwrap_or_else(|_| "8105".into());
    let vapid_pub_key     = std::env::var("VAPID_PUBLIC_KEY").unwrap_or_else(|_| "not-configured".into());
    let vapid_private_key = std::env::var("VAPID_PRIVATE_KEY").unwrap_or_default();
    let vapid_subject     = std::env::var("VAPID_SUBJECT").unwrap_or_else(|_| "mailto:admin@f33d3r.com".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Herald push notification schema up to date");

    let state = AppState { pool, vapid_pub_key, vapid_private_key, vapid_subject };

    let app = Router::new()
        .route("/metrics",                              get(observ::metrics_handler))
        .route("/health",                               get(handlers::health))
        .route("/v1/stats",                             get(handlers::stats))
        .route("/v1/vapid-public-key",                  get(handlers::vapid_public_key))
        .route("/v1/subscriptions",                     post(handlers::subscribe))
        .route("/v1/subscriptions/:device_id",          delete(handlers::unsubscribe))
        .route("/v1/notify",                            post(handlers::notify))
        .route("/v1/preferences/:pial_shard_id",        get(handlers::get_preferences))
        .route("/v1/preferences/:pial_shard_id",        patch(handlers::update_preferences))
        .layer(tower_http::cors::CorsLayer::permissive())
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Herald push notification brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
