pub mod brain;
pub mod handlers;
pub mod types;

use axum::{
    routing::{get, post},
    Router,
};
use std::sync::Arc;
use tower_http::trace::TraceLayer;

use brain::{brain_heartbeat, brain_status, receive_signal, register_brain};
use handlers::{feedback_handler, health_handler, rank_handler, AppState};

pub fn router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/metrics", get(crate::observ::metrics_handler))
        .route("/rank", post(rank_handler))
        .route("/feedback", post(feedback_handler))
        .route("/health", get(health_handler))
        // Brain registry endpoints
        .route("/brain/register", post(register_brain))
        .route("/brain/heartbeat", post(brain_heartbeat))
        .route("/brain/status", get(brain_status))
        .route("/brain/signal", post(receive_signal))
        .layer(TraceLayer::new_for_http())
        .with_state(state)
        .layer(axum::middleware::from_fn(crate::observ::http_middleware))
}
