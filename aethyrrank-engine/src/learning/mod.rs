//! How AethyrRank learns: ranking.events in, LinUCB models out.
//!
//!   features — the context each work was ranked in, per viewer
//!   signal   — the ranking.events contract
//!   apply    — one signal, inside a batch transaction
//!   consumer — the one owner of the topic, exactly once
pub mod apply;
pub mod consumer;
pub mod features;
pub mod signal;
