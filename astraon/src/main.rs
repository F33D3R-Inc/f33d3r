mod db;
mod error;
mod handlers;
mod models;
mod observ;

use axum::{
    routing::get,
    Router,
};
use sqlx::PgPool;
use tracing::info;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("astraon")?;

    // Astraon reads from f33d3r_feed (same DB as Nantar) until the
    // f33d3r_analytics DB is provisioned. ANALYTICS_DATABASE_URL overrides.
    let database_url = std::env::var("ANALYTICS_DATABASE_URL")
        .or_else(|_| std::env::var("DATABASE_URL"))
        .expect("DATABASE_URL or ANALYTICS_DATABASE_URL must be set");

    let port = std::env::var("PORT").unwrap_or_else(|_| "8088".into());

    let pool = PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Astraon analytics schema up to date");

    let app = Router::new()
        .route("/metrics",                                  get(observ::metrics_handler))
        .route("/health",                                   get(handlers::health))
        // Creator analytics
        .route("/v1/analytics/creator/:pial_id/summary",   get(handlers::creator_summary))
        .route("/v1/analytics/creator/:pial_id/posts",     get(handlers::creator_posts))
        .route("/v1/analytics/creator/:pial_id/timeline",  get(handlers::creator_timeline))
        .route("/v1/analytics/creator/:pial_id/audience",  get(handlers::creator_audience))
        .route("/v1/analytics/creator/:pial_id/breakdown", get(handlers::creator_breakdown))
        // Single-post deep dive
        .route("/v1/analytics/post/:post_id",              get(handlers::post_detail))
        // Admin / site-wide
        .route("/v1/analytics/site/summary",               get(handlers::site_summary))
        .with_state(pool)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Astraon (creator analytics) listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
