//! The Manhattan outbox drain — one crate, used by every Rust brain.
//!
//! Every brain that publishes to the naming plane does so through a
//! transactional outbox: triggers write `manhattan_outbox` rows in the same
//! transaction as the row that caused them, and this drain delivers them. It
//! used to be a file copied into ten brains, and the ten copies drifted; a
//! wire-contract change that reaches some brains and misses others is exactly
//! the failure one crate makes impossible. Same argument, same remedy, as
//! `manhattan-client`.
//!
//! # What it does
//!
//! Reads the queue in id order, sends a batch to `POST /v1/apply` as the
//! triggers wrote it, and acts on one verdict per row:
//!
//!   applied, already — delivered.
//!   refused          — quarantined at once; the queue drains past it.
//!   retry            — held at the head, backed off, the attempt counted;
//!                      after `MAX_ATTEMPTS` such answers it is quarantined.
//!   error            — held at the head, the attempt NOT counted: the plane,
//!                      not the row, failed.
//!   skipped          — an earlier row held; untouched.
//!
//! It no longer decides what a 409 means. Whether a replayed bind is already
//! true, whether a refused edge is nonetheless present, whether retracting a
//! pair that cannot exist is settled — Manhattan answers those beside the
//! row, and this drain applies outcomes.
//!
//! # Three properties
//!
//!   Order. Later rows depend on earlier ones — a node before its edges — so
//!   the queue is delivered in id order and the first row that must hold its
//!   place holds everything behind it.
//!
//!   Durability. A row is marked delivered only after Manhattan has said so.
//!   A plane that cannot be reached, or faults, holds the head without
//!   counting: an outage must never quarantine the write that was first in
//!   line when it began.
//!
//!   One drain at a time. Two replicas draining one queue would interleave it.
//!   A session advisory lock makes the second replica do nothing this tick.
//!
//! # Its own connections, the brain's sqlx
//!
//! The drain opens a small pool of its own from the brain's `DATABASE_URL`
//! rather than borrowing the brain's, so a brain's `main` hands it a URL and
//! nothing else. It is compiled against whichever sqlx the brain is on: the
//! brains are split between sqlx 0.7 and 0.8, and the two cannot share a
//! binary (each carries its own libsqlite3-sys, and only one crate may link
//! the native library). So the whole drain lives in `drain.rs`, and two thin
//! crates compile it — this one against 0.8, `manhattan-outbox-sqlx07` against
//! 0.7 — both exporting the library name `manhattan_outbox`. A brain depends on
//! the one matching its sqlx and the code it calls is the same either way.
//! When the last brain reaches 0.8, delete the 0.7 crate.

mod drain;
pub use drain::*;
