mod db;
mod handlers;
mod identity;
mod models;
mod observ;
mod outbox;

use axum::{
    routing::{get, post},
    Router,
};
use tracing::{info, warn};

use handlers::AppState;
use manhattan_client::Manhattan;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("zodacare")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8090".into());
    let elohim_url =
        std::env::var("ELOHIM_VENI_URL").unwrap_or_else(|_| "http://localhost:8093".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Zodacare safety schema up to date");

    let elohim_client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()?;

    // The naming plane. Zodacare owns no identities — elohim-veni does — so
    // every person a report, risk profile or ban names is resolved here rather
    // than joined out of another brain's database.
    let manhattan = Manhattan::from_env("zodacare")?;
    if manhattan.configured() {
        info!("Manhattan naming plane configured");
    } else {
        // Not a soft failure. Registrations still queue durably in the outbox,
        // but any lookup by handle is refused until the plane is reachable,
        // because guessing which identity a handle points at is exactly the
        // failure this replaces.
        warn!("MANHATTAN_URL is not set: handle resolution will be refused");
    }

    // Durable handoff: triggers enqueue registrations inside the same
    // transaction as the row that caused them; this loop delivers them in
    // order and never drops one.
    outbox::spawn(database_url.clone(), manhattan.clone());

    let state = AppState {
        pool,
        elohim_client,
        elohim_url,
        manhattan,
    };

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        // Health + stats
        .route("/health", get(handlers::health))
        .route("/v1/stats", get(handlers::stats))
        // Content reports
        .route(
            "/v1/reports",
            post(handlers::create_report).get(handlers::list_reports),
        )
        .route("/v1/reports/:id", get(handlers::get_report))
        .route("/v1/reports/:id/resolve", post(handlers::resolve_report))
        .route("/v1/reports/:id/escalate", post(handlers::escalate_report))
        // User risk + moderation
        // :ident is a NAME — a PIAL uuid, pial:<uuid>, @handle or handle:<h> —
        // resolved through Manhattan, never a stored handle copy.
        .route("/v1/user/:ident/risk", get(handlers::get_user_risk))
        .route("/v1/user/:ident/action", post(handlers::apply_user_action))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Zodacare safety brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
