//! The ranking.events contract: one message is one thing a person did that
//! says something about what they want to see.
//!
//! Producers:
//!   * feed-engine — engagement and its undo, follows, views, "not interested",
//!     replies, quotes and deletes, relayed from its ranking_outbox
//!     (feed-engine/internal/db/ranking_outbox.go), keyed by the viewer's PIAL;
//!   * Ain Soph — tips and subscriptions (ain-soph/src/handlers.rs,
//!     emit_payment_signal), keyed by the payer's PIAL.
//!
//! Keying by the viewer's PIAL puts one person's signals on one partition, in
//! order, so an unlike is always applied after the like it takes back.
//!
//! ```json
//! {
//!   "event_id":       "uuid — stable per action; a redelivery is the same event",
//!   "source":         "feed-engine | ain-soph",
//!   "viewer_account": "account uuid (feed-engine signals)",
//!   "viewer_pial":    "PIAL — server-to-server only",
//!   "action":         "like | unlike | repost | unrepost | bookmark | unbookmark |
//!                      dislike | undislike | reply | quote | view | not_interested |
//!                      follow | unfollow | delete | play | complete | skip |
//!                      tip | subscription",
//!   "work_id":        "the work acted on (work actions)",
//!   "target_account": "the account followed or unfollowed",
//!   "creator_pial":   "the creator paid (tip, subscription)",
//!   "occurred_at":    "RFC 3339 — when the person acted",
//!   "received_at":    "RFC 3339 — when the producer recorded it"
//! }
//! ```

use chrono::{DateTime, Utc};
use serde::Deserialize;
use uuid::Uuid;

use crate::contracts::event_taxonomy::EventType;

pub const TOPIC: &str = "ranking.events";
pub const DEAD_TOPIC: &str = "ranking.events.dead";

#[derive(Debug, Deserialize)]
pub struct RankingSignal {
    pub event_id: Uuid,
    pub source: String,
    #[serde(default)]
    pub viewer_account: Option<String>,
    pub viewer_pial: String,
    pub action: Action,
    #[serde(default)]
    pub work_id: Option<String>,
    #[serde(default)]
    pub target_account: Option<String>,
    #[serde(default)]
    pub creator_pial: Option<String>,
    pub occurred_at: DateTime<Utc>,
    pub received_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Action {
    Like,
    Unlike,
    Repost,
    Unrepost,
    Bookmark,
    Unbookmark,
    Dislike,
    Undislike,
    Reply,
    Quote,
    View,
    NotInterested,
    Follow,
    Unfollow,
    Delete,
    Play,
    Complete,
    Skip,
    Tip,
    Subscription,
}

/// What a person's action is, as something to learn from.
pub enum Effect {
    /// A reward. `standing` names a reaction that can later be taken back; its
    /// exact update is stored so the undo can reverse precisely it.
    Learn {
        event: EventType,
        standing: Option<&'static str>,
    },
    /// Taking back the named standing reaction.
    Undo(&'static str),
    /// The work no longer exists to be shown.
    Forget,
}

/// What the context of a signal is looked up by.
pub enum Subject {
    Work,
    Creator,
    PaidCreator,
}

impl Action {
    /// The reward each action carries is the taxonomy's
    /// (contracts::event_taxonomy::reward_signal), never a producer's number.
    pub fn effect(self) -> Effect {
        use Action::*;
        let learn = |event, standing| Effect::Learn { event, standing };
        match self {
            Like => learn(EventType::Like, Some("like")),
            Repost => learn(EventType::Share, Some("repost")),
            Bookmark => learn(EventType::Bookmark, Some("bookmark")),
            Dislike => learn(EventType::NegativeFeedback, Some("dislike")),
            Follow => learn(EventType::Follow, Some("follow")),
            Unlike => Effect::Undo("like"),
            Unrepost => Effect::Undo("repost"),
            Unbookmark => Effect::Undo("bookmark"),
            Undislike => Effect::Undo("dislike"),
            Unfollow => Effect::Undo("follow"),
            Reply => learn(EventType::Comment, None),
            Quote => learn(EventType::Share, None),
            View => learn(EventType::Impression, None),
            NotInterested => learn(EventType::NegativeFeedback, None),
            Play => learn(EventType::AudioPlay, None),
            Complete => learn(EventType::AudioComplete, None),
            Skip => learn(EventType::AudioSkip, None),
            Tip => learn(EventType::Tip, None),
            Subscription => learn(EventType::Subscription, None),
            Delete => Effect::Forget,
        }
    }

    pub fn subject(self) -> Subject {
        match self {
            Action::Follow | Action::Unfollow => Subject::Creator,
            Action::Tip | Action::Subscription => Subject::PaidCreator,
            _ => Subject::Work,
        }
    }
}

impl RankingSignal {
    /// Parse and check that the fields the action needs are present. An error
    /// is permanent: the message can never be applied and goes to the dead topic.
    pub fn parse(raw: &[u8]) -> Result<Self, String> {
        let s: RankingSignal =
            serde_json::from_slice(raw).map_err(|e| format!("not a ranking signal: {e}"))?;
        let present = |v: &Option<String>| v.as_deref().is_some_and(|x| !x.trim().is_empty());
        // A closed set: it labels metrics, and a label must not be producer-minted.
        if !matches!(s.source.as_str(), "feed-engine" | "ain-soph") {
            return Err(format!("unknown source {:?}", s.source));
        }
        if s.viewer_pial.trim().is_empty() {
            return Err("viewer_pial is empty".into());
        }
        match s.action.subject() {
            Subject::Work if !present(&s.work_id) => {
                return Err(format!("{:?} names no work_id", s.action))
            }
            Subject::Creator if !present(&s.target_account) => {
                return Err(format!("{:?} names no target_account", s.action))
            }
            Subject::PaidCreator if !present(&s.creator_pial) => {
                return Err(format!("{:?} names no creator_pial", s.action))
            }
            _ => {}
        }
        if !matches!(s.action.subject(), Subject::PaidCreator)
            && !matches!(s.action, Action::Delete)
            && !present(&s.viewer_account)
        {
            return Err(format!("{:?} names no viewer_account", s.action));
        }
        Ok(s)
    }
}
