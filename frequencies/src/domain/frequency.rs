//! The durable Frequency row and its enumerations.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// Where a Frequency is in its life. The wire spelling is the database
/// spelling; both are snake_case and neither is ever renamed.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, sqlx::Type)]
#[serde(rename_all = "snake_case")]
#[sqlx(type_name = "TEXT", rename_all = "snake_case")]
pub enum FrequencyState {
    Draft,
    Scheduled,
    Starting,
    Live,
    Ending,
    Ended,
    ProcessingReplay,
    Archived,
    Cancelled,
    Failed,
    ModerationTerminated,
}

impl FrequencyState {
    /// Every state, for exhaustive tests and the schema writer.
    #[cfg_attr(not(test), allow(dead_code))]
    pub const ALL: [FrequencyState; 11] = [
        Self::Draft,
        Self::Scheduled,
        Self::Starting,
        Self::Live,
        Self::Ending,
        Self::Ended,
        Self::ProcessingReplay,
        Self::Archived,
        Self::Cancelled,
        Self::Failed,
        Self::ModerationTerminated,
    ];

    pub fn as_str(self) -> &'static str {
        match self {
            Self::Draft => "draft",
            Self::Scheduled => "scheduled",
            Self::Starting => "starting",
            Self::Live => "live",
            Self::Ending => "ending",
            Self::Ended => "ended",
            Self::ProcessingReplay => "processing_replay",
            Self::Archived => "archived",
            Self::Cancelled => "cancelled",
            Self::Failed => "failed",
            Self::ModerationTerminated => "moderation_terminated",
        }
    }

    #[cfg_attr(not(test), allow(dead_code))]
    pub fn parse(s: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|v| v.as_str() == s)
    }

    /// A session exists or is being set up or torn down: the one-open-per-host
    /// index covers exactly these.
    pub fn is_open(self) -> bool {
        matches!(self, Self::Starting | Self::Live | Self::Ending)
    }

    /// Nothing further can happen to this object except archival bookkeeping.
    pub fn is_over(self) -> bool {
        matches!(
            self,
            Self::Ended
                | Self::ProcessingReplay
                | Self::Archived
                | Self::Cancelled
                | Self::Failed
                | Self::ModerationTerminated
        )
    }

    /// Listeners may Tune In only while live. `starting` is the host's own
    /// window; `ending` refuses new joins by definition (§50).
    pub fn accepts_joins(self) -> bool {
        self == Self::Live
    }

    /// Whether this is a terminal state that can never be left. Pinned by
    /// the lifecycle tests; the transition table is what enforces it.
    #[cfg_attr(not(test), allow(dead_code))]
    pub fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::Archived | Self::Cancelled | Self::Failed | Self::ModerationTerminated
        )
    }
}

impl std::fmt::Display for FrequencyState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, sqlx::Type)]
#[serde(rename_all = "snake_case")]
#[sqlx(type_name = "TEXT", rename_all = "snake_case")]
pub enum Visibility {
    Public,
    Followers,
    Subscribers,
    Private,
}

impl Visibility {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Public => "public",
            Self::Followers => "followers",
            Self::Subscribers => "subscribers",
            Self::Private => "private",
        }
    }

    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "public" => Some(Self::Public),
            "followers" => Some(Self::Followers),
            "subscribers" => Some(Self::Subscribers),
            "private" => Some(Self::Private),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, sqlx::Type)]
#[serde(rename_all = "snake_case")]
#[sqlx(type_name = "TEXT", rename_all = "snake_case")]
pub enum ReplayStatus {
    None,
    Processing,
    Ready,
    Failed,
}

/// A Frequency as stored. Field names are column names.
#[derive(Debug, Clone, Serialize, sqlx::FromRow)]
pub struct Frequency {
    pub id: Uuid,
    pub version: i64,
    pub host_pial: String,
    pub title: String,
    pub description: String,
    pub state: FrequencyState,
    pub visibility: Visibility,
    pub language: String,
    pub adult_content: bool,
    pub speaker_verity_min_tier: i16,
    pub scheduled_at: Option<DateTime<Utc>>,
    pub started_at: Option<DateTime<Utc>>,
    pub ended_at: Option<DateTime<Utc>>,
    pub end_reason: Option<String>,
    pub recording_enabled: bool,
    pub replay_status: ReplayStatus,
    pub max_speakers: i32,
    pub max_listeners: i32,
    pub requests_open: bool,
    pub locked: bool,
    pub media_node: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

/// Limits every caller-supplied field is held to. Enforced here and by CHECK
/// constraints; the two agree, and the constraint is the one that cannot be
/// bypassed.
pub const TITLE_MAX_CHARS: usize = 120;
pub const DESCRIPTION_MAX_CHARS: usize = 600;
pub const REASON_MAX_CHARS: usize = 140;
pub const MAX_SPEAKERS_CAP: i32 = 50;
pub const MAX_LISTENERS_CAP: i32 = 100_000;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_state_round_trips_through_its_string() {
        for s in FrequencyState::ALL {
            assert_eq!(FrequencyState::parse(s.as_str()), Some(s));
        }
        assert_eq!(FrequencyState::parse("space"), None);
    }

    #[test]
    fn only_live_accepts_joins() {
        for s in FrequencyState::ALL {
            assert_eq!(s.accepts_joins(), s == FrequencyState::Live, "{s}");
        }
    }

    #[test]
    fn open_states_match_the_partial_index() {
        let open: Vec<_> = FrequencyState::ALL
            .into_iter()
            .filter(|s| s.is_open())
            .collect();
        assert_eq!(
            open,
            vec![
                FrequencyState::Starting,
                FrequencyState::Live,
                FrequencyState::Ending
            ]
        );
    }
}
