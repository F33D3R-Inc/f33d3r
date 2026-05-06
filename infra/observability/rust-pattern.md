# Rust observability pattern — F33D3R Sprint 0 / S0.2

This document is the **canonical recipe** for instrumenting a Rust brain with
the same `/metrics` + structured logs + request-ID propagation as Nantar.

It is intentionally mechanical — apply it identically to every Rust brain.
When you finish, the Prometheus scrape config in `prometheus.yml` will turn
that brain's target from `down` to `up` automatically.

The 13 Rust brains to instrument (in suggested order):

1. **Verity** — cleanest code base, easiest first apply.
2. **Elohim Veni** — same shape as Verity.
3. **Zodacare**
4. **Schema Registry**
5. **Registrar**
6. **Ain Soph**
7. **Aethyr Ledger**
8. **Thessalon**
9. **Caeor**
10. **Vovin** (aethyr-msg)
11. **Zior** (zior-engine)
12. **AethyrRank** (aethyrrank-engine)
13. **eKYC** — Python; see python-pattern.md (separate doc).

---

## Step 1 — `Cargo.toml` additions

In each brain's `Cargo.toml`, add to `[dependencies]`:

```toml
tracing               = "0.1"
tracing-subscriber    = { version = "0.3", features = ["env-filter", "json", "fmt"] }
metrics               = "0.23"
metrics-exporter-prometheus = "0.15"
metrics-util          = "0.17"
uuid                  = { version = "1", features = ["v4"] }
tower-http            = { version = "0.6", features = ["trace", "request-id", "set-header"] }
```

`uuid` is likely already present. `tower-http` may already be present too —
just add the missing features.

---

## Step 2 — Drop in `src/observ.rs`

Create this file in every Rust brain. Copy verbatim. Only the `BRAIN` constant
changes per brain.

```rust
//! F33D3R observability — JSON logs, /metrics, request-id propagation.
//!
//! Mirror of feed-engine/internal/observ in Go.
//! Mounted in main.rs via observ::init() + observ::router_layer().

use std::time::Instant;

use axum::{
    body::Body,
    extract::MatchedPath,
    http::{HeaderName, HeaderValue, Request, Response},
    middleware::{self, Next},
    response::IntoResponse,
};
use metrics_exporter_prometheus::{PrometheusBuilder, PrometheusHandle};
use tracing_subscriber::{
    fmt,
    layer::SubscriberExt,
    util::SubscriberInitExt,
    EnvFilter,
};
use uuid::Uuid;

/// Set this constant per brain. Used in every log line + every metric label.
pub const BRAIN: &str = "verity"; // ← change per brain

pub const HEADER_REQUEST_ID: HeaderName = HeaderName::from_static("x-request-id");
pub const HEADER_PIAL_IDENTITY: HeaderName = HeaderName::from_static("x-pial-identity");

static PROM_HANDLE: once_cell::sync::OnceCell<PrometheusHandle> = once_cell::sync::OnceCell::new();

/// Initialise tracing + metrics. Call from main() before serving traffic.
pub fn init() -> anyhow::Result<()> {
    let env = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let dev_mode = std::env::var("DEV_MODE").map(|v| v == "true").unwrap_or(false);

    if dev_mode {
        tracing_subscriber::registry()
            .with(env)
            .with(fmt::layer().with_target(false))
            .init();
    } else {
        tracing_subscriber::registry()
            .with(env)
            .with(fmt::layer().json().flatten_event(true).with_target(false))
            .init();
    }

    let handle = PrometheusBuilder::new()
        .install_recorder()
        .map_err(|e| anyhow::anyhow!("prometheus install: {e}"))?;
    PROM_HANDLE.set(handle).ok();

    tracing::info!(brain = BRAIN, dev_mode, "observ initialised");
    Ok(())
}

/// Axum handler for GET /metrics — mount on the router.
pub async fn metrics_handler() -> impl IntoResponse {
    match PROM_HANDLE.get() {
        Some(h) => (axum::http::StatusCode::OK, h.render()),
        None => (
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "metrics not initialised".to_string(),
        ),
    }
}

/// Per-request middleware. Generates/propagates request-id, sets it on the
/// response, records request count + duration histogram.
pub async fn middleware(req: Request<Body>, next: Next) -> Response<Body> {
    let start = Instant::now();
    let method = req.method().clone();
    let path_template = req
        .extensions()
        .get::<MatchedPath>()
        .map(|p| p.as_str().to_string())
        .unwrap_or_else(|| req.uri().path().to_string());

    // Request id
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

    let span = tracing::info_span!(
        "http_request",
        brain = BRAIN,
        request_id = %req_id,
        method = %method,
        path = %path_template,
        pial_id = %pial,
    );
    let _enter = span.enter();

    let mut req = req;
    req.headers_mut().insert(
        HEADER_REQUEST_ID,
        HeaderValue::from_str(&req_id).unwrap_or(HeaderValue::from_static("invalid")),
    );

    let response = next.run(req).await;

    let status = response.status().as_u16();
    let dur = start.elapsed().as_secs_f64();

    metrics::counter!(
        "http_requests_total",
        "brain" => BRAIN,
        "method" => method.as_str().to_string(),
        "path" => path_template.clone(),
        "status" => status.to_string(),
    )
    .increment(1);

    metrics::histogram!(
        "http_request_duration_seconds",
        "brain" => BRAIN,
        "method" => method.as_str().to_string(),
        "path" => path_template,
        "status" => status.to_string(),
    )
    .record(dur);

    if status >= 500 {
        tracing::error!(status, dur_ms = (dur * 1000.0) as u64, "request failed");
    } else {
        tracing::info!(status, dur_ms = (dur * 1000.0) as u64, "request");
    }

    let mut response = response;
    response.headers_mut().insert(
        HEADER_REQUEST_ID,
        HeaderValue::from_str(&req_id).unwrap_or(HeaderValue::from_static("invalid")),
    );
    response
}

/// Helper to attach the middleware as a layer, used in main.rs:
/// `let app = router.layer(observ::router_layer());`
pub fn router_layer() -> axum::middleware::FromFnLayer<...> {
    middleware::from_fn(middleware)
}
```

