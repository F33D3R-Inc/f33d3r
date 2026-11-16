mod notifications;
mod auth;
mod db;
mod error;
mod handlers;
mod identity;
mod manhattan_outbox;
mod models;
mod observ;
mod ranking_outbox;

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

    // Payment signals for AethyrRank are rows written with the payment and
    // relayed to ranking.events through the Sitra Achra proxy. Without the
    // proxy they would pile up undelivered, so its address is required.
    let sitra_proxy = std::env::var("SITRA_PROXY_URL").unwrap_or_default();
    if sitra_proxy.trim().is_empty() {
        anyhow::bail!("SITRA_PROXY_URL must be set for Ain Soph: payment signals are relayed through it");
    }
    ranking_outbox::spawn_relay(pool.clone(), sitra_proxy);

    // Registrations are queued transactionally by trigger; this delivers them.
    manhattan_outbox::start_drain(database_url.clone(), manhattan.clone());
    notification_outbox::spawn(database_url.clone());

    let state = AppState {
        pool,
        manhattan,
        internal_api_key,
    };

    // Auth posture. Every route but /health and /metrics requires the internal
    // service key (require_internal_key), reads included: Ain Soph is a private
    // book that only Themis and feed-engine reach. Every handler that moves value,
    // casts stake or reads an account also takes the `auth::Caller` extractor,
    // which checks the key again and honours X-Pial-Identity only alongside it,
    // so adding a route cannot silently inherit "no auth".
    //
    // There is no withdrawal and no legacy unversioned route: AET is
    // non-redeemable platform credit, and the only mint is a service deposit.
    let key_layer = state.internal_api_key.clone();
    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(handlers::health))
        .route("/v1/unit", get(handlers::get_unit))
        .route("/v1/supply", get(handlers::get_supply))
        .route("/v1/balance/:user_id", get(handlers::get_balance))
        .route(
            "/v1/transactions/:user_id",
            get(handlers::list_transactions),
        )
        .route("/v1/deposit", post(handlers::deposit))
        .route("/v1/transfer", post(handlers::transfer))
        .route("/v1/tip", post(handlers::tip))
        .route("/v1/accounts/reattribute", post(handlers::reattribute))
        // Governance
        .route(
            "/v1/proposals",
            get(handlers::list_proposals).post(handlers::create_proposal),
        )
        .route("/v1/proposals/:id", get(handlers::get_proposal))
        .route("/v1/proposals/:id/vote", post(handlers::cast_vote))
        .with_state(state)
        .layer(axum::middleware::from_fn_with_state(key_layer, auth::require_internal_key))
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Ain Soph listening on {} — ETHRA/AET network active", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
