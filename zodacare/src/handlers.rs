use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};
use serde_json::json;
use sqlx::PgPool;
use tracing::{info, warn};
use uuid::Uuid;

use crate::models::{
    ActionResponse, CreateReportRequest, ModerationActionRequest,
    ReportsQuery, ResolveReportRequest, StatsResponse,
};
use crate::db;

// ── Shared application state ──────────────────────────────────────────────────

#[derive(Clone)]
pub struct AppState {
    pub pool:          PgPool,
    pub elohim_client: reqwest::Client,
    pub elohim_url:    String,
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health() -> impl IntoResponse {
    Json(json!({ "status": "ok", "service": "zodacare" }))
}

// ── GET /v1/stats ─────────────────────────────────────────────────────────────

pub async fn stats(State(state): State<AppState>) -> impl IntoResponse {
    let pool = &state.pool;
    let total_reports   = db::count_total_reports(pool).await.unwrap_or(0);
    let pending_reports = db::count_pending_reports(pool).await.unwrap_or(0);
    let actioned_today  = db::count_actioned_today(pool).await.unwrap_or(0);
    let banned_users    = db::count_banned_users(pool).await.unwrap_or(0);
    Json(StatsResponse {
        service: "zodacare",
        total_reports,
        pending_reports,
        actioned_today,
        banned_users,
    })
}

// ── POST /v1/reports ──────────────────────────────────────────────────────────

pub async fn create_report(
    State(state): State<AppState>,
    Json(req): Json<CreateReportRequest>,
) -> impl IntoResponse {
    if req.content_id.is_empty() || req.reason.is_empty() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "error": "content_id and reason are required" })),
        ).into_response();
    }

    let content_type = req.content_type.as_deref().unwrap_or("post");

    match db::insert_report(
        &state.pool,
        req.reporter_pial_id,
        req.reported_pial_id,
        &req.content_id,
        content_type,
        &req.reason,
        req.detail.as_deref(),
    ).await {
        Ok(id) => {
            info!(report_id = %id, content_id = %req.content_id, reason = %req.reason, "report created");
            (
                StatusCode::CREATED,
                Json(json!({
                    "id":      id,
                    "status":  "pending",
                    "created": true
                })),
            ).into_response()
        }
        Err(e) => {
            warn!(error = %e, "failed to insert report");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            ).into_response()
        }
    }
}

// ── GET /v1/reports ───────────────────────────────────────────────────────────

pub async fn list_reports(
    State(state): State<AppState>,
    Query(q): Query<ReportsQuery>,
) -> impl IntoResponse {
    let limit  = q.limit.unwrap_or(50).min(200);
    let offset = q.offset.unwrap_or(0);

    match db::list_reports(&state.pool, q.status.as_deref(), limit, offset).await {
        Ok(reports) => (StatusCode::OK, Json(json!({ "reports": reports, "count": reports.len() }))).into_response(),
        Err(e) => {
            warn!(error = %e, "list_reports failed");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({ "error": "db_error" }))).into_response()
        }
    }
}

// ── GET /v1/reports/:id ───────────────────────────────────────────────────────

pub async fn get_report(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
) -> impl IntoResponse {
    match db::get_report(&state.pool, id).await {
        Ok(Some(r)) => (StatusCode::OK, Json(r)).into_response(),
        Ok(None)    => (StatusCode::NOT_FOUND, Json(json!({ "error": "not_found" }))).into_response(),
        Err(e) => {
            warn!(error = %e, "get_report failed");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({ "error": "db_error" }))).into_response()
        }
    }
}

// ── POST /v1/reports/:id/resolve ─────────────────────────────────────────────

pub async fn resolve_report(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<ResolveReportRequest>,
) -> impl IntoResponse {
    let valid_statuses = ["actioned", "dismissed"];
    if !valid_statuses.contains(&req.status.as_str()) {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "error": "status must be 'actioned' or 'dismissed'" })),
        ).into_response();
    }

    match db::resolve_report(&state.pool, id, &req.status, req.resolved_by.as_deref()).await {
        Ok(true)  => (StatusCode::OK, Json(json!({ "id": id, "status": req.status }))).into_response(),
        Ok(false) => (StatusCode::NOT_FOUND, Json(json!({ "error": "report not found or already resolved" }))).into_response(),
        Err(e) => {
            warn!(error = %e, "resolve_report failed");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({ "error": "db_error" }))).into_response()
        }
    }
}

