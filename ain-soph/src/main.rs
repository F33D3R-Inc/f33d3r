mod auth;
mod db;
mod error;
mod handlers;
mod identity;
mod manhattan_outbox;
mod models;
mod nexus_aml;
mod observ;

use axum::{
    routing::{get, post},
    Router,
};
use sqlx::PgPool;
use tracing::info;

use auth::InternalApiKey;
use handlers::AppState;
use manhattan_client::Manhattan;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("ain_soph")?;

    let database_url =
        std::env::var("DATABASE_URL").expect("DATABASE_URL must be set for Ain Soph");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8089".into());

    // Ain Soph moves money. A brain that cannot tell a peer from a stranger must
    // not start and then quietly accept both — an empty key would make every
    // caller a trusted service. feed-engine refuses to boot on the same
    // condition; this is that posture, on the brain that holds the ledger.
    let internal_api_key = InternalApiKey::from_env();
    if internal_api_key.is_empty() {
        anyhow::bail!(
            "INTERNAL_API_KEY must be set for Ain Soph: it authenticates the \
             service-to-service lane on every value-moving route"
        );
    }

    let pool = PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Ain Soph (ETHRA/AET) schema up to date");

    // The naming plane. A wallet is attributed to an identity Manhattan
    // resolved, never to a handle or to another brain's row id — see identity.rs.
    let manhattan = Manhattan::from_env("ain-soph")?;
    info!(
        configured = manhattan.configured(),
        "Manhattan naming plane"
    );

    // Registrations are queued transactionally by trigger; this delivers them.
    manhattan_outbox::start_drain(database_url.clone(), manhattan.clone());

    let state = AppState {
        pool,
        manhattan,
        internal_api_key,
    };

    // Auth posture, route by route.
    //
    // Every POST below moves value or casts stake, and every one of them now
    // takes the `auth::Caller` extractor: no `X-Internal-Key` and no
    // `X-Pial-Identity` means 401 before the handler body runs, and the identity
    // acted upon is derived from that caller rather than from the request body.
    // The layer beneath is still observability only — authentication is an
    // extractor per route rather than a blanket middleware precisely so that
    // adding a route cannot silently inherit "no auth".
    //
    // The GET routes are reads and are unchanged. They remain internal-only by
    // deployment: prod publishes no port for this brain and local now binds to
    // the loopback address rather than 0.0.0.0.
    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        // Health
        .route("/health", get(handlers::health))
        // Legacy routes (backward compat)
        .route("/balance/:user_id", get(handlers::get_balance))
        .route("/deposit", post(handlers::deposit))
        .route("/transfer", post(handlers::transfer))
        .route("/withdraw", post(handlers::withdraw))
        // AET v1 API
        .route("/v1/supply", get(handlers::get_supply))
        .route("/v1/balance/:user_id", get(handlers::get_balance))
        .route(
            "/v1/transactions/:user_id",
            get(handlers::list_transactions),
        )
        .route("/v1/deposit", post(handlers::deposit))
        .route("/v1/transfer", post(handlers::transfer))
        .route("/v1/tip", post(handlers::tip))
        .route("/v1/withdraw", post(handlers::withdraw))
        // Ledger cross-brain checkpoint (called by Aethyr Ledger after each block seal)
        .route("/v1/ledger/checkpoint", post(handlers::ledger_checkpoint))
        // Governance
        .route(
            "/v1/proposals",
            get(handlers::list_proposals).post(handlers::create_proposal),
        )
        .route("/v1/proposals/:id", get(handlers::get_proposal))
        .route("/v1/proposals/:id/vote", post(handlers::cast_vote))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Ain Soph listening on {} — ETHRA/AET network active", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
