//! X-Internal-Key on every route but /health and /metrics.
//!
//! Only feed-engine calls AethyrRank, and a ranked request carries viewer
//! PIALs, interest vectors and safety settings. The key is the shared
//! INTERNAL_API_KEY; the comparison is constant-time, as caeor's is.

use axum::extract::{Request, State};
use axum::http::StatusCode;
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use std::sync::Arc;

use crate::api::handlers::AppState;

pub const HEADER: &str = "X-Internal-Key";

fn same(a: &str, b: &str) -> bool {
    a.len() == b.len() && a.bytes().zip(b.bytes()).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

pub async fn require_internal_key(
    State(state): State<Arc<AppState>>,
    req: Request,
    next: Next,
) -> Response {
    let path = req.uri().path();
    if path == "/health" || path == "/metrics" {
        return next.run(req).await;
    }
    let presented = req
        .headers()
        .get(HEADER)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if state.internal_api_key.is_empty() || !same(presented, &state.internal_api_key) {
        return (StatusCode::UNAUTHORIZED, "internal key required").into_response();
    }
    next.run(req).await
}
