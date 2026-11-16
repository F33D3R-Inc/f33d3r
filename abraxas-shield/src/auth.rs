//! X-Internal-Key on every route but /health and /metrics, compared in
//! constant time. Abraxas has no unauthenticated surface.

use axum::extract::{Request, State};
use axum::http::StatusCode;
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde_json::json;

use crate::api::AppState;

fn same(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

pub async fn require_internal_key(State(st): State<AppState>, req: Request, next: Next) -> Response {
    let path = req.uri().path();
    if path == "/health" || path == "/metrics" {
        return next.run(req).await;
    }
    let presented = req.headers().get("X-Internal-Key").map(|v| v.as_bytes()).unwrap_or(b"");
    let key = st.cfg.internal_key.as_bytes();
    if key.is_empty() || !same(presented, key) {
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({ "error": "unauthorized", "detail": "X-Internal-Key missing or wrong" })),
        )
            .into_response();
    }
    next.run(req).await
}
