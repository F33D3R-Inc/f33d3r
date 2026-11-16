//! Shared state, the error shape every route answers with, and the router.
//!
//! Every route except /health and /metrics requires X-Internal-Key (auth.rs).
//! Abraxas has no public surface: caeor, feed-engine and the admin Facets
//! feed-engine renders are its only callers.

use std::sync::Arc;

use axum::extract::{DefaultBodyLimit, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use manhattan_client::Manhattan;
use serde_json::json;
use sqlx::PgPool;
use tokio::sync::Notify;

use crate::config::Config;

#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    pub cfg: Arc<Config>,
    pub worker: crate::worker::Worker,
    pub chroma: crate::banned::ChromaRegistry,
    pub legacy: crate::ownership::LegacyAudio,
    pub suppression: crate::review::Suppression,
    pub jobs_wake: Arc<Notify>,
    pub manhattan: Manhattan,
}

/// `{error, detail}` with a status, the shape the worker uses too.
#[derive(Debug)]
pub struct ApiError {
    pub status: StatusCode,
    pub code: &'static str,
    pub detail: String,
}

impl ApiError {
    pub fn new(status: StatusCode, code: &'static str, detail: impl Into<String>) -> Self {
        Self { status, code, detail: detail.into() }
    }
    pub fn bad_request(detail: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, "bad_request", detail)
    }
    pub fn not_found(detail: impl Into<String>) -> Self {
        Self::new(StatusCode::NOT_FOUND, "not_found", detail)
    }
    pub fn conflict(detail: impl Into<String>) -> Self {
        Self::new(StatusCode::CONFLICT, "conflict", detail)
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        if self.status.is_server_error() {
            tracing::error!(code = self.code, detail = %self.detail, "request failed");
        }
        (self.status, Json(json!({ "error": self.code, "detail": self.detail }))).into_response()
    }
}

/// A database failure is a refusal to answer, never an answer: callers of a
/// safety gate treat 503 as "not judged".
impl From<sqlx::Error> for ApiError {
    fn from(e: sqlx::Error) -> Self {
        ApiError::new(StatusCode::SERVICE_UNAVAILABLE, "database_unavailable", e.to_string())
    }
}

pub type ApiResult<T> = Result<T, ApiError>;

pub async fn health(State(st): State<AppState>) -> Response {
    let report = async {
        let version = crate::migrate::schema_version(&st.pool).await.map_err(|e| e.to_string())?;
        let (outbox_pending, outbox_quarantined) = crate::outbox::backlog(&st.pool).await.map_err(|e| e.to_string())?;
        let jobs = crate::jobs::backlog(&st.pool).await.map_err(|e| e.to_string())?;
        let (commands_pending, commands_blocked) = crate::enforcement::backlog(&st.pool).await.map_err(|e| e.to_string())?;
        let review: Vec<(String, i64)> = sqlx::query_as(
            "SELECT stage, COUNT(*) FROM review_queue WHERE stage <> 'decided' GROUP BY stage ORDER BY stage",
        )
        .fetch_all(&st.pool)
        .await
        .map_err(|e| e.to_string())?;
        Ok::<_, String>(json!({
            "status": "ok",
            "service": "abraxas-shield",
            "schema_version": version,
            "safety_outbox": { "pending": outbox_pending, "quarantined": outbox_quarantined },
            "identity_commands": { "pending": commands_pending, "blocked": commands_blocked },
            "jobs_pending": jobs.into_iter().collect::<std::collections::BTreeMap<_, _>>(),
            "review_open": review.into_iter().collect::<std::collections::BTreeMap<_, _>>(),
            "manhattan": st.manhattan.configured(),
        }))
    }
    .await;
    match report {
        Ok(body) => (StatusCode::OK, Json(body)).into_response(),
        Err(e) => (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({ "status": "degraded", "service": "abraxas-shield", "error": e })),
        )
            .into_response(),
    }
}

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/metrics", get(crate::observ::metrics_handler))
        .route("/health", get(health))
        // The synchronous gate (caeor, feed-engine's live lane).
        .route("/v1/check", post(crate::check::check_asset))
        .route("/v1/check/frame", post(crate::check::check_frame))
        .route("/v1/assets/:asset_id/verdict", get(crate::check::asset_verdict))
        // Banned registry.
        .route("/v1/banned", get(crate::admin::list_banned).post(crate::admin::add_banned))
        .route("/v1/banned/from-asset", post(crate::admin::ban_from_asset))
        .route("/v1/banned/revoke", post(crate::admin::revoke_banned_value))
        .route("/v1/banned/:id/revoke", post(crate::admin::revoke_banned))
        // Review queue.
        .route("/v1/review", get(crate::review::list))
        .route("/v1/review/:id", get(crate::review::get))
        .route("/v1/review/:id/decide", post(crate::review::decide))
        // Ownership, by PIAL.
        .route("/v1/ownership/pial/:pial", get(crate::admin::ownership_of_pial))
        // Verdicts, for subjects the admin Facets render.
        .route("/v1/verdicts/:kind/:id", get(crate::admin::subject_verdicts))
        // Reports (absorbed from zodacare).
        .route("/v1/stats", get(crate::reports::stats))
        .route("/v1/reports", post(crate::reports::create).get(crate::reports::list))
        .route("/v1/reports/:id", get(crate::reports::get))
        .route("/v1/reports/:id/resolve", post(crate::reports::resolve))
        .route("/v1/reports/:id/escalate", post(crate::reports::escalate))
        .layer(DefaultBodyLimit::max(1 << 20))
        .layer(axum::middleware::from_fn_with_state(state.clone(), crate::auth::require_internal_key))
        .with_state(state)
        .layer(axum::middleware::from_fn(crate::observ::http_middleware))
}
