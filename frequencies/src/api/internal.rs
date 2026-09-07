//! Platform operations: the shared secret alone, no person behind the call.

use axum::{
    extract::{Path, State},
    http::StatusCode,
    Json,
};
use metrics::counter;
use serde::Deserialize;
use serde_json::{json, Value};
use uuid::Uuid;

use super::{ops, view};
use crate::auth::Service;
use crate::domain::FrequencyState;
use crate::error::Result;
use crate::security::abuse;
use crate::state::AppState;
use crate::telemetry::metrics as m;

#[derive(Deserialize)]
pub struct TerminateBody {
    #[serde(default)]
    pub reason: String,
}

/// Moderation termination (§23). Immediately: no new joins, every session
/// invalidated, hot state cleared, event published, state terminal. Nothing
/// here depends on a browser cooperating.
pub async fn terminate(
    State(s): State<AppState>,
    svc: Service,
    Path(id): Path<Uuid>,
    Json(body): Json<TerminateBody>,
) -> Result<(StatusCode, Json<Value>)> {
    let reason = abuse::reason(&body.reason)?;
    let reason = if reason.is_empty() {
        "moderation".to_string()
    } else {
        reason
    };
    let cause = ops::Cause::service(&svc.correlation_id);
    let freq = ops::end(
        &s,
        id,
        &cause,
        &reason,
        FrequencyState::ModerationTerminated,
    )
    .await?;
    counter!(m::MODERATION_ACTIONS_TOTAL, "action" => "terminate").increment(1);
    let mut conn = s.pool.acquire().await?;
    let v = view::build(&mut conn, &s.hot, &freq, None).await?;
    Ok((StatusCode::OK, Json(json!({ "frequency": v }))))
}
