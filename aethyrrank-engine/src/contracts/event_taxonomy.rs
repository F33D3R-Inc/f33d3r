//! Event Taxonomy — the single source of truth for all F33D3R event types.
//!
//! ## THE RULE
//!
//! Every EventType in the entire F33D3R system is defined here.
//! Every brain imports from this module.
//! Nobody defines their own EventType anywhere else.
//! Adding a new event = add it here, recompile all brains.
//! The compiler enforces exhaustive match — no silent gaps.
//!
//! ## Adding a new event type
//!
//! 1. Add the variant to `EventType`
//! 2. Add its reward signal to `reward_signal()`
//! 3. Add its category to `event_category()`
//! 4. The compiler will catch every match statement that needs updating
//!
//! This is how you scale to 11 brain regions without compile breaks.

use serde::{Deserialize, Serialize};

/// Every event that any F33D3R brain can emit or receive.
/// Organised by brain region for readability; all are equal at runtime.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EventType {
    // ── Feed / Social (Nantar) ────────────────────────────────────────────
    Impression,
    Click,
    Like,
    Share,
    Comment,
    Save,
    Skip,
    NegativeFeedback,
    ViewComplete,
    Repost,
    Bookmark,
    Follow,
    Unfollow,

    // ── Commerce (Thessalon) ──────────────────────────────────────────────
    Purchase,
    CartAdd,
    CartRemove,
    Checkout,
    Refund,
    ProductView,
    WishlistAdd,

    // ── Music / Audio (Zior) ─────────────────────────────────────────────
    AudioPlay,
    AudioSkip,
    AudioComplete,
    AudioReplay,
    AudioShare,
    AudioSave,
    AudioDownload,

    // ── Streaming / Live (Caeor) ──────────────────────────────────────────
    StreamJoin,
    StreamLeave,
    StreamDonate,
    StreamSubscribe,
    StreamClip,
    StreamShare,

    // ── Voice Rooms (Loxion) ──────────────────────────────────────────────
    VoiceJoin,
    VoiceLeave,
    VoiceSpeak,
    VoiceReact,

    // ── Video (Astraon) ───────────────────────────────────────────────────
    VideoWatch,
    VideoComplete,
    VideoSkip,
    VideoLike,
    VideoShare,
    VideoComment,
    VideoSave,

    // ── Wallet / Payments (Ain Soph) ──────────────────────────────────────
    Tip,
    Withdraw,
    Deposit,
    Subscription,
    PaymentFail,

    // ── Messaging (Vovin) ─────────────────────────────────────────────────
    MessageSent,
    MessageRead,
    MessageReaction,

    // ── Security / Audit (Elohim Veni) ────────────────────────────────────
    LoginSuccess,
    LoginFail,
    ContentReport,
    ContentFlag,
    AccountSuspend,
}

/// Canonical reward signal for every event type.
///
/// This is the single source of truth for LinUCB reward signals.
/// All brain regions that do bandit updates call this function.
/// Range: [-1.0, 2.0] — negative for bad signals, positive for good.
pub fn reward_signal(event: &EventType) -> f64 {
    match event {
        // Strongest positive — money and deep commitment
        EventType::Purchase | EventType::Subscription | EventType::StreamSubscribe => 2.0,

        // Very strong positive — saves and replays signal deep resonance
        EventType::Save
        | EventType::Bookmark
        | EventType::AudioSave
        | EventType::VideoSave
        | EventType::WishlistAdd
        | EventType::AudioReplay => 1.0,

        // Strong positive — sharing is high-value social proof
        EventType::Share
        | EventType::AudioShare
        | EventType::VideoShare
        | EventType::StreamShare
        | EventType::StreamClip => 0.9,

        // Good positive — comments and donations require intent
        EventType::Comment
        | EventType::VideoComment
        | EventType::StreamDonate
        | EventType::Tip
        | EventType::VoiceSpeak => 0.8,

        // Positive — likes and follows are meaningful
        EventType::Like | EventType::VideoLike | EventType::Follow | EventType::VoiceReact => 0.6,

        // Moderate positive — completions signal quality
        EventType::ViewComplete | EventType::AudioComplete | EventType::VideoComplete => 0.5,

        // Mild positive — clicks show interest
        EventType::Click
        | EventType::AudioPlay
        | EventType::VideoWatch
        | EventType::StreamJoin
        | EventType::VoiceJoin
        | EventType::ProductView => 0.3,

        // Weak positive — mere exposure
        EventType::Impression | EventType::MessageRead => 0.1,

        // Neutral — system events, not preference signals
        EventType::Deposit
        | EventType::Withdraw
        | EventType::MessageSent
        | EventType::MessageReaction
        | EventType::AudioDownload
        | EventType::LoginSuccess
        | EventType::CartAdd
        | EventType::Checkout
        | EventType::Repost => 0.2,

        // Negative — disengagement signals
        EventType::Skip
        | EventType::AudioSkip
        | EventType::VideoSkip
        | EventType::StreamLeave
        | EventType::VoiceLeave
        | EventType::CartRemove
        | EventType::Unfollow => -0.2,

        // Strong negative — explicit rejection
        EventType::NegativeFeedback | EventType::Refund => -1.0,

        // Security events — not ranking signals, handled by Elohim Veni
        EventType::LoginFail
        | EventType::ContentReport
        | EventType::ContentFlag
        | EventType::AccountSuspend
        | EventType::PaymentFail => 0.0,
    }
}

