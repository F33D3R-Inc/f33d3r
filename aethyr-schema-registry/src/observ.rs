//! F33D3R Rust observability — JSON logs, Prometheus /metrics, request-id
//! propagation. Mirror of feed-engine/internal/observ.
//!
//! This file is identical across every F33D3R Rust brain. Brain name is a
//! runtime parameter to init(), not a const. To update the pattern, edit
//! infra/observability/rust-pattern.md, edit this file to match, and copy it
//! verbatim to every brain — `md5sum */src/observ.rs` must print one hash.
//!
//! Usage in main.rs:
//!
//!     mod observ;
//!     #[tokio::main]
//!     async fn main() -> anyhow::Result<()> {
//!         observ::init("verity")?;
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
    extract::{MatchedPath, Request},
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

/// The metric label for a request that matched no route. One value for all of
/// them, because a path nobody routed is a path anybody can invent — a
/// scanner walking a wordlist would otherwise mint one time series per word.
const UNMATCHED_ROUTE: &str = "/{unmatched}";

/// Per-request middleware. Propagates X-Request-ID, records counters +
/// duration histogram, emits one structured log line per non-noisy request.
///
/// Mount it with `Router::layer` (not `route_layer`), AFTER every route and
/// merge: axum runs a `layer` middleware after routing, which is what puts
/// `MatchedPath` in the request extensions.
pub async fn http_middleware(req: Request<Body>, next: Next) -> Response<Body> {
    let start = Instant::now();
    let method = req.method().clone();
    let path = req.uri().path().to_string();

    // The route PATTERN the request matched (`/v1/resolve/:name`), never the
    // path it was sent with. A metric label must draw from a set fixed at
    // compile time — the router's route table — because Prometheus keeps one
    // time series per distinct value for as long as the process lives. The
    // path itself is unbounded: every name resolved, every address, every
    // Number is a new string, and a pattern-guessing normaliser only ever
    // catches the shapes its author thought of. The raw path still goes to
    // the log line, where a distinct value costs nothing.
    let route = req
        .extensions()
        .get::<MatchedPath>()
        .map(|m| m.as_str().to_string())
        .unwrap_or_else(|| UNMATCHED_ROUTE.to_string());

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
        route = %route,
        path = %path,
        pial_id = %pial,
    );
    let _enter = span.enter();

    let mut req = req;
    if req.headers().get(HEADER_REQUEST_ID).is_none() {
        if let Ok(v) = HeaderValue::from_str(&req_id) {
            req.headers_mut()
                .insert(HeaderName::from_static(HEADER_REQUEST_ID), v);
        }
    }

    let mut response = next.run(req).await;

    let status = response.status().as_u16();
    let dur = start.elapsed().as_secs_f64();

    let labels: [(&str, String); 4] = [
        ("brain", brain().to_string()),
        ("method", method.as_str().to_string()),
        ("path", route.clone()),
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
        response
            .headers_mut()
            .insert(HeaderName::from_static(HEADER_REQUEST_ID), v);
    }
    response
}

fn is_noisy(path: &str) -> bool {
    matches!(path, "/health" | "/metrics") || path.starts_with("/static/")
}
