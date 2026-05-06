mod db;
mod handlers;
mod models;
mod clients;

use axum::{
    routing::{delete, get, post, put},
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

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL required");
    let port        = std::env::var("PORT").unwrap_or_else(|_| "8084".into());
    let ain_soph    = std::env::var("AIN_SOPH_URL").unwrap_or_else(|_| "http://localhost:8089".into());
    let verity      = std::env::var("VERITY_URL").unwrap_or_else(|_| "http://localhost:8095".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Thessalon commerce schema up to date");

    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(10))
        .build()?;

    let state = AppState { pool, http, ain_soph_url: ain_soph, verity_url: verity };

    let app = Router::new()
        .route("/health",                                    get(handlers::health))
        .route("/v1/stats",                                  get(handlers::stats))
        // Creator onboarding
        .route("/v1/creator/enable",                         post(handlers::creator_enable))
        .route("/v1/creator/:pial_id/status",                get(handlers::creator_status))
        // Subscription plans
        .route("/v1/creator/:pial_id/plans",                 get(handlers::list_plans))
        .route("/v1/plans",                                  post(handlers::create_plan))
        .route("/v1/plans/:id",                              put(handlers::update_plan).delete(handlers::delete_plan))
        // Subscriptions
        .route("/v1/subscribe",                              post(handlers::subscribe))
        .route("/v1/subscribe",                              delete(handlers::unsubscribe))
        .route("/v1/access/subscription",                    get(handlers::check_subscription))
        .route("/v1/creator/:pial_id/subscribers",           get(handlers::list_subscribers))
        .route("/v1/subscriber/:pial_id/subscriptions",      get(handlers::list_subscriptions))
        // PPV
        .route("/v1/ppv",                                    post(handlers::create_ppv))
        .route("/v1/ppv/:id/purchase",                       post(handlers::purchase_ppv))
        .route("/v1/access/ppv",                             get(handlers::check_ppv_access))
        .route("/v1/creator/:pial_id/ppv",                   get(handlers::list_ppv_items))
        // Tips
        .route("/v1/tips",                                   post(handlers::send_tip))
        // Earnings
        .route("/v1/creator/:pial_id/earnings",              get(handlers::earnings))
        .layer(tower_http::cors::CorsLayer::permissive())
        .with_state(state);

    let addr = format!("0.0.0.0:{port}");
    info!("Thessalon commerce brain listening on {addr}");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
