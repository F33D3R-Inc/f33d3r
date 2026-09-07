//! Request Mic: a listener asks, with a reason, and a moderator decides.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, sqlx::Type)]
#[serde(rename_all = "snake_case")]
#[sqlx(type_name = "TEXT", rename_all = "snake_case")]
pub enum RequestStatus {
    Pending,
    Approved,
    Declined,
    Withdrawn,
    Expired,
}

impl RequestStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Pending => "pending",
            Self::Approved => "approved",
            Self::Declined => "declined",
            Self::Withdrawn => "withdrawn",
            Self::Expired => "expired",
        }
    }

    /// Only a pending request can be resolved, and it can be resolved once.
    /// Two moderators pressing Accept at the same instant: the first wins the
    /// row, the second finds nothing to accept. The SQL in
    /// `repository::postgres::resolve_request` (`WHERE status = 'pending'`)
    /// enforces it; this is the rule stated once so a test can pin it.
    #[cfg_attr(not(test), allow(dead_code))]
    pub fn can_resolve_to(self, to: RequestStatus) -> bool {
        self == Self::Pending && to != Self::Pending
    }
}

#[derive(Debug, Clone, Serialize, sqlx::FromRow)]
pub struct SpeakerRequest {
    pub id: Uuid,
    pub frequency_id: Uuid,
    pub pial: String,
    pub reason: String,
    pub status: RequestStatus,
    pub upvotes: i32,
    pub created_at: DateTime<Utc>,
    pub resolved_at: Option<DateTime<Utc>>,
    pub resolved_by: Option<String>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use RequestStatus::*;

    #[test]
    fn a_request_is_resolved_exactly_once() {
        for to in [Approved, Declined, Withdrawn, Expired] {
            assert!(Pending.can_resolve_to(to));
        }
        for from in [Approved, Declined, Withdrawn, Expired] {
            for to in [Pending, Approved, Declined, Withdrawn, Expired] {
                assert!(!from.can_resolve_to(to), "{from:?} -> {to:?}");
            }
        }
        assert!(!Pending.can_resolve_to(Pending));
    }
}
