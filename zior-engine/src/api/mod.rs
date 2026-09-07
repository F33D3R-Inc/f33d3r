pub mod handlers;
pub mod types;

use axum::{
    extract::DefaultBodyLimit,
    routing::{get, post},
    Router,
};
use std::sync::Arc;
use tower_http::trace::TraceLayer;

use crate::api::handlers::ZiorState;

pub fn router(state: Arc<ZiorState>) -> Router {
    // The request body may carry an audio file up to the configured upload
    // limit plus the multipart framing and the id fields around it. Without
    // this, axum's default 2 MiB body limit rejects every real track before
    // the handler's own size check ever runs.
    let body_limit = state.config.audio.max_upload_bytes + (1 << 20);
    Router::new()
        .route("/metrics", get(crate::observ::metrics_handler))
        // Audio ingestion
        .route("/upload", post(handlers::upload_track))
        // Signal query (content-service reads this)
        .route("/signal/:track_id", get(handlers::get_signal))
        .route("/signals", get(handlers::list_signals))
        // Behavioral event ingestion
        .route("/events", post(handlers::ingest_events))
        // Similarity / clustering
        .route("/similar/:track_id", get(handlers::similar_tracks))
        .route("/clusters", get(handlers::get_clusters))
        // Health
        .route("/health", get(handlers::health))
        .layer(DefaultBodyLimit::max(body_limit))
        .layer(TraceLayer::new_for_http())
        .with_state(state)
        .layer(axum::middleware::from_fn(crate::observ::http_middleware))
}
