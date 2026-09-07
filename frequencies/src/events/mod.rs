//! Frequency events on Sitra Achra.
//!
//! Every mutation writes its event into `frequency_events` in the same
//! transaction (see `schemas::Event` and `repository::postgres::insert_event`);
//! `drain` publishes them, in order, and marks them published. Nothing in this
//! brain calls the producer directly from a request handler.

pub mod drain;
pub mod schemas;

pub use schemas::{Event, EventType};

/// The one topic. Partitioned by frequency_id, so a consumer sees one
/// Frequency's events in order.
pub const TOPIC: &str = "frequency.events";
