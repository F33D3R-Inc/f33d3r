/// Structured logging helpers.
///
/// In production, replace the `tracing` macros with your preferred sink
/// (e.g. JSON → Datadog, OpenTelemetry spans). The fields emitted here
/// are the canonical set expected by downstream dashboards.
///
/// Required fields per request:
///   request_id  — propagated from SessionContext.request_id
///   surface     — feed | explore | search | …
///   latency_ms  — wall-clock milliseconds for /rank
///   pool_size   — number of candidates submitted
///   items_returned — number of items in the response
use tracing::{info, warn};

pub fn log_rank_start(request_id: &str, surface: &str, pool_size: usize) {
    info!(
        request_id = request_id,
        surface = surface,
        pool_size = pool_size,
        "rank.start"
    );
}

pub fn log_rank_end(request_id: &str, latency_ms: u64, items_returned: usize) {
    info!(
        request_id = request_id,
        latency_ms = latency_ms,
        items_returned = items_returned,
        "rank.end"
    );
}

pub fn log_fallback(request_id: &str, reason: &str) {
    warn!(
        request_id = request_id,
        reason = reason,
        "rank.fallback_triggered"
    );
}
