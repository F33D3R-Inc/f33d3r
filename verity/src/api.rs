//! The error shapes every verity route answers with.
//!
//! A 4xx names what was wrong with the request. A 5xx says only that verity
//! failed: the cause is logged here, with the context that identifies the
//! request, and never sent to the caller or swallowed.

use axum::{http::StatusCode, response::Json};
use serde_json::{json, Value};
use tracing::error;

pub type ApiErr = (StatusCode, Json<Value>);

pub fn bad_request(msg: &str) -> ApiErr {
    (
        StatusCode::BAD_REQUEST,
        Json(json!({ "error": "bad_request", "message": msg })),
    )
}

pub fn unprocessable(msg: &str) -> ApiErr {
    (
        StatusCode::UNPROCESSABLE_ENTITY,
        Json(json!({ "error": "invalid", "message": msg })),
    )
}

/// The 500 answer, for a caller that has already logged the cause.
pub fn internal_plain() -> ApiErr {
    (
        StatusCode::INTERNAL_SERVER_ERROR,
        Json(json!({ "error": "internal" })),
    )
}

/// Logs a failed step with what it was and which record it concerned, then
/// answers 500.
pub fn internal(step: &str, subject: impl std::fmt::Display, e: impl std::fmt::Display) -> ApiErr {
    error!(step, subject = %subject, error = %e, "verity step failed");
    internal_plain()
}
