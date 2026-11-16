//! Service-key authentication: the same constant-time X-Service-Key check
//! transcoding uses (transcoding/src/handler.rs). Only internal services reach
//! caeor; no route but /health and /metrics answers without the key.

use axum::extract::{Request, State};
use axum::http::StatusCode;
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};

use crate::api::AppState;

fn same(a: &str, b: &str) -> bool {
    a.len() == b.len() && a.bytes().zip(b.bytes()).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

pub async fn require_service_key(State(st): State<AppState>, req: Request, next: Next) -> Response {
    let path = req.uri().path();
    if path == "/health" || path == "/metrics" {
        return next.run(req).await;
    }
    let key = &st.cfg.service_key;
    let presented = req
        .headers()
        .get("X-Service-Key")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if key.is_empty() || !same(presented, key) {
        return (StatusCode::UNAUTHORIZED, "service key required").into_response();
    }
    next.run(req).await
}
