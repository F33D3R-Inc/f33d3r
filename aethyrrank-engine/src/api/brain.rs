//! Brain registry API endpoints.
//!
//! POST /brain/register  — called by every brain on startup
//! POST /brain/heartbeat — called every 30s by each brain
//! GET  /brain/status    — returns all brain statuses
//! POST /brain/signal    — receives signals from registered brains

use axum::{extract::State, http::StatusCode, Json};
use chrono::Utc;
use std::sync::Arc;
use tracing::{info, warn};

use crate::api::handlers::AppState;
use crate::contracts::brain_id::BrainId;
use crate::contracts::signal::{BrainRegistration, BrainSignal};

// ── POST /brain/register ──────────────────────────────────────────────────────

pub async fn register_brain(
    State(state): State<Arc<AppState>>,
    Json(reg): Json<BrainRegistration>,
) -> StatusCode {
    info!(
        brain   = %reg.brain_id,
        version = reg.schema_version,
        port    = reg.port,
        "brain registration received"
    );
    state.brain_registry.register(reg);
    StatusCode::OK
}

// ── POST /brain/heartbeat ─────────────────────────────────────────────────────

pub async fn brain_heartbeat(
    State(state): State<Arc<AppState>>,
    Json(body): Json<serde_json::Value>,
) -> StatusCode {
    let brain_id: BrainId = match serde_json::from_value(
        body.get("brain_id")
            .cloned()
            .unwrap_or(serde_json::Value::Null),
    ) {
        Ok(id) => id,
        Err(e) => {
            warn!(error = %e, "heartbeat with invalid brain_id");
            return StatusCode::BAD_REQUEST;
        }
    };
    state.brain_registry.heartbeat(brain_id);
    StatusCode::OK
}

// ── GET /brain/status ─────────────────────────────────────────────────────────

pub async fn brain_status(State(state): State<Arc<AppState>>) -> Json<serde_json::Value> {
    let snapshot = state.brain_registry.status_snapshot();
    Json(serde_json::json!({
        "ts":     Utc::now().to_rfc3339(),
        "brains": snapshot,
    }))
}

// ── POST /brain/signal ────────────────────────────────────────────────────────

pub async fn receive_signal(
    State(state): State<Arc<AppState>>,
    Json(signal): Json<BrainSignal>,
) -> StatusCode {
    let accepted = state.brain_registry.record_signal(signal.source_brain);
    if !accepted {
        warn!(brain = %signal.source_brain, "signal rejected — brain not online");
        return StatusCode::FORBIDDEN;
    }
    info!(
        brain       = %signal.source_brain,
        signal_type = ?signal.signal_type,
        schema_v    = signal.schema_version,
        "brain signal received"
    );
    // Future: route signal to appropriate handler based on signal_type
    StatusCode::ACCEPTED
}
