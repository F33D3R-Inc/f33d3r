mod db;
mod handlers;
mod identity;
mod inbox;
mod manhattan_outbox;
mod models;
mod observ;

use axum::{
    routing::{delete, get, post},
    Router,
};
use tracing::info;

use handlers::AppState;
use manhattan_client::Manhattan;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("herald")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8105".into());
    let vapid_pub_key =
        std::env::var("VAPID_PUBLIC_KEY").unwrap_or_else(|_| "not-configured".into());
    let vapid_private_key = std::env::var("VAPID_PRIVATE_KEY").unwrap_or_default();
    let vapid_subject =
        std::env::var("VAPID_SUBJECT").unwrap_or_else(|_| "mailto:admin@f33d3r.com".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Herald push notification schema up to date");

    // The naming plane, reached by name. Herald resolves identities through it
    // and registers the names it meets; it never claims to own one.
    let manhattan = Manhattan::from_env("herald")?;
    info!(
        configured = manhattan.configured(),
        "Manhattan naming plane client built"
    );

    // The outbox drain. Registrations are already durable at this point — the
    // triggers wrote them in the same transaction as the row that caused them —
    // so this task only has to deliver them, in order, and say so when it cannot.
    tokio::spawn(manhattan_outbox::start_drain(
        database_url.clone(),
        manhattan.clone(),
    ));

    let internal_api_key = std::env::var("INTERNAL_API_KEY").unwrap_or_default();
    let brokers = std::env::var("KAFKA_BROKERS").unwrap_or_default();
    let inbox_bus = if brokers.is_empty() {
        tracing::warn!("KAFKA_BROKERS is empty: the notification inbox will not receive events");
        None
    } else {
        Some(inbox::InboxBus::new(&brokers)?)
    };

    let state = AppState {
        pool,
        vapid_pub_key,
        vapid_private_key,
        vapid_subject,
        manhattan,
        internal_api_key,
        inbox_bus: inbox_bus.clone(),
        feed_engine_url: std::env::var("FEED_ENGINE_URL")
            .unwrap_or_else(|_| "http://feed-engine:8081".into()),
        http: reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(5))
            .build()?,
    };

    if let Some(bus) = inbox_bus {
        inbox::spawn(state.clone(), bus, brokers);
    }

    // Only other brains may reach these. Subscriptions and preferences are private,
    // and this port is published in local development.
    let internal = Router::new()
        .route("/v1/stats", get(handlers::stats))
        .route("/v1/subscriptions", post(handlers::subscribe))
        .route(
            "/v1/subscriptions/:device_id",
            delete(handlers::unsubscribe),
        )
        .route(
            "/v1/preferences/:pial_shard_id",
            get(handlers::get_preferences).patch(handlers::update_preferences),
        )
        .route_layer(axum::middleware::from_fn_with_state(
            state.clone(),
            inbox::require_internal,
        ));

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(handlers::health))
        .route("/v1/vapid-public-key", get(handlers::vapid_public_key))
        .route("/v1/inbox/purge", post(inbox::purge))
        .route("/v1/inbox/missing", post(inbox::missing))
        .route("/v1/inbox/:account", get(inbox::list))
        .route("/v1/inbox/:account/unread", get(inbox::unread))
        .route("/v1/inbox/:account/group/:id", get(inbox::group))
        .route("/v1/inbox/:account/read", post(inbox::mark_read))
        .route("/v1/inbox/:account/actors", get(inbox::actors))
        .merge(internal)
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{}", port);
    info!("Herald push notification brain listening on {}", addr);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}
