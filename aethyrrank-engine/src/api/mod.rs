pub mod handlers;
pub mod types;

use axum::{
    routing::{get, post},
    Router,
};
use std::sync::Arc;
use tower_http::trace::TraceLayer;

use handlers::{health_handler, rank_handler, AppState};

/// /rank is the only working route. Ranking signals arrive on Sitra Achra
/// ranking.events (crate::learning), never over HTTP.
pub fn router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/metrics", get(crate::observ::metrics_handler))
        .route("/rank", post(rank_handler))
        .route("/health", get(health_handler))
        .layer(axum::middleware::from_fn_with_state(
            Arc::clone(&state),
            crate::auth::require_internal_key,
        ))
        .layer(TraceLayer::new_for_http())
        .with_state(state)
        .layer(axum::middleware::from_fn(crate::observ::http_middleware))
}
