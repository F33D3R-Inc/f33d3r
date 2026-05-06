//! Schema registry HTTP handlers.
//!
//! Every endpoint is documented with its purpose, input, and output.
//! All errors return structured JSON, never raw strings.

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    Json,
};
use chrono::Utc;
use serde::Deserialize;
use sqlx::PgPool;
use std::sync::Arc;
use tracing::{info, warn};

use crate::api::types::*;
use crate::config::RegistryConfig;
use crate::db;
use crate::validator;

pub struct AppState {
    pub pool: PgPool,
    pub config: RegistryConfig,
}

type State_ = State<Arc<AppState>>;
type ApiResult<T> = Result<Json<T>, (StatusCode, Json<ErrorResponse>)>;

fn db_err(e: impl std::fmt::Display) -> (StatusCode, Json<ErrorResponse>) {
    warn!(error = %e, "database error");
    (
        StatusCode::INTERNAL_SERVER_ERROR,
        Json(ErrorResponse::new(format!("database error: {}", e))),
    )
}

// ── POST /brain/register ──────────────────────────────────────────────────────

/// Register a brain with the schema registry.
/// Called by every brain on startup. Idempotent.
pub async fn register_brain(
    State(state): State_,
    Json(req): Json<RegisterBrainRequest>,
) -> ApiResult<serde_json::Value> {
    info!(brain = %req.brain_id, version = req.schema_version, port = req.port, "brain register");

    let record = BrainRegistrationRecord {
        brain_id: req.brain_id.clone(),
        current_version: req.schema_version,
        port: req.port,
        host: req.host.unwrap_or_else(|| "localhost".into()),
        signal_types: req.signal_types,
        description: req.description.unwrap_or_default(),
        status: "online".into(),
        last_seen: Utc::now(),
    };

    db::upsert_brain(&state.pool, &record)
        .await
        .map_err(db_err)?;

    Ok(Json(serde_json::json!({
        "status":   "registered",
        "brain_id": req.brain_id,
        "ts":       Utc::now().to_rfc3339(),
    })))
}

// ── POST /brain/heartbeat ─────────────────────────────────────────────────────

/// Record a heartbeat from a registered brain.
pub async fn brain_heartbeat(
    State(state): State_,
    Json(req): Json<HeartbeatRequest>,
) -> ApiResult<serde_json::Value> {
    db::update_brain_heartbeat(&state.pool, &req.brain_id)
        .await
        .map_err(db_err)?;
    Ok(Json(
        serde_json::json!({ "status": "ok", "ts": Utc::now().to_rfc3339() }),
    ))
}

// ── GET /brains ───────────────────────────────────────────────────────────────

/// List all registered brains with their current status.
pub async fn list_brains(State(state): State_) -> ApiResult<serde_json::Value> {
    let brains = db::get_all_brains(&state.pool).await.map_err(db_err)?;
    let online = brains.iter().filter(|b| b.status == "online").count();
    let planned = brains.iter().filter(|b| b.status == "planned").count();

    Ok(Json(serde_json::json!({
        "total":   brains.len(),
        "online":  online,
        "planned": planned,
        "brains":  brains,
        "ts":      Utc::now().to_rfc3339(),
    })))
}

// ── POST /schema/register ─────────────────────────────────────────────────────

/// Register a new schema version for a brain's signal type.
///
/// If a previous version exists, runs compatibility check first.
/// In strict mode (default), rejects incompatible changes.
pub async fn register_schema(
    State(state): State_,
    Json(req): Json<RegisterSchemaRequest>,
) -> ApiResult<RegisterSchemaResponse> {
    info!(
        brain       = %req.brain_id,
        signal_type = %req.signal_type,
        version     = req.version,
        "schema register"
    );

    // Check compatibility against previous version
    let prev = db::get_schema(&state.pool, &req.brain_id, &req.signal_type, None)
        .await
        .map_err(db_err)?;

    let (compatible, violations) = if let Some(prev_schema) = &prev {
        if prev_schema.version >= req.version {
            return Err((
                StatusCode::CONFLICT,
                Json(ErrorResponse::with_details(
                    "version conflict",
                    format!(
                        "version {} already exists for {}/{}",
                        req.version, req.brain_id, req.signal_type
                    ),
                )),
            ));
        }
        let result = validator::check_compatibility(&prev_schema.schema_json, &req.schema_json);
        (result.is_compatible, result.violations)
    } else {
        (true, vec![])
    };

    // Reject in strict mode
    if state.config.strict_compatibility && !compatible {
        let prev_v = prev.as_ref().map(|p| p.version);
        db::log_compatibility(
            &state.pool,
            &req.brain_id,
            &req.signal_type,
            prev_v,
            req.version,
            false,
            &serde_json::json!(violations),
        )
        .await
        .map_err(db_err)?;

        return Err((
            StatusCode::UNPROCESSABLE_ENTITY,
            Json(ErrorResponse::with_details(
                "schema incompatible",
                violations.join("; "),
            )),
        ));
    }

    // Store schema
    let schema_id = db::insert_schema(
        &state.pool,
        &req.brain_id,
        &req.signal_type,
        req.version,
        &req.schema_json,
        req.description.as_deref().unwrap_or(""),
        req.registered_by.as_deref().unwrap_or(""),
    )
    .await
    .map_err(db_err)?;

    // Log compatibility result
    let prev_v = prev.as_ref().map(|p| p.version);
    db::log_compatibility(
        &state.pool,
        &req.brain_id,
        &req.signal_type,
        prev_v,
        req.version,
        compatible,
        &serde_json::json!(violations),
    )
    .await
    .map_err(db_err)?;

    info!(
        brain       = %req.brain_id,
        signal_type = %req.signal_type,
        version     = req.version,
        compatible,
        "schema registered"
    );

    Ok(Json(RegisterSchemaResponse {
        schema_id,
        brain_id: req.brain_id,
        signal_type: req.signal_type,
        version: req.version,
        compatible,
        violations,
    }))
}

