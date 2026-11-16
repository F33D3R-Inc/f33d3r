mod notifications;
mod addresses;
mod admin;
mod caeor;
mod cek;
mod charges;
mod clients;
mod commerce;
mod commerce_events;
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
    routing::{get, post, put},
    Router,
};
use std::sync::Arc;
use tracing::info;

pub struct AppState {
    pub pool: sqlx::PgPool,
    pub cfg: config::Config,
    pub http: reqwest::Client,
    /// The naming plane. Themis owns no identity and no work — it resolves both
    /// by name through Manhattan rather than assuming another brain's rows.
    pub manhattan: manhattan_client::Manhattan,
    pub themis_privkey: p256::SecretKey,
    pub themis_pubkey_b64: String,
    /// The AET unit, learnt once from Ain Soph, which owns it.
    pub units_per_aet: tokio::sync::OnceCell<i64>,
}

impl AppState {
    /// Units per AET, as Ain Soph publishes it. Asked for once and kept: the
    /// unit is a constant of the ledger, not a setting.
    pub async fn units_per_aet(&self) -> anyhow::Result<i64> {
        self.units_per_aet
            .get_or_try_init(|| async {
                clients::AinSoph {
                    http: &self.http,
                    url: &self.cfg.ain_soph_url,
                    key: &self.cfg.internal_api_key,
                }
                .units_per_aet()
                .await
            })
            .await
            .copied()
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let _ = rustls::crypto::aws_lc_rs::default_provider().install_default();

    observ::init("themis")?;
    let cfg = config::Config::from_env()?;
    let pool = sqlx::PgPool::connect(&cfg.database_url).await?;
    notification_outbox::spawn(cfg.database_url.clone());
    db::migrate(&pool).await?;
    info!("Themis schema up to date");

    let privkey = cek::load_privkey(&pool, &cfg.themis_privkey_b64, &cfg.themis_key_dir).await?;
    let pubkey_b64 = cek::pubkey_to_b64(&privkey.public_key());
    info!("Themis ECDH pubkey: {}", &pubkey_b64[..16]);

    let http = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(10))
        .build()?;

    let manhattan = manhattan_client::Manhattan::from_env("themis")?;
    if manhattan.configured() {
        info!("Manhattan naming plane configured");
    }


    let state = Arc::new(AppState {
        pool,
        cfg,
        http,
        manhattan: manhattan.clone(),
        themis_privkey: privkey,
        themis_pubkey_b64: pubkey_b64,
        units_per_aet: tokio::sync::OnceCell::new(),
    });

    commerce_events::spawn_relay(state.pool.clone(), state.cfg.kafka_brokers.clone());
    charges::spawn_maintenance(state.clone());

    tokio::spawn(manhattan_outbox::run_drain(state.cfg.database_url.clone(), manhattan));
    tokio::spawn(watcher::run_btc_watcher(state.clone()));
    tokio::spawn(watcher::run_xrp_watcher(state.clone()));
    tokio::spawn(watcher::run_delivery_retry(state.clone()));

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
        .route("/v1/commerce/stats", get(commerce::commerce_stats))
        .route("/v1/creator/enable", post(commerce::creator_enable))
        .route("/v1/creator/:pial_id/status", get(commerce::creator_status))
        .route("/v1/creator/:pial_id/plans", get(commerce::list_plans))
        .route("/v1/plans", post(commerce::create_plan))
        .route(
            "/v1/plans/:id",
            put(commerce::update_plan).delete(commerce::delete_plan),
        )
        .route(
            "/v1/subscribe",
            post(commerce::subscribe).delete(commerce::unsubscribe),
        )
        .route("/v1/access/subscription", get(commerce::check_subscription))
        .route(
            "/v1/creator/:pial_id/subscribers",
            get(commerce::list_subscribers),
        )
        .route(
            "/v1/subscriber/:pial_id/subscriptions",
            get(commerce::list_subscriptions),
        )
        .route("/v1/ppv", post(commerce::create_ppv))
        .route("/v1/ppv/:id/purchase", post(commerce::purchase_ppv))
        .route("/v1/access/ppv", get(commerce::check_ppv_access))
        .route("/v1/creator/:pial_id/ppv", get(commerce::list_ppv_items))
        .route("/v1/tips", post(commerce::send_tip))
        .route(
            "/v1/creator/:pial_id/earnings",
            get(commerce::creator_earnings),
        )
        // ── Platform value moves (peer calls, founder/admin checked by the caller)
        .route("/v1/admin/aet/credit", post(admin::credit))
        .route("/v1/admin/fees/sweep", post(admin::sweep_fees))
        .route("/v1/admin/accounts/reattribute", post(admin::reattribute))
        .route("/v1/admin/import/subscriptions", post(admin::import))
        .layer(middleware::from_fn_with_state(state.clone(), handlers::require_service_key))
        .layer(middleware::from_fn(observ::http_middleware))
        .with_state(state);

    let addr = format!("0.0.0.0:{}", "8100");
    info!("Themis listening on {addr}");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
