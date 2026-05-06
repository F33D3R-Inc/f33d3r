mod brain;
#[allow(dead_code)] mod crypto;
mod db;
mod handlers;
mod models;
#[allow(dead_code)] mod protocol;
#[allow(dead_code)] mod ratchet;
mod relay;
#[allow(dead_code)] mod x3dh;

use axum::{
    routing::{delete, get, post},
    Router,
};
use tracing::info;
use tracing_subscriber::{layer::SubscriberExt, util::SubscriberInitExt, EnvFilter};

use handlers::AppState;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::registry()
        .with(EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()))
        .with(tracing_subscriber::fmt::layer())
        .init();

    let database_url = std::env::var("DATABASE_URL")
        .expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8092".into());

    let registry_url   = std::env::var("REGISTRY_URL")
        .unwrap_or_else(|_| "http://localhost:8090".into());
    let aethyrrank_url = std::env::var("AETHYRRANK_URL")
        .unwrap_or_else(|_| "http://localhost:8091".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("AethyrMsg schema up to date");

    let rate_limiter = relay::RateLimiter::new();
    let push         = relay::PushRegistry::new();
    let brain        = brain::BrainClient::new(
        registry_url,
        aethyrrank_url,
        port.parse().unwrap_or(8092),
    );

    // Register with schema registry — log but do not abort on failure.
    if let Err(e) = brain.register().await {
        tracing::warn!("schema registry registration skipped: {e}");
    }

    // Background: heartbeat + maintenance
    tokio::spawn(brain::BrainClient::run_heartbeat(std::sync::Arc::clone(&brain)));
    tokio::spawn(relay::run_maintenance(pool.clone(), std::sync::Arc::clone(&rate_limiter)));

    let state = AppState { pool, rate_limiter, push, brain };

    let app = Router::new()
        .route("/health",                           get(handlers::health))
        .route("/v1/stats",                         get(handlers::stats))
        // Identity + Glyph signing key
        .route("/v1/identity",                      post(handlers::register_identity))
        .route("/v1/identity/:id",                  get(handlers::get_identity))
        .route("/v1/identity/:id/glyph",            post(handlers::register_glyph))
        .route("/v1/identity/by-handle/:handle",    get(handlers::get_identity_by_handle))
        // Prekeys (X3DH V2)
        .route("/v1/prekeys/signed",                post(handlers::register_spk))
        .route("/v1/prekeys/onetime",               post(handlers::upload_otpk))
        .route("/v1/prekeys/:identity",             get(handlers::get_prekey_bundle))
        // Full AMP messages (X3DH + ratchet, V2)
        .route("/v1/messages",                      post(handlers::send_message))
        .route("/v1/messages/:identity",            get(handlers::fetch_messages))
        .route("/v1/message/:id",                   delete(handlers::ack_message))
        // V1 simple DM relay (ECDH-AES-GCM, no X3DH required)
        .route("/v1/dm",                            post(handlers::send_dm))
        .route("/v1/dm/:identity",                  get(handlers::fetch_dms))
        // Groups + WebSocket
        .route("/v1/groups",                        post(handlers::create_group))
        .route("/v1/groups/:group_id/members",      get(handlers::get_group_members)
                                                        .post(handlers::add_group_member))
        .route("/v1/push/:identity",                get(handlers::ws_push))
        // Relay node network + Karma engine
        .route("/v1/relay/nodes",                   get(handlers::list_relay_nodes)
                                                        .post(handlers::register_relay_node))
        .route("/v1/relay/nodes/:id/heartbeat",     post(handlers::relay_heartbeat))
        .route("/v1/relay/route/:identity",         get(handlers::get_route))
        .route("/v1/relay/karma",                   post(handlers::record_karma_event))
        // Ratchet state persistence (encrypted blob from client)
        .route("/v1/ratchet/:local/:remote",        get(handlers::get_ratchet_state)
                                                        .post(handlers::save_ratchet_state))
        // ── Vovin v2: Multi-device registry ──────────────────────────────────
        .route("/v1/devices",                       post(handlers::register_device))
        .route("/v1/devices/:pial_id",              get(handlers::list_devices))
        // ── Aethyr File Fabric (AFF) ──────────────────────────────────────────
        // File upload: POST chunk-by-chunk. First chunk creates manifest.
        // File download: GET manifest to know chunk count, then GET each chunk.
        .route("/v1/aff/upload",                    post(handlers::aff_upload_chunk))
        .route("/v1/aff/:file_id",                  get(handlers::aff_get_manifest))
        .route("/v1/aff/:file_id/chunk/:index",     get(handlers::aff_get_chunk))
        .layer(tower_http::cors::CorsLayer::permissive())
        .with_state(state);

    let addr = format!("0.0.0.0:{}", port);
    info!("AethyrMsg relay listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
