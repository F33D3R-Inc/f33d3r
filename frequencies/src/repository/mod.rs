//! Storage. PostgreSQL holds what must survive; Redis holds what is true only
//! while a session is running.
//!
//! Redis disappearing must not destroy a Frequency. After a Redis restart the
//! durable object is intact in PostgreSQL, the live sets are empty, and the
//! next heartbeat from each participant repopulates them. After a PostgreSQL
//! outage nothing is served until it is back — there is no cache to answer
//! from, by design.

pub mod postgres;
pub mod redis;
