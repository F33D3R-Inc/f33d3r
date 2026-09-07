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

use crate::db;
use crate::identity::{self, ResolveError};
use crate::models::{
    ActionResponse, CreateReportRequest, ModerationActionRequest, ReportsQuery,
    ResolveReportRequest, StatsResponse,
};
use crate::outbox;
use manhattan_client::Manhattan;

// ── Shared application state ──────────────────────────────────────────────────

#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    pub elohim_client: reqwest::Client,
    pub elohim_url: String,
    /// The naming plane. Zodacare owns no identities; it resolves every person
    /// it acts on through here rather than joining into another brain's rows.
    pub manhattan: Manhattan,
}

// ── Identity resolution failures, as HTTP ─────────────────────────────────────

/// Turn a failed resolution into a refusal. There is no lenient branch: a
/// moderation action against an identity we could not verify is the failure
/// this whole migration exists to prevent.
fn identity_refusal(e: ResolveError) -> axum::response::Response {
    let status = match e {
        ResolveError::Malformed(_) => StatusCode::BAD_REQUEST,
        ResolveError::Unresolvable(_) => StatusCode::NOT_FOUND,
        ResolveError::NotAnIdentity { .. } => StatusCode::CONFLICT,
        ResolveError::NoPial(_) => StatusCode::CONFLICT,
        ResolveError::PlaneUnavailable(_) => StatusCode::SERVICE_UNAVAILABLE,
    };
    warn!(error = %e, "identity could not be resolved; refusing");
    (status, Json(json!({ "error": e.to_string() }))).into_response()
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health(State(state): State<AppState>) -> impl IntoResponse {
    let configured = state.manhattan.configured();
    match outbox::pending_count(&state.pool).await {
        Ok(pending) => (
            StatusCode::OK,
            Json(json!({
                "status":                   "ok",
                "service":                  "zodacare",
                "manhattan":                configured,
                "manhattan_outbox_pending": pending,
            })),
        )
            .into_response(),
        // A health check that cannot read its own outbox does not get to claim
        // health. The backlog is the only signal that registrations have stopped
        // reaching the plane, since every local write still succeeds without it.
        Err(e) => {
            warn!(error = %e, "outbox backlog unreadable");
            (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(json!({
                    "status":    "degraded",
                    "service":   "zodacare",
                    "manhattan": configured,
                    "error":     e.to_string(),
                })),
            )
                .into_response()
        }
    }
}

// ── GET /v1/stats ─────────────────────────────────────────────────────────────

pub async fn stats(State(state): State<AppState>) -> impl IntoResponse {
    let pool = &state.pool;
    let total_reports = db::count_total_reports(pool).await.unwrap_or(0);
    let pending_reports = db::count_pending_reports(pool).await.unwrap_or(0);
    let actioned_today = db::count_actioned_today(pool).await.unwrap_or(0);
    let banned_users = db::count_banned_users(pool).await.unwrap_or(0);
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
        )
            .into_response();
    }

    let content_type = req.content_type.as_deref().unwrap_or("post");

    // Both parties, resolved to their PIAL in one round trip before a single
    // row is written. Whatever spelling the caller used — uuid, pial:, @handle
    // — what lands in content_reports is the identity root, so the two columns
    // that spell this concept differently can no longer disagree about it.
    let inputs: Vec<&str> = [
        req.reporter_pial_id.as_deref(),
        req.reported_pial_id.as_deref(),
    ]
    .into_iter()
    .flatten()
    .collect();

    let resolved = match identity::resolve_all(&state.manhattan, &inputs).await {
        Ok(r) => r,
        Err(e) => return identity_refusal(e),
    };
    // Order is preserved and one PIAL was returned per supplied input, so this
    // walk is total — no index arithmetic, no unwrap.
    let mut next = resolved.into_iter();
    let reporter_pial_id = req.reporter_pial_id.as_ref().and_then(|_| next.next());
    let reported_pial_id = req.reported_pial_id.as_ref().and_then(|_| next.next());

    match db::insert_report(
        &state.pool,
        reporter_pial_id,
        reported_pial_id,
        &req.content_id,
        content_type,
        &req.reason,
        req.detail.as_deref(),
    )
    .await
    {
        Ok(id) => {
            info!(report_id = %id, content_id = %req.content_id, reason = %req.reason, "report created");
            (
                StatusCode::CREATED,
                Json(json!({
                    "id":      id,
                    "status":  "pending",
                    "created": true
                })),
            )
                .into_response()
        }
        Err(e) => {
            warn!(error = %e, "failed to insert report");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── GET /v1/reports ───────────────────────────────────────────────────────────

pub async fn list_reports(
    State(state): State<AppState>,
    Query(q): Query<ReportsQuery>,
) -> impl IntoResponse {
    let limit = q.limit.unwrap_or(50).min(200);
    let offset = q.offset.unwrap_or(0);

    match db::list_reports(&state.pool, q.status.as_deref(), limit, offset).await {
        Ok(reports) => (
            StatusCode::OK,
            Json(json!({ "reports": reports, "count": reports.len() })),
        )
            .into_response(),
        Err(e) => {
            warn!(error = %e, "list_reports failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── GET /v1/reports/:id ───────────────────────────────────────────────────────

pub async fn get_report(State(state): State<AppState>, Path(id): Path<Uuid>) -> impl IntoResponse {
    match db::get_report(&state.pool, id).await {
        Ok(Some(r)) => (StatusCode::OK, Json(r)).into_response(),
        Ok(None) => (StatusCode::NOT_FOUND, Json(json!({ "error": "not_found" }))).into_response(),
        Err(e) => {
            warn!(error = %e, "get_report failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
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
        )
            .into_response();
    }

    match db::resolve_report(&state.pool, id, &req.status, req.resolved_by.as_deref()).await {
        Ok(true) => (
            StatusCode::OK,
            Json(json!({ "id": id, "status": req.status })),
        )
            .into_response(),
        Ok(false) => (
            StatusCode::NOT_FOUND,
            Json(json!({ "error": "report not found or already resolved" })),
        )
            .into_response(),
        Err(e) => {
            warn!(error = %e, "resolve_report failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── POST /v1/reports/:id/escalate ────────────────────────────────────────────

pub async fn escalate_report(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
) -> impl IntoResponse {
    match db::escalate_report(&state.pool, id).await {
        Ok(true) => (
            StatusCode::OK,
            Json(json!({ "id": id, "status": "reviewing", "escalated": true })),
        )
            .into_response(),
        Ok(false) => (StatusCode::NOT_FOUND, Json(json!({ "error": "not_found" }))).into_response(),
        Err(e) => {
            warn!(error = %e, "escalate failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── GET /v1/user/:pial_id/risk ────────────────────────────────────────────────

/// `:ident` is a NAME, not a row key: a bare PIAL uuid, `pial:<uuid>`, `@handle`
/// or `handle:<h>`. It is resolved to the identity root before the risk profile
/// is read, so a handle that has since moved reads its current owner's profile
/// — or, if the name was revoked, reads nothing at all.
pub async fn get_user_risk(
    State(state): State<AppState>,
    Path(ident): Path<String>,
) -> impl IntoResponse {
    let pial_id = match identity::resolve_one(&state.manhattan, &ident).await {
        Ok(p) => p,
        Err(e) => return identity_refusal(e),
    };

    match db::get_risk_profile(&state.pool, pial_id).await {
        Ok(Some(profile)) => (StatusCode::OK, Json(profile)).into_response(),
        Ok(None) => (
            StatusCode::OK,
            Json(json!({
                "pial_id":         pial_id,
                "risk_score":      0.0,
                "report_count":    0,
                "violation_count": 0,
                "last_action":     null,
                "status":          "active"
            })),
        )
            .into_response(),
        Err(e) => {
            warn!(error = %e, "get_risk_profile failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── POST /v1/user/:pial_id/action ─────────────────────────────────────────────

/// `:ident` is a NAME. Enforcement is the sharpest place this matters: a ban
/// keyed on a stale handle copy silences whoever holds that handle now rather
/// than whoever earned the ban. Resolving first — and refusing outright when the
/// name cannot be resolved — makes that outcome unreachable.
pub async fn apply_user_action(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    Json(req): Json<ModerationActionRequest>,
) -> impl IntoResponse {
    let valid_types = ["warn", "restrict", "ban", "unban"];
    if !valid_types.contains(&req.action_type.as_str()) {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "error": "action_type must be warn | restrict | ban | unban" })),
        )
            .into_response();
    }

    // Resolved before anything is written and before Elohim Veni is told, so
    // the local record and the enforcement both name the same person.
    let pial_id = match identity::resolve_one(&state.manhattan, &ident).await {
        Ok(p) => p,
        Err(e) => return identity_refusal(e),
    };

    let action_type = req.action_type.clone();
    match db::apply_moderation_action(
        &state.pool,
        pial_id,
        &req.action_type,
        &req.reason,
        req.applied_by.as_deref(),
        req.expires_at,
    )
    .await
    {
        Ok(_action_id) => {
            info!(pial_id = %pial_id, action = %action_type, "moderation action applied");

            // Notify Elohim Veni to enforce capabilities.
            let elohim_notified = notify_elohim_veni(
                &state.elohim_client,
                &state.elohim_url,
                pial_id,
                &action_type,
                &req.reason,
            )
            .await;

            let new_status = match action_type.as_str() {
                "ban" => "banned",
                "restrict" => "restricted",
                "warn" => "warned",
                "unban" => "active",
                _ => "active",
            };

            (
                StatusCode::OK,
                Json(ActionResponse {
                    pial_id,
                    action: action_type,
                    status: new_status.to_string(),
                    elohim_notified,
                }),
            )
                .into_response()
        }
        Err(e) => {
            warn!(error = %e, "apply_moderation_action failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
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
        "ban" => Some("REVOKED"),
        "restrict" => Some("SUSPENDED"),
        "unban" => Some("ACTIVE"),
        _ => None,
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
