use axum::{extract::State, http::StatusCode, response::Json};
use serde_json::{json, Value};

use crate::manhattan_outbox;
use crate::migrate;
use crate::repository::postgres as pg;
use crate::state::AppState;

type Reply = Result<Json<Value>, (StatusCode, Json<Value>)>;

fn fail(context: &str, e: impl std::fmt::Display) -> (StatusCode, Json<Value>) {
    tracing::error!(error = %e, "{context}");
    (
        StatusCode::INTERNAL_SERVER_ERROR,
        Json(json!({ "status": "error", "error": context })),
    )
}

/// Liveness plus the two backlogs that tell an operator what is actually
/// wrong: a drain that stopped shows up here as a number that climbs.
pub async fn health(State(s): State<AppState>) -> Reply {
    let manhattan_backlog = manhattan_outbox::backlog(&s.pool)
        .await
        .map_err(|e| fail("manhattan outbox backlog", e))?;
    let events_unpublished = pg::unpublished_events(&s.pool)
        .await
        .map_err(|e| fail("event outbox backlog", e))?;
    let schema = migrate::schema_version(&s.pool)
        .await
        .map_err(|e| fail("schema version", e))?;

    Ok(Json(json!({
        "status": "ok",
        "service": "auralis",
        "node_id": s.config.node_id,
        "schema_version": schema,
        "manhattan": s.manhattan.configured(),
        "manhattan_outbox_backlog": manhattan_backlog,
        "events_unpublished": events_unpublished,
    })))
}

/// Readiness: this node can serve. PostgreSQL answers, Redis answers. The
/// media socket joins this check in Phase 3.
pub async fn ready(State(s): State<AppState>) -> Reply {
    let (n,): (i32,) = sqlx::query_as("SELECT 1")
        .fetch_one(&s.pool)
        .await
        .map_err(|e| fail("postgres not ready", e))?;
    debug_assert_eq!(n, 1);
    s.hot.ping().await.map_err(|e| fail("redis not ready", e))?;
    Ok(Json(json!({ "ready": true, "node_id": s.config.node_id })))
}
