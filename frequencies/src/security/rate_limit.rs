//! Server-side rate limits (§31), counted in Redis so every node sees the same
//! count.
//!
//! Fixed windows: INCR a key, EXPIRE it on first increment, refuse above the
//! limit. Cheap, node-independent, and honest about its edge (a burst can
//! straddle two windows and reach 2× the limit for one window). That is
//! acceptable for these limits, whose job is to make abuse expensive, not to
//! shape fair traffic.

use std::time::Duration;

/// One limit: how many, per how long.
#[derive(Debug, Clone, Copy)]
pub struct Limit {
    pub scope: &'static str,
    pub max: u32,
    pub window: Duration,
}

pub const CREATE: Limit = Limit {
    scope: "create",
    max: 10,
    window: Duration::from_secs(3600),
};
pub const START: Limit = Limit {
    scope: "start",
    max: 20,
    window: Duration::from_secs(3600),
};
pub const JOIN: Limit = Limit {
    scope: "join",
    max: 60,
    window: Duration::from_secs(60),
};
pub const HEARTBEAT: Limit = Limit {
    scope: "heartbeat",
    max: 30,
    window: Duration::from_secs(60),
};
pub const REQUEST_MIC: Limit = Limit {
    scope: "request_mic",
    max: 6,
    window: Duration::from_secs(60),
};
pub const UPVOTE: Limit = Limit {
    scope: "upvote",
    max: 30,
    window: Duration::from_secs(60),
};
pub const MODERATION: Limit = Limit {
    scope: "moderation",
    max: 120,
    window: Duration::from_secs(60),
};
pub const ROLE_TOGGLE: Limit = Limit {
    scope: "role_toggle",
    max: 30,
    window: Duration::from_secs(60),
};

/// Pure decision: given the count after increment, is the request allowed?
pub fn allowed(count_after_increment: i64, limit: Limit) -> bool {
    count_after_increment <= limit.max as i64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_limit_is_inclusive_and_the_next_is_refused() {
        assert!(allowed(1, REQUEST_MIC));
        assert!(allowed(REQUEST_MIC.max as i64, REQUEST_MIC));
        assert!(!allowed(REQUEST_MIC.max as i64 + 1, REQUEST_MIC));
    }

    #[test]
    fn every_scope_is_unique() {
        let scopes = [
            CREATE,
            START,
            JOIN,
            HEARTBEAT,
            REQUEST_MIC,
            UPVOTE,
            MODERATION,
            ROLE_TOGGLE,
        ]
        .map(|l| l.scope);
        let mut seen = std::collections::HashSet::new();
        for s in scopes {
            assert!(seen.insert(s), "duplicate rate-limit scope {s}");
        }
    }
}
