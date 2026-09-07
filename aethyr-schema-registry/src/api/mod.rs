pub mod handlers;
pub mod types;

use axum::{
    routing::{get, post},
    Router,
};
use std::sync::Arc;
use tower_http::trace::TraceLayer;

use handlers::{
    brain_heartbeat, check_compatibility, get_schema, health, list_brains, list_schema_versions,
    register_brain, register_schema, validate_payload, AppState,
};

pub fn router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/metrics", get(crate::observ::metrics_handler))
        // Brain registration and liveness
        .route("/brain/register", post(register_brain))
        .route("/brain/heartbeat", post(brain_heartbeat))
        .route("/brains", get(list_brains))
        // Schema CRUD
        .route("/schema/register", post(register_schema))
        .route("/schema/:brain_id/:signal_type", get(get_schema))
        .route(
            "/schema/:brain_id/:signal_type/versions",
            get(list_schema_versions),
        )
        // Validation and compatibility
        .route("/schema/validate", post(validate_payload))
        .route("/schema/compatibility", post(check_compatibility))
        // Health
        .route("/health", get(health))
        .layer(TraceLayer::new_for_http())
        .with_state(state)
        .layer(axum::middleware::from_fn(crate::observ::http_middleware))
}