// ── GET /schema/:brain_id/:signal_type ────────────────────────────────────────

#[derive(Deserialize)]
pub struct VersionQuery {
    pub version: Option<i32>,
}

/// Get a schema. Defaults to latest version.
pub async fn get_schema(
    State(state): State_,
    Path((brain_id, signal_type)): Path<(String, String)>,
    Query(q): Query<VersionQuery>,
) -> ApiResult<SchemaRecord> {
    match db::get_schema(&state.pool, &brain_id, &signal_type, q.version)
        .await
        .map_err(db_err)?
    {
        None => Err((
            StatusCode::NOT_FOUND,
            Json(ErrorResponse::new(format!(
                "schema not found: {}/{}",
                brain_id, signal_type
            ))),
        )),
        Some(s) => Ok(Json(s)),
    }
}

// ── GET /schema/:brain_id/:signal_type/versions ───────────────────────────────

/// List all versions of a schema.
pub async fn list_schema_versions(
    State(state): State_,
    Path((brain_id, signal_type)): Path<(String, String)>,
) -> ApiResult<serde_json::Value> {
    let versions = db::get_all_versions(&state.pool, &brain_id, &signal_type)
        .await
        .map_err(db_err)?;
    Ok(Json(serde_json::json!({
        "brain_id":    brain_id,
        "signal_type": signal_type,
        "versions":    versions,
    })))
}

// ── POST /schema/validate ─────────────────────────────────────────────────────

/// Validate a payload against its schema.
/// Brains call this before emitting signals to AethyrRank.
pub async fn validate_payload(
    State(state): State_,
    Json(req): Json<ValidateRequest>,
) -> ApiResult<ValidateResponse> {
    let schema = db::get_schema(&state.pool, &req.brain_id, &req.signal_type, req.version)
        .await
        .map_err(db_err)?
        .ok_or_else(|| {
            (
                StatusCode::NOT_FOUND,
                Json(ErrorResponse::new(format!(
                    "schema not found: {}/{}",
                    req.brain_id, req.signal_type
                ))),
            )
        })?;

    let result = validator::validate_payload(&schema.schema_json, &req.payload);
    Ok(Json(ValidateResponse {
        valid: result.is_compatible,
        violations: result.violations,
    }))
}

// ── POST /schema/compatibility ────────────────────────────────────────────────

/// Check if a proposed new schema is compatible with the current latest.
/// Call this before POST /schema/register to preview the result.
#[derive(Deserialize)]
pub struct CompatibilityCheckRequest {
    pub brain_id: String,
    pub signal_type: String,
    pub new_schema: serde_json::Value,
}

pub async fn check_compatibility(
    State(state): State_,
    Json(req): Json<CompatibilityCheckRequest>,
) -> ApiResult<CompatibilityResponse> {
    let current = db::get_schema(&state.pool, &req.brain_id, &req.signal_type, None)
        .await
        .map_err(db_err)?;

    let (compatible, violations, from_v, to_v) = match &current {
        None => (true, vec![], None, 1),
        Some(s) => {
            let result = validator::check_compatibility(&s.schema_json, &req.new_schema);
            (
                result.is_compatible,
                result.violations,
                Some(s.version),
                s.version + 1,
            )
        }
    };

    Ok(Json(CompatibilityResponse {
        brain_id: req.brain_id,
        signal_type: req.signal_type,
        from_version: from_v,
        to_version: to_v,
        compatible,
        violations,
    }))
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health(State(state): State_) -> Json<serde_json::Value> {
    let db_ok = sqlx::query("SELECT 1").execute(&state.pool).await.is_ok();
    Json(serde_json::json!({
        "status":  if db_ok { "ok" } else { "degraded" },
        "service": "aethyr-schema-registry",
        "version": env!("CARGO_PKG_VERSION"),
        "db":      if db_ok { "connected" } else { "error" },
        "ts":      Utc::now().to_rfc3339(),
    }))
}
