//! Safety barrier: an assertion, not a policy.
//!
//! What a viewer may be shown is decided in exactly one place: feed-engine's
//! `withoutBarrierRefused` (feed-engine/internal/handler/aethyr_rank.go). It
//! removes every adult, NSFW or gore work from the candidate window of any
//! viewer whose safety epsilon is below 1.0 *before* AethyrRank is asked, so
//! the feed shows the same thing whether or not the ranker answers.
//!
//! feed-engine marks such a work with `adult_probability = 1.0` and every
//! other work with `0.0` (feed-engine/internal/aethyr/content.go). The
//! invariant the authority guarantees is therefore:
//!
//! ```text
//!     safety_epsilon < 1.0  ⇒  every item in the pool has adult_probability = 0
//! ```
//!
//! AethyrRank does not re-decide that. It checks the invariant, and a request
//! that breaks it is refused whole: the error is logged at ERROR level and
//! counted, and feed-engine serves its own already-filtered window instead. A
//! violation means the single authority has a bug, which is an incident, never
//! something to quietly repair here — repairing it here would make this a
//! second policy that the authority could drift away from unnoticed.

use crate::api::types::{ContentItem, UserState};

/// Metric counting requests refused because they broke the barrier invariant.
pub const METRIC_BARRIER_VIOLATIONS: &str = "aethyrrank_barrier_violations_total";

/// The first item that the authority should have refused for this viewer, if any.
pub fn first_violation<'a>(user: &UserState, pool: &'a [ContentItem]) -> Option<&'a ContentItem> {
    if user.safety_epsilon >= 1.0 {
        return None;
    }
    pool.iter().find(|c| c.adult_probability > 0.0)
}
