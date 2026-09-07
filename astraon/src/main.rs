mod db;
mod error;
mod handlers;
mod models;
mod observ;

use axum::{routing::get, Router};
use sqlx::PgPool;
use tracing::{info, warn};

use manhattan_client::Manhattan;

/// Everything a request needs: the facts pool, and the naming plane.
///
/// These are two different kinds of thing on purpose. The pool answers "what
/// happened"; Manhattan answers "who is this". Astraon owns neither identity nor
/// content — it only measures — so the identity half of every request crosses a
/// process boundary instead of becoming a join.
#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    pub manhattan: Manhattan,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("astraon")?;

    // COUPLING, DECLARED: this pool still points at f33d3r_feed — feed-engine's
    // own database. Astraon does not merely reference feed-engine's entities by
    // name here, it reads feed-engine's tables, which is the one thing the
    // naming plane exists to end. It is written down rather than hidden so the
    // next change to feed-engine's schema is a known risk instead of a silent
    // wrong number. See the decoupling note at the head of db.rs.
    let database_url = std::env::var("ANALYTICS_DATABASE_URL")
        .or_else(|_| std::env::var("DATABASE_URL"))
        .expect("DATABASE_URL or ANALYTICS_DATABASE_URL must be set");

    let port = std::env::var("PORT").unwrap_or_else(|_| "8088".into());

    let pool = PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Astraon analytics schema up to date");

    let manhattan = Manhattan::from_env("astraon")?;
    if manhattan.configured() {
        info!("Manhattan naming plane configured; identities resolve by name");
    } else {
        // Loud, never silent: without MANHATTAN_URL every identity arrives
        // unverified and handle-addressed requests cannot be served at all.
        warn!("MANHATTAN_URL is not set — identity names cannot be resolved");
    }

    let state = AppState { pool, manhattan };

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(handlers::health))
        // Creator analytics. The path segment is a NAME — a PIAL uuid or a
        // handle — resolved through Manhattan, not a row id looked up locally.
        .route(
            "/v1/analytics/creator/:identity/summary",
            get(handlers::creator_summary),
        )
        .route(
            "/v1/analytics/creator/:identity/posts",
            get(handlers::creator_posts),
        )
        .route(
            "/v1/analytics/creator/:identity/timeline",
            get(handlers::creator_timeline),
        )
        .route(
            "/v1/analytics/creator/:identity/audience",
            get(handlers::creator_audience),
        )
        .route(
            "/v1/analytics/creator/:identity/breakdown",
            get(handlers::creator_breakdown),
        )
        // Single-work deep dive
        .route("/v1/analytics/post/:work_id", get(handlers::post_detail))
        // Admin / site-wide
        .route("/v1/analytics/site/summary", get(handlers::site_summary))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Astraon (creator analytics) listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
