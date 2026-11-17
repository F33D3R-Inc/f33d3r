mod api;
mod contact;
mod db;
mod decision;
mod follow_graph;
mod handles;
mod handlers;
mod identity;
mod import;
mod manhattan_outbox;
mod migrate;
mod models;
mod notifications;
mod number_lease;
mod observ;

use axum::{
    routing::{delete, get, post, put},
    Router,
};
use tracing::info;

use handlers::AppState;

/// Where the PIAL assertion signing keys live (identity/tokens.rs).
fn signing_key_dir() -> std::path::PathBuf {
    std::env::var("PIAL_SIGNING_KEY_DIR")
        .unwrap_or_else(|_| "/var/lib/elohim-veni/keys".into())
        .into()
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("elohim_veni")?;

    let database_url = std::env::var("DATABASE_URL").map_err(|_| anyhow::anyhow!("DATABASE_URL must be set"))?;

    // Subcommands: one-shot operations that share this binary's schema and
    // rules rather than reimplementing them in a script.
    match std::env::args().nth(1).as_deref() {
        None | Some("serve") => {}
        Some("import") => return import::run(&database_url).await,
        Some("rotate-signing-key") => {
            let pool = sqlx::PgPool::connect(&database_url).await?;
            db::migrate(&pool).await?;
            let kid = identity::tokens::rotate(&pool, &signing_key_dir()).await?;
            info!(kid = %kid, "new PIAL signing key written; it signs once it has been published for {}s",
                identity::tokens::PUBLISH_LEAD_SECS);
            return Ok(());
        }
        Some(other) => anyhow::bail!("unknown command {other:?}: expected serve | import | rotate-signing-key"),
    }

    let port = std::env::var("PORT").unwrap_or_else(|_| "8093".into());

    // Every route but /health, /metrics and the JWKS requires this key. Without
    // it there is nothing to compare against, and an identity authority that
    // accepts anyone is not one: refuse to boot.
    let internal_api_key = std::env::var("INTERNAL_API_KEY").unwrap_or_default();
    if internal_api_key.is_empty() {
        anyhow::bail!("INTERNAL_API_KEY must be set");
    }

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Elohim Veni schema up to date");

    let tokens = identity::tokens::TokenIssuer::open(&pool, signing_key_dir()).await?;
    tokens.spawn_reload();

    // The naming plane. Elohim Veni is the identity authority — Manhattan's
    // authority map gives this brain the identity, key and address node kinds —
    // so every PIAL and every key asserted here is registered there, and every
    // other brain's PIAL column is a reference to what this one owns.
    //
    // The client is built once and shared. Nothing writes to it directly: writes
    // go through the transactional outbox, which the drain below delivers.
    let manhattan = manhattan_client::Manhattan::from_env("elohim-veni")?;
    manhattan_outbox::spawn(database_url.clone(), manhattan.clone());
    notification_outbox::spawn(database_url.clone());
    identity::relay::spawn(
        pool.clone(),
        reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(10))
            .build()?,
    );
    identity::maintenance::spawn(pool.clone());

    // Expired Numbers stop admitting new contact at the decision, in SQL,
    // against this database's own clock — that enforcement does not depend on
    // this task running. The sweeper only releases the NAME in the naming
    // plane, so an expired Number becomes the same answer as a retired one all
    // the way down rather than only at the policy layer.
    number_lease::spawn(pool.clone(), manhattan.clone());

    // Fails boot on a malformed seed; an unset seed yields an unsigned signer that
    // reports itself as unsigned on every bundle.
    let bundle_signer = contact::BundleSigner::from_env()?;
    if !bundle_signer.configured() {
        tracing::warn!("CONTACT_BUNDLE_SIGNING_SEED is unset — contact key bundles ship unsigned");
    }

    let state = AppState {
        pool,
        internal_api_key,
        tokens,
        manhattan,
        bundle_signer,
    };

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        // Health
        .route("/health", get(handlers::health))
        // Stats
        .route("/v1/stats", get(handlers::stats))
        // PIAL assertion by another brain (feed-engine's assertPIAL).
        .route("/v1/pial/bootstrap", post(handlers::pial_bootstrap))
        // ── The identity authority ────────────────────────────────────────
        .route("/v1/auth/jwks", get(identity::http::jwks))
        .route("/v1/auth/login", post(identity::http::login))
        .route("/v1/accounts", post(identity::http::create_account))
        .route("/v1/accounts/founder", get(identity::http::get_founder))
        .route("/v1/accounts/by-handle/:handle", get(identity::http::get_account_by_handle))
        .route("/v1/accounts/:id", get(identity::http::get_account))
        .route("/v1/accounts/:id/password", post(identity::http::change_password))
        .route("/v1/accounts/:id/password/reset", post(identity::http::reset_password))
        .route("/v1/accounts/:id/deactivate", post(identity::http::deactivate_account))
        .route("/v1/accounts/:id/purge", post(identity::http::purge_account))
        .route("/v1/accounts/:id/totp", get(identity::http::totp_status))
        .route("/v1/accounts/:id/totp/enroll", post(identity::http::totp_enroll))
        .route("/v1/accounts/:id/totp/confirm", post(identity::http::totp_confirm))
        .route("/v1/accounts/:id/totp/disable", post(identity::http::totp_disable))
        .route(
            "/v1/accounts/:id/backup-codes",
            get(identity::http::backup_code_status).post(identity::http::generate_backup_codes),
        )
        .route("/v1/accounts/:id/backup-codes/consume", post(identity::http::consume_backup_code))
        .route("/v1/recovery/verify", post(identity::http::recovery_verify))
        .route("/v1/recovery/complete", post(identity::http::recovery_complete))
        .route("/v1/sessions", get(identity::http::list_sessions))
        .route("/v1/sessions/resolve", post(identity::http::resolve_session))
        .route("/v1/sessions/revoke", post(identity::http::revoke_sessions))
        .route("/v1/sessions/switch", post(identity::http::switch_session))
        .route("/v1/sessions/:id", delete(identity::http::delete_session))
        .route("/v1/pials/:pial_id", get(identity::http::get_pial))
        .route("/v1/pials/:pial_id/facts", put(identity::http::put_facts))
        .route("/v1/pials/:pial_id/role", put(identity::http::put_role))
        .route("/v1/pials/:pial_id/status", put(identity::http::put_status))
        .route("/v1/pials/:pial_id/trust", put(identity::http::put_trust))
        .route("/v1/pials/:pial_id/capabilities/:cap", put(identity::http::put_capability))
        .route("/v1/pials/:pial_id/ban", post(identity::http::ban))
        .route("/v1/pials/:pial_id/unban", post(identity::http::unban))
        .route("/v1/pials/:pial_id/tombstone", post(identity::http::tombstone))
        .route("/v1/pials/:pial_id/accounts", post(identity::http::add_persona))
        .route("/v1/pials/:pial_id/accounts/link", post(identity::http::link_account))
        // ── Handles (the registry, merged in from registry-brain) ──────────
        .route("/v1/handles", post(handles::claim))
        .route("/v1/handles/search", get(handles::search))
        .route("/v1/handles/reserved", get(handles::list_reserved).post(handles::reserve))
        .route("/v1/handles/auctions/:id", get(handles::get_auction))
        .route("/v1/handles/auctions/:id/bid", post(handles::bid))
        .route("/v1/handles/auctions/:id/settle", post(handles::settle))
        .route("/v1/handles/:handle", get(handles::resolve))
        .route("/v1/handles/:handle/bind", post(handles::bind))
        .route("/v1/handles/:handle/unbind", post(handles::unbind))
        .route("/v1/handles/:handle/transfer", post(handles::transfer))
        .route("/v1/handles/:handle/admin", post(handles::admin))
        .route("/v1/handles/:handle/auction", post(handles::create_auction))
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
        .route("/v1/pial/:pial_id/trust", get(handlers::get_trust))
        // Contact addresses — this brain owns `address`/`addr` in Manhattan.
        .route("/v1/addresses/mint", post(handlers::mint_address))
        .route("/v1/addresses/revoke", post(handlers::revoke_address))
        .route("/v1/addresses/list", post(handlers::list_addresses))
        // F33D3R Numbers — allocation, rotation and per-Number policy.
        .route("/v1/numbers/mint", post(handlers::mint_number))
        .route("/v1/numbers/revoke", post(handlers::revoke_number))
        .route("/v1/numbers/policy", post(handlers::set_number_policy))
        .route("/v1/numbers/:account_id", get(handlers::list_numbers))
        // Contact policy, capabilities, requests and key discovery.
        .route("/v1/contact/policy", post(handlers::set_contact_policy))
        .route(
            "/v1/contact/policy/:account_id",
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
            "/v1/contact/capabilities/:account_id",
            get(handlers::list_contact_capabilities),
        )
        .route(
            "/v1/contact/requests/decide",
            post(handlers::decide_contact_request),
        )
        .route(
            "/v1/contact/requests/:account_id",
            get(handlers::list_contact_requests),
        )
        .route(
            "/v1/pial/messaging-key/register",
            post(handlers::register_messaging_key),
        )
        // Decision engine
        .route("/v1/decisions", post(handlers::make_decision))
        .route("/v1/decisions/log", get(handlers::decision_log))
        .layer(axum::middleware::from_fn_with_state(
            state.clone(),
            api::require_internal_key,
        ))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Elohim Veni security brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
