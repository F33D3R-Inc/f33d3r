//! What every route shares: the typed refusal, and the internal-key guard.

use axum::{
    extract::{Request, State},
    http::StatusCode,
    middleware::Next,
    response::{IntoResponse, Response},
    Json,
};
use serde_json::json;
use subtle::ConstantTimeEq;

use crate::handlers::AppState;

/// A refusal the caller can act on: an HTTP status, a stable machine code and a
/// sentence. The body is always `{"error": code, "message": message}`, so no
/// caller ever has to match English to learn what happened.
#[derive(Debug)]
pub struct ApiError {
    pub status: StatusCode,
    pub code: &'static str,
    pub message: String,
}

pub type ApiResult<T> = Result<T, ApiError>;

impl ApiError {
    pub fn new(status: StatusCode, code: &'static str, message: impl Into<String>) -> Self {
        Self {
            status,
            code,
            message: message.into(),
        }
    }
    pub fn bad_request(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, code, message)
    }
    pub fn not_found(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(StatusCode::NOT_FOUND, code, message)
    }
    pub fn conflict(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(StatusCode::CONFLICT, code, message)
    }
    pub fn forbidden(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(StatusCode::FORBIDDEN, code, message)
    }
    pub fn unauthorized(code: &'static str, message: impl Into<String>) -> Self {
        Self::new(StatusCode::UNAUTHORIZED, code, message)
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (
            self.status,
            Json(json!({ "error": self.code, "message": self.message })),
        )
            .into_response()
    }
}

/// A failure that is not the caller's: logged with its cause, answered as 500.
/// The cause stays in the log; the wire carries no internals.
impl From<anyhow::Error> for ApiError {
    fn from(e: anyhow::Error) -> Self {
        tracing::error!(error = ?e, "internal error");
        Self::new(
            StatusCode::INTERNAL_SERVER_ERROR,
            "internal",
            "The identity authority could not complete the request.",
        )
    }
}

impl From<sqlx::Error> for ApiError {
    fn from(e: sqlx::Error) -> Self {
        Self::from(anyhow::Error::new(e))
    }
}

/// Routes any caller may reach without the internal key: the container
/// healthcheck, the Prometheus scrape (which cannot send headers), and the
/// public verification keys, which exist to be fetched by anyone.
const OPEN_PATHS: &[&str] = &["/health", "/metrics", "/v1/auth/jwks"];

/// Every other route requires `X-Internal-Key`, compared in constant time. A
/// missing or wrong key is one answer, 401, so the guard says nothing about
/// which it was.
pub async fn require_internal_key(
    State(state): State<AppState>,
    req: Request,
    next: Next,
) -> Response {
    if OPEN_PATHS.contains(&req.uri().path()) {
        return next.run(req).await;
    }
    let presented = req
        .headers()
        .get("x-internal-key")
        .map(|v| v.as_bytes())
        .unwrap_or_default();
    let expected = state.internal_api_key.as_bytes();
    // ct_eq on slices of different lengths is false without comparing bytes;
    // only the length is observable, and the key's length is not the secret.
    if expected.is_empty() || !bool::from(presented.ct_eq(expected)) {
        return ApiError::unauthorized("unauthorized", "A valid X-Internal-Key is required.")
            .into_response();
    }
    next.run(req).await
}
