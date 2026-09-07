mod contact;
mod db;
mod decision;
mod follow_graph;
mod handlers;
mod manhattan_outbox;
mod models;
mod number_lease;
mod observ;

use axum::{
    routing::{get, post},
    Router,
};
use tracing::info;

use handlers::AppState;
#[allow(unused_imports)]
use reqwest;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("elohim_veni")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8093".into());

    let verity_url = std::env::var("VERITY_URL").unwrap_or_else(|_| "http://localhost:8095".into());

    let internal_api_key = std::env::var("INTERNAL_API_KEY").unwrap_or_default();

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Elohim Veni schema up to date");

    // The naming plane. Elohim Veni is the identity authority — Manhattan's
    // authority map gives this brain the identity, key and address node kinds —
    // so every PIAL and every key asserted here is registered there, and every
    // other brain's PIAL column is a reference to what this one owns.
    //
    // The client is built once and shared. Nothing writes to it directly: writes
    // go through the transactional outbox, which the drain below delivers.
    let manhattan = manhattan_client::Manhattan::from_env("elohim-veni")?;
    manhattan_outbox::spawn(database_url.clone(), manhattan.clone());

    // Expired Numbers stop admitting new contact at the decision, in SQL,
    // against this database's own clock — that enforcement does not depend on
    // this task running. The sweeper only releases the NAME in the naming
    // plane, so an expired Number becomes the same answer as a retired one all
    // the way down rather than only at the policy layer.
    number_lease::spawn(pool.clone(), manhattan.clone());

    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(3))
        .build()?;

    let nexus_tier_cache = std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashMap::<
        String,
        (i16, std::time::Instant),
    >::new()));

    // Fails boot on a malformed seed; an unset seed yields an unsigned signer that
    // reports itself as unsigned on every bundle.
    let bundle_signer = contact::BundleSigner::from_env()?;
    if !bundle_signer.configured() {
        tracing::warn!("CONTACT_BUNDLE_SIGNING_SEED is unset — contact key bundles ship unsigned");
    }

    let state = AppState {
        pool,
        verity_url,
        http,
        internal_api_key,
        nexus_tier_cache,
        manhattan,
        bundle_signer,
    };

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        // Health
        .route("/health", get(handlers::health))
        // Stats
        .route("/v1/stats", get(handlers::stats))
        // PIAL lifecycle
        .route("/v1/pial/bootstrap", post(handlers::pial_bootstrap))
        .route(
            "/v1/pial/capabilities/bulk",
            post(handlers::bulk_update_capabilities),
        )
        // PIAL key management — literal-segment routes BEFORE parameterised :pial_id routes
        .route(
            "/v1/pial/signing-key/register",
            post(handlers::register_signing_key),
        )
        .route(
            "/v1/pial/ecdh-key/register",
            post(handlers::register_ecdh_key),
        )
        .route(
            "/v1/pial/:pial_id/signing-pubkey",
            get(handlers::get_signing_pubkey),
        )
        .route(
            "/v1/pial/:pial_id/ecdh-pubkey",
            get(handlers::get_ecdh_pubkey),
        )
        .route(
            "/v1/pial/:pial_id/capabilities",
            get(handlers::get_capabilities).post(handlers::update_capability),
        )
        .route("/v1/pial/:pial_id/trust", get(handlers::get_trust))
        .route(
            "/v1/pial/:pial_id/status",
            post(handlers::update_pial_status),
        )
        .route("/v1/pial/:pial_id/ban", post(handlers::ban_pial))
        // Contact addresses — this brain owns `address`/`addr` in Manhattan.
        .route("/v1/addresses/mint", post(handlers::mint_address))
        .route("/v1/addresses/revoke", post(handlers::revoke_address))
        .route("/v1/addresses/list", post(handlers::list_addresses))
        // F33D3R Numbers — allocation, rotation and per-Number policy.
        .route("/v1/numbers/mint", post(handlers::mint_number))
        .route("/v1/numbers/revoke", post(handlers::revoke_number))
        .route("/v1/numbers/policy", post(handlers::set_number_policy))
        .route("/v1/numbers/:pial_id", get(handlers::list_numbers))
        // Contact policy, capabilities, requests and key discovery.
        .route("/v1/contact/policy", post(handlers::set_contact_policy))
        .route(
            "/v1/contact/policy/:pial_id",
            get(handlers::get_contact_policy),
        )
        .route("/v1/contact/evaluate", post(handlers::evaluate_contact))
        .route("/v1/contact/keybundle", post(handlers::contact_key_bundle))
        .route(
            "/v1/contact/capabilities/mint",
            post(handlers::mint_contact_capability),
        )
        .route(
            "/v1/contact/capabilities/revoke",
            post(handlers::revoke_contact_capability),
        )
        .route(
            "/v1/contact/capabilities/:pial_id",
            get(handlers::list_contact_capabilities),
        )
        .route(
            "/v1/contact/requests/decide",
            post(handlers::decide_contact_request),
        )
        .route(
            "/v1/contact/requests/:pial_id",
            get(handlers::list_contact_requests),
        )
        .route(
            "/v1/pial/messaging-key/register",
            post(handlers::register_messaging_key),
        )
        // Decision engine
        .route("/v1/decisions", post(handlers::make_decision))
        .route("/v1/decisions/log", get(handlers::decision_log))
        // NEXUS tier proxy — routes to Verity; Nantar calls this instead of Verity directly
        .route("/v1/nexus/tier/:shard", get(handlers::nexus_tier))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Elohim Veni security brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