(The exact `FromFnLayer<...>` signature varies by axum version — copy the
working form from Verity once it's the first brain to land. Subsequent
brains use the same signature.)

---

## Step 3 — wire into `main.rs`

```rust
mod observ;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init()?;
    // …existing setup…

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(health))
        // …existing routes…
        .layer(axum::middleware::from_fn(observ::middleware));

    // …existing server bind/serve…
}
```

Order matters: `.layer(observ::middleware)` should be the outermost so every
route gets request-id + metrics, **except** `/metrics` which we want to keep
out of its own histograms. Use `.route_layer` instead of `.layer` to scope
the middleware to feature routes only — pattern lifted from Verity once it
lands.

---

## Step 4 — propagate to outbound HTTP calls

Every brain that makes outbound HTTP (Nantar → Verity, Thessalon → Ain Soph,
etc.) needs to forward the request-id header on outbound requests:

```rust
use reqwest::header::HeaderMap;

fn correlation_headers(req_id: &str, pial: &str) -> HeaderMap {
    let mut h = HeaderMap::new();
    if !req_id.is_empty() {
        h.insert("x-request-id", req_id.parse().unwrap());
    }
    if !pial.is_empty() {
        h.insert("x-pial-identity", pial.parse().unwrap());
    }
    h
}

// usage:
client
    .post(&url)
    .headers(correlation_headers(req_id, pial))
    .json(&body)
    .send()
    .await?
```

A reusable wrapper struct is welcome but not required at T0.

---

## Step 5 — Kafka header propagation

For producers using `rdkafka`:

```rust
use rdkafka::message::{Header, OwnedHeaders};

let headers = OwnedHeaders::new()
    .insert(Header { key: "x-request-id", value: Some(req_id.as_bytes()) })
    .insert(Header { key: "x-pial-identity", value: Some(pial.as_bytes()) });

producer.send(
    FutureRecord::to(topic)
        .key(key)
        .payload(&payload)
        .headers(headers),
    Duration::from_secs(5),
).await?;
```

Consumers extract them and put them on the tracing span:

```rust
let req_id = msg.headers()
    .and_then(|h| h.iter().find(|h| h.key == "x-request-id"))
    .and_then(|h| h.value)
    .and_then(|v| std::str::from_utf8(v).ok())
    .unwrap_or("");
```

---

## Step 6 — verify

After applying to a brain, restart the stack and check:

```bash
# Brain target should be UP in Prometheus:
curl -s "http://localhost:9090/api/v1/query?query=up{brain=\"verity\"}" | jq
# expected: result.value[1] == "1"

# Metrics should be populated:
curl -s "http://localhost:9090/api/v1/query?query=http_requests_total{brain=\"verity\"}" | jq

# Logs should be JSON in Loki:
curl -s -G "http://localhost:3100/loki/api/v1/query_range" \
  --data-urlencode 'query={brain="verity"}' --data-urlencode 'limit=10' | jq
```

If all three return data, the brain is fully instrumented. Move to the next.

---

## Style rules

- **Never log raw PIAL UUIDs at INFO level** — only at DEBUG. PIAL is sensitive.
- **Never log raw message ciphertext** — Vovin metadata only.
- **Never log Verity document hashes or attestation payloads.**
- **Always include `request_id`** on every error log so on-call can correlate.
- **Never increment a counter in a hot loop without a fixed label set** — that's how cardinality explodes.

---

## When all 13 are done

`prometheus.yml` already lists every brain. Once each brain exposes /metrics,
`up{job="<brain>"} == 1` for all of them. The F33D3R Overview dashboard will
populate end-to-end. That's the Sprint 0 / S0.2 acceptance criterion met.
