mod api;
mod auth;
mod compliance;
mod crypto;
mod db;
mod kyc;
mod manhattan_outbox;
mod models;
mod nexus;
mod nexus_sweep;
mod observ;
mod retention;

use axum::{
    extract::{DefaultBodyLimit, FromRef, State},
    response::Json,
    routing::{get, post},
    Router,
};
use serde_json::{json, Value};
use sqlx::PgPool;
use std::sync::Arc;
use tracing::{info, warn};

use auth::InternalApiKey;
use crypto::RecordsKey;
use manhattan_client::Manhattan;

/// Verity owns EVIDENCE and LEGAL RECORDS: the KYC evidence for each attempt,
/// the document-reuse index, the 2257 records with their encrypted ID copies,
/// the audit trail, and the retention that ends them. It does not decide or
/// store a verification level — identity (elohim-veni) owns that.
pub struct AppState {
    pool: PgPool,
    /// The naming plane, reached by name. Verity owns no identity — every PIAL
    /// its tables carry is a reference resolved through here rather than a
    /// foreign key into a brain that owns it.
    manhattan: Manhattan,
    /// The service-to-service secret. Every evidence and compliance route
    /// requires it — see `auth`.
    internal_api_key: InternalApiKey,
    /// Seals the 2257 ID copies at rest.
    records_key: RecordsKey,
}

// The `Caller` extractor reaches the configured key through `FromRef`. The
// router state is `Arc<AppState>`, so the impl is on the Arc.
impl FromRef<Arc<AppState>> for InternalApiKey {
    fn from_ref(state: &Arc<AppState>) -> InternalApiKey {
        state.internal_api_key.clone()
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("verity")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8095".into());

    // Verity holds legal records. A brain that cannot tell a peer from a
    // stranger must not start and then quietly accept both — an empty key
    // would make every caller a trusted service.
    let internal_api_key = InternalApiKey::from_env();
    if internal_api_key.is_empty() {
        anyhow::bail!(
            "INTERNAL_API_KEY must be set for Verity: it authenticates the \
             service-to-service lane on every evidence and compliance route"
        );
    }

    // No key, no start: there is no mode in which an ID copy is stored in the
    // clear.
    let records_key = RecordsKey::from_env()?;
    info!(key_id = records_key.key_id(), "records key loaded");

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Verity schema up to date");

    let manhattan = Manhattan::from_env("verity")?;
    manhattan_outbox::spawn(database_url.clone(), manhattan.clone());
    nexus_sweep::spawn(pool.clone(), nexus_sweep::interval_from_env());
    retention::spawn(pool.clone(), retention::interval_from_env());

    let state = Arc::new(AppState {
        pool: pool.clone(),
        manhattan,
        internal_api_key: internal_api_key.clone(),
        records_key,
    });
    let nexus_state = Arc::new(nexus::NexusState {
        pool,
        internal_api_key,
    });

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(health))
        // ── KYC evidence ───────────────────────────────────────────────────
        // One attempt, its evidence and its audit rows — written by
        // feed-engine after ekyc decides. The body carries the ID images for a
        // creator pass, so this route alone takes a larger body.
        .route(
            "/v1/kyc/submissions",
            post(kyc::submit).layer(DefaultBodyLimit::max(kyc::SUBMISSION_BODY_LIMIT)),
        )
        .route("/v1/kyc/:pial_id", get(kyc::summary))
        // ── 18 U.S.C. 2257 ─────────────────────────────────────────────────
        .route("/v1/compliance/2257/record", post(compliance::record))
        .route(
            "/v1/compliance/2257/content-removed",
            post(compliance::content_removed),
        )
        .route("/v1/compliance/2257/:pial_id", get(compliance::get))
        .with_state(state)
        .merge(nexus::nexus_router(nexus_state))
        .layer(axum::middleware::from_fn(observ::http_middleware));

    // Binds 0.0.0.0 inside the container so peer brains on the internal docker
    // network reach it as `verity:8095`. LAN exposure is removed at the host
    // boundary instead: prod publishes no port for this brain, and local
    // publishes it on `127.0.0.1:8095` rather than `0.0.0.0` — see the compose
    // files. Binding loopback here would break service-to-service.
    let addr = format!("0.0.0.0:{port}");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    info!("Verity listening on {addr}");
    axum::serve(listener, app).await?;
    Ok(())
}

// ── Health ────────────────────────────────────────────────────────────────────
//
// The naming plane is reported, never depended on: if Manhattan is unreachable
// the registrations queue in the outbox, the API answers normally, and names
// simply stop resolving elsewhere. The backlog is reported alongside it because
// depth alone says a queue is behind — the age of the head and the error it
// last failed with say whether it is moving at all.
//
// A failed backlog query is reported, not swallowed, but it does not fail the
// check: this endpoint is the container healthcheck other brains gate their
// start-up on, and turning it into a database liveness probe is a change to
// make deliberately, not as a side effect of adding a counter to it.

async fn health(State(state): State<Arc<AppState>>) -> Json<Value> {
    let configured = state.manhattan.configured();

    match db::outbox_backlog(&state.pool).await {
        Ok(backlog) => Json(json!({
            "status":                              "ok",
            "service":                             "verity",
            "manhattan":                           configured,
            "manhattan_outbox_pending":            backlog.pending,
            "manhattan_outbox_oldest_age_seconds": backlog.oldest_age_seconds,
            "manhattan_outbox_head_error":         backlog.head_last_error,
            // Rows the drain gave up on, and persona links with no name to
            // express them yet. Both are naming-plane writes that have not
            // happened, and neither is allowed to be invisible.
            "manhattan_outbox_quarantined":        backlog.quarantined,
            "deferred_persona_links":
                db::deferred_persona_links(&state.pool).await.ok(),
        })),
        Err(e) => {
            warn!(error = %e, "manhattan outbox backlog query failed");
            Json(json!({
                "status":                   "degraded",
                "service":                  "verity",
                "manhattan":                configured,
                "manhattan_outbox_pending": Value::Null,
                "manhattan_outbox_error":   e.to_string(),
            }))
        }
    }
}

