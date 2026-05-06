/// Metrics stub — wire to your Prometheus / OpenTelemetry exporter.
///
/// Each counter/histogram listed here corresponds to a Grafana panel in the
/// recommended AethyrRank dashboard. Replace the `println!` stubs with your
/// actual metrics client (e.g. `metrics` crate with `metrics-exporter-prometheus`).
///
/// Recommended Prometheus metric names (kept here as string constants so they
/// are easy to grep and rename together):

pub const METRIC_RANK_LATENCY: &str = "aethyrrank_rank_latency_ms";
pub const METRIC_LINUCB_SCORE_MEAN: &str = "aethyrrank_linucb_score_mean";
pub const METRIC_AESQ_SCORE_MEAN: &str = "aethyrrank_aesq_score_mean";
pub const METRIC_EXPLORATION_INJECTED: &str = "aethyrrank_exploration_injected_total";
pub const METRIC_FALLBACK_TRIGGERED: &str = "aethyrrank_fallback_triggered_total";
pub const METRIC_FEEDBACK_EVENTS: &str = "aethyrrank_feedback_events_total";
pub const METRIC_COLD_START_REQUESTS: &str = "aethyrrank_cold_start_requests_total";

/// Record a completed /rank call.
pub fn record_rank(latency_ms: u64, aesq_mean: f64, linucb_mean: f64, surface: &str) {
    // TODO: replace with metrics::histogram!(METRIC_RANK_LATENCY, latency_ms as f64, "surface" => surface.to_string());
    let _ = (latency_ms, aesq_mean, linucb_mean, surface);
}

/// Record a fallback event (AethyrRank unavailable or errored).
pub fn record_fallback(surface: &str) {
    // TODO: replace with metrics::counter!(METRIC_FALLBACK_TRIGGERED, 1, "surface" => surface.to_string());
    let _ = surface;
}

/// Record exploration slot injections.
pub fn record_exploration(count: usize, surface: &str) {
    // TODO: replace with metrics::counter!(METRIC_EXPLORATION_INJECTED, count as u64, "surface" => surface.to_string());
    let _ = (count, surface);
}

/// Record incoming feedback events by type.
pub fn record_feedback(event_type: &str, surface: &str) {
    // TODO: replace with metrics::counter!(METRIC_FEEDBACK_EVENTS, 1, "event_type" => event_type, "surface" => surface.to_string());
    let _ = (event_type, surface);
}
