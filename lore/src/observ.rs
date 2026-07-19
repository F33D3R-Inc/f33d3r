//! F33D3R Rust observability — JSON logs, Prometheus /metrics, request-id propagation.
//! Identical across every F33D3R Rust brain.

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

pub fn init(brain_name: &str) -> anyhow::Result<()> {
    let _ = BRAIN.set(brain_name.to_string());
    let env = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let dev_mode = std::env::var("DEV_MODE").map(|v| v == "true").unwrap_or(false);
    if dev_mode {
        tracing_subscriber::registry()
            .with(env)
            .with(tracing_subscriber::fmt::layer().with_target(false))
            .try_init().ok();
    } else {
        tracing_subscriber::registry()
            .with(env)
            .with(tracing_subscriber::fmt::layer().json().flatten_event(true).with_target(false).with_current_span(true))
            .try_init().ok();
    }
    let handle = PrometheusBuilder::new().install_recorder()
        .map_err(|e| anyhow::anyhow!("prometheus install: {e}"))?;
    let _ = PROM_HANDLE.set(handle);
    tracing::info!(brain = brain_name, dev_mode, "observ initialised");
    Ok(())
}

pub async fn metrics_handler() -> (StatusCode, String) {
    match PROM_HANDLE.get() {
        Some(h) => (StatusCode::OK, h.render()),
        None => (StatusCode::SERVICE_UNAVAILABLE, "metrics not initialised".into()),
    }
}

pub async fn http_middleware(req: Request<Body>, next: Next) -> Response<Body> {
    let start = Instant::now();
    let method = req.method().clone();
    let path = req.uri().path().to_string();

    let req_id = req.headers().get(HEADER_REQUEST_ID)
        .and_then(|v| v.to_str().ok().map(String::from))
        .unwrap_or_else(|| Uuid::new_v4().to_string());

    let pial = req.headers().get(HEADER_PIAL_IDENTITY)
        .and_then(|v| v.to_str().ok().map(String::from))
        .unwrap_or_default();

    let span = tracing::info_span!("http_request", brain = brain(), request_id = %req_id, method = %method, path = %path, pial_id = %pial);
    let _enter = span.enter();

    let mut req = req;
    if req.headers().get(HEADER_REQUEST_ID).is_none() {
        if let Ok(v) = HeaderValue::from_str(&req_id) {
            req.headers_mut().insert(HeaderName::from_static(HEADER_REQUEST_ID), v);
        }
    }

    let mut response = next.run(req).await;
    let status = response.status().as_u16();
    let dur = start.elapsed().as_secs_f64();

    metrics::counter!("http_requests_total", "brain" => brain().to_string(), "method" => method.as_str().to_string(), "path" => path.clone(), "status" => status.to_string()).increment(1);
    metrics::histogram!("http_request_duration_seconds", "brain" => brain().to_string(), "method" => method.as_str().to_string(), "path" => path.clone(), "status" => status.to_string()).record(dur);

    if !matches!(path.as_str(), "/health" | "/metrics") {
        let dur_ms = (dur * 1000.0) as u64;
        if status >= 500 { tracing::error!(status, dur_ms, "request"); }
        else if status >= 400 { tracing::warn!(status, dur_ms, "request"); }
        else { tracing::info!(status, dur_ms, "request"); }
    }

    if let Ok(v) = HeaderValue::from_str(&req_id) {
        response.headers_mut().insert(HeaderName::from_static(HEADER_REQUEST_ID), v);
    }
    response
}
