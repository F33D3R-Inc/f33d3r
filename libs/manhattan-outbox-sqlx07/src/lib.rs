//! The Manhattan outbox drain, compiled against sqlx 0.7.
//!
//! This crate has no source of its own: `drain.rs` is the file
//! `manhattan-outbox` compiles against sqlx 0.8, reached by path, so the two
//! crates cannot drift. It exists because the brains are split between sqlx
//! releases and one binary cannot hold both. Delete it when the last brain
//! reaches 0.8. Everything it does is documented in
//! ../manhattan-outbox/src/lib.rs.

#[path = "../../manhattan-outbox/src/drain.rs"]
mod drain;
pub use drain::*;