// ── POST /v1/reports/:id/escalate ────────────────────────────────────────────

pub async fn escalate_report(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
) -> impl IntoResponse {
    match db::escalate_report(&state.pool, id).await {
        Ok(true)  => (StatusCode::OK, Json(json!({ "id": id, "status": "reviewing", "escalated": true }))).into_response(),
        Ok(false) => (StatusCode::NOT_FOUND, Json(json!({ "error": "not_found" }))).into_response(),
        Err(e) => {
            warn!(error = %e, "escalate failed");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({ "error": "db_error" }))).into_response()
        }
    }
}

// ── GET /v1/user/:pial_id/risk ────────────────────────────────────────────────

pub async fn get_user_risk(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
) -> impl IntoResponse {
    match db::get_risk_profile(&state.pool, pial_id).await {
        Ok(Some(profile)) => (StatusCode::OK, Json(profile)).into_response(),
        Ok(None) => (StatusCode::OK, Json(json!({
            "pial_id":         pial_id,
            "risk_score":      0.0,
            "report_count":    0,
            "violation_count": 0,
            "last_action":     null,
            "status":          "active"
        }))).into_response(),
        Err(e) => {
            warn!(error = %e, "get_risk_profile failed");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({ "error": "db_error" }))).into_response()
        }
    }
}

// ── POST /v1/user/:pial_id/action ─────────────────────────────────────────────

pub async fn apply_user_action(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<ModerationActionRequest>,
) -> impl IntoResponse {
    let valid_types = ["warn", "restrict", "ban", "unban"];
    if !valid_types.contains(&req.action_type.as_str()) {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "error": "action_type must be warn | restrict | ban | unban" })),
        ).into_response();
    }

    let action_type = req.action_type.clone();
    match db::apply_moderation_action(
        &state.pool,
        pial_id,
        &req.action_type,
        &req.reason,
        req.applied_by.as_deref(),
        req.expires_at,
    ).await {
        Ok(_action_id) => {
            info!(pial_id = %pial_id, action = %action_type, "moderation action applied");

            // Notify Elohim Veni to enforce capabilities.
            let elohim_notified = notify_elohim_veni(
                &state.elohim_client,
                &state.elohim_url,
                pial_id,
                &action_type,
                &req.reason,
            ).await;

            let new_status = match action_type.as_str() {
                "ban"      => "banned",
                "restrict" => "restricted",
                "warn"     => "warned",
                "unban"    => "active",
                _          => "active",
            };

            (
                StatusCode::OK,
                Json(ActionResponse {
                    pial_id,
                    action: action_type,
                    status: new_status.to_string(),
                    elohim_notified,
                }),
            ).into_response()
        }
        Err(e) => {
            warn!(error = %e, "apply_moderation_action failed");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({ "error": "db_error" }))).into_response()
        }
    }
}

// ── Elohim Veni integration ───────────────────────────────────────────────────

async fn notify_elohim_veni(
    client: &reqwest::Client,
    elohim_url: &str,
    pial_id: Uuid,
    action_type: &str,
    reason: &str,
) -> bool {
    if elohim_url.is_empty() {
        return false;
    }

    // Set PIAL status based on action.
    let pial_status = match action_type {
        "ban"      => Some("REVOKED"),
        "restrict" => Some("SUSPENDED"),
        "unban"    => Some("ACTIVE"),
        _          => None,
    };

    if let Some(status) = pial_status {
        let url = format!("{}/v1/pial/{}/status", elohim_url, pial_id);
        let payload = json!({
            "status":     status,
            "reason":     reason,
            "updated_by": "zodacare"
        });
        match client.post(&url).json(&payload).send().await {
            Ok(r) if r.status().is_success() => {
                info!(pial_id = %pial_id, status = %status, "Elohim Veni PIAL status updated");
            }
            Ok(r) => {
                warn!(pial_id = %pial_id, status = %r.status(), "Elohim Veni returned error for status update");
                return false;
            }
            Err(e) => {
                warn!(error = %e, "failed to notify Elohim Veni");
                return false;
            }
        }
    }

    // For ban: also revoke all capabilities.
    if action_type == "ban" {
        let url = format!("{}/v1/pial/{}/ban", elohim_url, pial_id);
        let payload = json!({ "reason": reason, "applied_by": "zodacare" });
        let _ = client.post(&url).json(&payload).send().await;
    }

    true
}
