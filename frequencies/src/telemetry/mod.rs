//! Metrics the directive requires (§34), named once here.
//!
//! The HTTP metrics (`http_requests_total`, `http_request_duration_seconds`)
//! come from observ.rs, the shared module, and are not repeated. Everything
//! Frequency-specific is prefixed `frequency_` / `frequencies_`, the way the
//! video lane prefixes `live_`, and registered with fixed label sets so a hot
//! loop can never mint a new series.

pub mod metrics;
