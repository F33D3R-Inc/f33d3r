mod db;
mod handlers;
mod identity;
mod manhattan_outbox;
mod models;
mod observ;

use axum::{
    routing::{get, post},
    Router,
};
use tracing::info;

use handlers::AppState;
use manhattan_client::Manhattan;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("lore")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8106".into());
    let herald_url = std::env::var("HERALD_URL").unwrap_or_else(|_| "http://herald:8105".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Lore achievement schema up to date");

    let herald_client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()?;

    // Lore owns XP and achievements. It owns no identity: elohim-veni does, and
    // every progression row here references a name this client resolved.
    let manhattan = Manhattan::from_env("lore")?;
    info!(
        configured = manhattan.configured(),
        "Manhattan naming plane"
    );

    // The outbox is written by triggers inside the transaction that awarded the
    // achievement; this drain is the half that delivers it. Registrations queued
    // while Manhattan is unreachable are delivered whole once it is.
    manhattan_outbox::spawn(database_url.clone(), manhattan.clone());

    let state = AppState {
        pool,
        herald_url,
        herald_client,
        manhattan,
    };

    // The identity path segment is a NAME, not a key: pial:<uuid>,
    // shard:<64-hex>, handle:<handle>, or a bare PIAL uuid.
    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(handlers::health))
        .route("/v1/achievements", get(handlers::list_achievements))
        .route(
            "/v1/users/:identity/achievements",
            get(handlers::get_user_achievements),
        )
        .route(
            "/v1/users/:identity/achievements/:ach_id/award",
            post(handlers::award_achievement),
        )
        .route("/v1/users/:identity/xp", get(handlers::get_xp))
        .route("/v1/events", post(handlers::receive_event))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Lore achievement brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
