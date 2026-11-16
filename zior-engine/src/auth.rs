//! X-Internal-Key on every route but /health and /metrics: only feed-engine
//! calls Zior. Constant-time comparison, as caeor's and AethyrRank's.

use axum::extract::{Request, State};
use axum::http::StatusCode;
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use std::sync::Arc;

use crate::api::handlers::ZiorState;

fn same(a: &str, b: &str) -> bool {
    a.len() == b.len() && a.bytes().zip(b.bytes()).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

pub async fn require_internal_key(
    State(state): State<Arc<ZiorState>>,
    req: Request,
    next: Next,
) -> Response {
    let path = req.uri().path();
    if path == "/health" || path == "/metrics" {
        return next.run(req).await;
    }
    let presented = req
        .headers()
        .get("X-Internal-Key")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if state.internal_api_key.is_empty() || !same(presented, &state.internal_api_key) {
        return (StatusCode::UNAUTHORIZED, "internal key required").into_response();
    }
    next.run(req).await
}
