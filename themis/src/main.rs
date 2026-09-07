mod cek;
mod clients;
mod config;
mod db;
mod handlers;
mod manhattan_outbox;
mod models;
mod observ;
mod pial_ref;
mod watcher;

use axum::{
    middleware,
    routing::{delete, get, post, put},
    Router,
};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::Mutex;
use tracing::info;
use watcher::{WatchMap, XrpWatchMap};

pub struct AppState {
    pub pool: sqlx::PgPool,
    pub cfg: config::Config,
    pub http: reqwest::Client,
    /// The naming plane. Themis owns no identity and no work — it resolves both
    /// by name through Manhattan rather than assuming another brain's rows.
    pub manhattan: manhattan_client::Manhattan,
    pub themis_privkey: p256::SecretKey,
    pub themis_pubkey_b64: String,
    pub watch_map: WatchMap,
    pub xrp_watch_map: XrpWatchMap,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let _ = rustls::crypto::aws_lc_rs::default_provider().install_default();

    observ::init("themis")?;
    let cfg = config::Config::from_env();
    let pool = sqlx::PgPool::connect(&cfg.database_url).await?;
    db::migrate(&pool).await?;
    info!("Themis schema up to date");

    let privkey = cek::load_privkey(&cfg.themis_privkey_b64)?;
    let pubkey_b64 = cek::pubkey_to_b64(&privkey.public_key());
    info!("Themis ECDH pubkey: {}", &pubkey_b64[..16]);

    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(10))
        .build()?;

    let manhattan = manhattan_client::Manhattan::from_env("themis")?;
    if manhattan.configured() {
        info!("Manhattan naming plane configured");
    }

    let watch_map: WatchMap = Arc::new(Mutex::new(HashMap::new()));
    let xrp_watch_map: XrpWatchMap = Arc::new(Mutex::new(HashMap::new()));

    let state = Arc::new(AppState {
        pool,
        cfg,
        http,
        manhattan: manhattan.clone(),
        themis_privkey: privkey,
        themis_pubkey_b64: pubkey_b64,
        watch_map: watch_map.clone(),
        xrp_watch_map: xrp_watch_map.clone(),
    });

    tokio::spawn(manhattan_outbox::run_drain(state.cfg.database_url.clone(), manhattan));
    tokio::spawn(watcher::run_watcher(state.clone(), watch_map));
    tokio::spawn(watcher::run_xrp_watcher(state.clone(), xrp_watch_map));

    let app = Router::new()
        // ── Core health / metrics ───────────────────────────────────────────
        .route("/health", get(handlers::health))
        .route("/metrics", get(observ::metrics_handler))
        // ── Bitcoin + XRP marketplace ───────────────────────────────────────
        .route("/v1/pubkey", get(handlers::pubkey))
        .route("/v1/events", post(handlers::events))
        .route("/v1/shop/:seller_pial/status", get(handlers::shop_status))
        .route("/v1/shop/:seller_pial", get(handlers::shop_listings))
        .route("/v1/marketplace", get(handlers::marketplace_feed))
        .route("/v1/purchase/:id", get(handlers::purchase_status))
        .route("/v1/qr/:id", get(handlers::purchase_qr))
        .route("/v1/seller/:seller_pial/stats", get(handlers::seller_stats))
        .route("/v1/library", get(handlers::buyer_library))
        .route("/v1/access", get(handlers::access_check))
        // ── Creator commerce ────────────────────────────────────────────────
        .route("/v1/commerce/stats", get(handlers::commerce_stats))
        .route("/v1/creator/enable", post(handlers::creator_enable))
        .route("/v1/creator/:pial_id/status", get(handlers::creator_status))
        .route("/v1/creator/:pial_id/plans", get(handlers::list_plans))
        .route("/v1/plans", post(handlers::create_plan))
        .route(
            "/v1/plans/:id",
            put(handlers::update_plan).delete(handlers::delete_plan),
        )
        .route("/v1/subscribe", post(handlers::subscribe))
        .route("/v1/subscribe", delete(handlers::unsubscribe))
        .route("/v1/access/subscription", get(handlers::check_subscription))
        .route(
            "/v1/creator/:pial_id/subscribers",
            get(handlers::list_subscribers),
        )
        .route(
            "/v1/subscriber/:pial_id/subscriptions",
            get(handlers::list_subscriptions),
        )
        .route("/v1/ppv", post(handlers::create_ppv))
        .route("/v1/ppv/:id/purchase", post(handlers::purchase_ppv))
        .route("/v1/access/ppv", get(handlers::check_ppv_access))
        .route("/v1/creator/:pial_id/ppv", get(handlers::list_ppv_items))
        .route("/v1/tips", post(handlers::send_tip))
        .route(
            "/v1/creator/:pial_id/earnings",
            get(handlers::creator_earnings),
        )
        .layer(middleware::from_fn(observ::http_middleware))
        .with_state(state);

    let addr = format!("0.0.0.0:{}", "8100");
    info!("Themis listening on {addr}");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