/// Which brain region primarily emits this event type.
/// Used by the BrainRegistry to route signals correctly.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EventCategory {
    Feed,
    Commerce,
    Music,
    Streaming,
    VoiceRoom,
    Video,
    Wallet,
    Messaging,
    Security,
    Platform,
}

pub fn event_category(event: &EventType) -> EventCategory {
    match event {
        EventType::Impression
        | EventType::Click
        | EventType::Like
        | EventType::Share
        | EventType::Comment
        | EventType::Save
        | EventType::Skip
        | EventType::NegativeFeedback
        | EventType::ViewComplete
        | EventType::Repost
        | EventType::Bookmark
        | EventType::Follow
        | EventType::Unfollow => EventCategory::Feed,

        EventType::Purchase
        | EventType::CartAdd
        | EventType::CartRemove
        | EventType::Checkout
        | EventType::Refund
        | EventType::ProductView
        | EventType::WishlistAdd => EventCategory::Commerce,

        EventType::AudioPlay
        | EventType::AudioSkip
        | EventType::AudioComplete
        | EventType::AudioReplay
        | EventType::AudioShare
        | EventType::AudioSave
        | EventType::AudioDownload => EventCategory::Music,

        EventType::StreamJoin
        | EventType::StreamLeave
        | EventType::StreamDonate
        | EventType::StreamSubscribe
        | EventType::StreamClip
        | EventType::StreamShare => EventCategory::Streaming,

        EventType::VoiceJoin
        | EventType::VoiceLeave
        | EventType::VoiceSpeak
        | EventType::VoiceReact => EventCategory::VoiceRoom,

        EventType::VideoWatch
        | EventType::VideoComplete
        | EventType::VideoSkip
        | EventType::VideoLike
        | EventType::VideoShare
        | EventType::VideoComment
        | EventType::VideoSave => EventCategory::Video,

        EventType::Tip
        | EventType::Withdraw
        | EventType::Deposit
        | EventType::Subscription
        | EventType::PaymentFail => EventCategory::Wallet,

        EventType::MessageSent | EventType::MessageRead | EventType::MessageReaction => {
            EventCategory::Messaging
        }

        EventType::LoginSuccess
        | EventType::LoginFail
        | EventType::ContentReport
        | EventType::ContentFlag
        | EventType::AccountSuspend => EventCategory::Security,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn purchase_has_highest_reward() {
        assert_eq!(reward_signal(&EventType::Purchase), 2.0);
    }

    #[test]
    fn negative_feedback_has_most_negative_reward() {
        assert_eq!(reward_signal(&EventType::NegativeFeedback), -1.0);
    }

    #[test]
    fn skip_is_negative() {
        assert!(reward_signal(&EventType::Skip) < 0.0);
    }

    #[test]
    fn save_beats_like() {
        assert!(reward_signal(&EventType::Save) > reward_signal(&EventType::Like));
    }

    #[test]
    fn share_beats_click() {
        assert!(reward_signal(&EventType::Share) > reward_signal(&EventType::Click));
    }

    #[test]
    fn security_events_are_zero_reward() {
        assert_eq!(reward_signal(&EventType::ContentReport), 0.0);
        assert_eq!(reward_signal(&EventType::LoginFail), 0.0);
        assert_eq!(reward_signal(&EventType::AccountSuspend), 0.0);
    }

    #[test]
    fn audio_events_categorised_as_music() {
        assert_eq!(event_category(&EventType::AudioPlay), EventCategory::Music);
        assert_eq!(
            event_category(&EventType::AudioComplete),
            EventCategory::Music
        );
    }

    #[test]
    fn serialises_to_snake_case() {
        let json = serde_json::to_string(&EventType::NegativeFeedback).unwrap();
        assert_eq!(json, r#""negative_feedback""#);
    }
}
