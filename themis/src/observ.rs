//! F33D3R Rust observability — JSON logs, Prometheus /metrics, request-id
//! propagation. Mirror of feed-engine/internal/observ.
//!
//! This file is identical across every F33D3R Rust brain. Brain name is a
//! runtime parameter to init(), not a const. To update the pattern, edit
//! infra/observability/rust-pattern.md and copy this file to every brain.
//!
//! Usage in main.rs:
//!
//!     mod observ;
//!     #[tokio::main]
//!     async fn main() -> anyhow::Result<()> {
//!         observ::init("themis")?;
//!         let app = Router::new()
//!             .route("/metrics", get(observ::metrics_handler))
//!             .route("/health", get(health))
//!             // …existing routes…
//!             .layer(axum::middleware::from_fn(observ::http_middleware));
//!         // …serve…
//!     }

use std::time::Instant;

use axum::{
    body::Body,
    extract::Request,
    http::{HeaderName, HeaderValue, StatusCode},
    middleware::Next,
    response::Response,
};
use metrics_exporter_prometheus::{PrometheusBuilder, PrometheusHandle};
use once_cell::sync::OnceCell;
use tracing_subscriber::{layer::SubscriberExt, util::SubscriberInitExt, EnvFilter};
use uuid::Uuid;

const HEADER_REQUEST_ID: &str = "x-request-id";
const HEADER_PIAL_IDENTITY: &str = "x-pial-identity";

static BRAIN: OnceCell<String> = OnceCell::new();
static PROM_HANDLE: OnceCell<PrometheusHandle> = OnceCell::new();

fn brain() -> &'static str {
    BRAIN.get().map(String::as_str).unwrap_or("unknown")
}

/// Initialise tracing + metrics. Call once from main(), before serving traffic.
pub fn init(brain_name: &str) -> anyhow::Result<()> {
    let _ = BRAIN.set(brain_name.to_string());

    let env = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let dev_mode = std::env::var("DEV_MODE")
        .map(|v| v == "true")
        .unwrap_or(false);

    if dev_mode {
        tracing_subscriber::registry()
            .with(env)
            .with(tracing_subscriber::fmt::layer().with_target(false))
            .try_init()
            .ok();
    } else {
        tracing_subscriber::registry()
            .with(env)
            .with(
                tracing_subscriber::fmt::layer()
                    .json()
                    .flatten_event(true)
                    .with_target(false)
                    .with_current_span(true),
            )
            .try_init()
            .ok();
    }

    let handle = PrometheusBuilder::new()
        .install_recorder()
        .map_err(|e| anyhow::anyhow!("prometheus install: {e}"))?;
    let _ = PROM_HANDLE.set(handle);

    tracing::info!(brain = brain_name, dev_mode, "observ initialised");
    Ok(())
}

/// Axum handler for GET /metrics. Mount on the router.
pub async fn metrics_handler() -> (StatusCode, String) {
    match PROM_HANDLE.get() {
        Some(h) => (StatusCode::OK, h.render()),
        None => (
            StatusCode::SERVICE_UNAVAILABLE,
            "metrics not initialised".into(),
        ),
    }
}

/// Per-request middleware. Propagates X-Request-ID, records counters +
/// duration histogram, emits one structured log line per non-noisy request.
pub async fn http_middleware(req: Request<Body>, next: Next) -> Response<Body> {
    let start = Instant::now();
    let method = req.method().clone();
    let path = req.uri().path().to_string();
    let normalised = normalise_path(&path);

    // Request id: propagate inbound, generate if missing.
    let req_id = req
        .headers()
        .get(HEADER_REQUEST_ID)
        .and_then(|v| v.to_str().ok().map(String::from))
        .unwrap_or_else(|| Uuid::new_v4().to_string());

    let pial = req
        .headers()
        .get(HEADER_PIAL_IDENTITY)
        .and_then(|v| v.to_str().ok().map(String::from))
        .unwrap_or_default();

    // Span for this request — every tracing event inherits these fields.
    let span = tracing::info_span!(
        "http_request",
        brain = brain(),
        request_id = %req_id,
        method = %method,
        path = %normalised,
        pial_id = %pial,
    );
    let _enter = span.enter();

    let mut req = req;
    if req.headers().get(HEADER_REQUEST_ID).is_none() {
        if let Ok(v) = HeaderValue::from_str(&req_id) {
            req.headers_mut().insert(
                HeaderName::from_static(HEADER_REQUEST_ID),
                v,
            );
        }
    }

    let mut response = next.run(req).await;

    let status = response.status().as_u16();
    let dur = start.elapsed().as_secs_f64();

    let labels: [(&str, String); 4] = [
        ("brain", brain().to_string()),
        ("method", method.as_str().to_string()),
        ("path", normalised.clone()),
        ("status", status.to_string()),
    ];

    metrics::counter!(
        "http_requests_total",
        "brain" => labels[0].1.clone(),
        "method" => labels[1].1.clone(),
        "path" => labels[2].1.clone(),
        "status" => labels[3].1.clone(),
    )
    .increment(1);

    metrics::histogram!(
        "http_request_duration_seconds",
        "brain" => labels[0].1.clone(),
        "method" => labels[1].1.clone(),
        "path" => labels[2].1.clone(),
        "status" => labels[3].1.clone(),
    )
    .record(dur);

    if !is_noisy(&path) {
        let dur_ms = (dur * 1000.0) as u64;
        if status >= 500 {
            tracing::error!(status, dur_ms, "request");
        } else if status >= 400 {
            tracing::warn!(status, dur_ms, "request");
        } else {
            tracing::info!(status, dur_ms, "request");
        }
    }

    if let Ok(v) = HeaderValue::from_str(&req_id) {
        response.headers_mut().insert(
            HeaderName::from_static(HEADER_REQUEST_ID),
            v,
        );
    }
    response
}

fn is_noisy(path: &str) -> bool {
    matches!(path, "/health" | "/metrics") || path.starts_with("/static/")
}

/// Path normalisation for low-cardinality Prometheus labels.
/// Mirrors feed-engine/internal/observ/normalize.go logic.
fn normalise_path(p: &str) -> String {
    // Strip /v1/{...}/:uuid → /v1/{...}/:uuid (collapse uuid)
    // Pattern: any 8-4-4-4-12 hex segment
    let mut out = String::with_capacity(p.len());
    for seg in p.split('/') {
        if seg.is_empty() {
            out.push('/');
            continue;
        }
        if is_uuid_like(seg) {
            out.push_str(":uuid");
        } else if seg.chars().all(|c| c.is_ascii_digit()) && seg.len() <= 12 {
            out.push_str(":n");
        } else {
            out.push_str(seg);
        }
        out.push('/');
    }
    if out.len() > 1 && out.ends_with('/') {
        out.pop();
    }
    if out.is_empty() {
        "/".into()
    } else {
        out
    }
}

fn is_uuid_like(s: &str) -> bool {
    let bytes = s.as_bytes();
    if bytes.len() != 36 {
        return false;
    }
    for (i, b) in bytes.iter().enumerate() {
        match i {
            8 | 13 | 18 | 23 => {
                if *b != b'-' {
                    return false;
                }
            }
            _ => {
                if !b.is_ascii_hexdigit() {
                    return false;
                }
            }
        }
    }
    true
}
