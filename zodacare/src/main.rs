mod db;
mod handlers;
mod models;

use axum::{
    routing::{get, post},
    Router,
};
use tracing::info;
use tracing_subscriber::{layer::SubscriberExt, util::SubscriberInitExt, EnvFilter};

use handlers::AppState;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::registry()
        .with(EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()))
        .with(tracing_subscriber::fmt::layer())
        .init();

    let database_url = std::env::var("DATABASE_URL")
        .expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8090".into());
    let elohim_url = std::env::var("ELOHIM_VENI_URL")
        .unwrap_or_else(|_| "http://localhost:8093".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Zodacare safety schema up to date");

    let elohim_client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()?;

    let state = AppState { pool, elohim_client, elohim_url };

    let app = Router::new()
        // Health + stats
        .route("/health",                          get(handlers::health))
        .route("/v1/stats",                        get(handlers::stats))
        // Content reports
        .route("/v1/reports",                      post(handlers::create_report)
                                                       .get(handlers::list_reports))
        .route("/v1/reports/:id",                  get(handlers::get_report))
        .route("/v1/reports/:id/resolve",          post(handlers::resolve_report))
        .route("/v1/reports/:id/escalate",         post(handlers::escalate_report))
        // User risk + moderation
        .route("/v1/user/:pial_id/risk",           get(handlers::get_user_risk))
        .route("/v1/user/:pial_id/action",         post(handlers::apply_user_action))
        .layer(tower_http::cors::CorsLayer::permissive())
        .with_state(state);

    let addr = format!("0.0.0.0:{}", port);
    info!("Zodacare safety brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
