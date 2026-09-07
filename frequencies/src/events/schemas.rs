//! The event envelope and the closed set of event types.
//!
//! The envelope is events/taxonomy.md's, in full: event_id, event_type,
//! schema_version, pial_id, timestamp — plus frequency_id and correlation_id,
//! which every Frequency event carries. The JSON schemas under
//! events/schemas/frequency.*.json are generated from the same shape and
//! pinned by a test.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use uuid::Uuid;

pub const SCHEMA_VERSION: &str = "1.0";

/// Actor value when the platform, not a person, caused the event.
pub const ACTOR_SERVICE: &str = "service";

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum EventType {
    Created,
    Scheduled,
    Started,
    Ended,
    Cancelled,
    Failed,
    ListenerJoined,
    ListenerLeft,
    SpeakerRequested,
    SpeakerApproved,
    SpeakerDeclined,
    SpeakerJoined,
    SpeakerLeft,
    SpeakerMuted,
    HostChanged,
    CohostAdded,
    CohostRemoved,
    ParticipantRemoved,
    ParticipantBlocked,
    Locked,
    RequestsToggled,
    RecordingStarted,
    RecordingCompleted,
    ReplayReady,
    ModerationTerminated,
}

impl EventType {
    pub const ALL: [EventType; 25] = [
        Self::Created,
        Self::Scheduled,
        Self::Started,
        Self::Ended,
        Self::Cancelled,
        Self::Failed,
        Self::ListenerJoined,
        Self::ListenerLeft,
        Self::SpeakerRequested,
        Self::SpeakerApproved,
        Self::SpeakerDeclined,
        Self::SpeakerJoined,
        Self::SpeakerLeft,
        Self::SpeakerMuted,
        Self::HostChanged,
        Self::CohostAdded,
        Self::CohostRemoved,
        Self::ParticipantRemoved,
        Self::ParticipantBlocked,
        Self::Locked,
        Self::RequestsToggled,
        Self::RecordingStarted,
        Self::RecordingCompleted,
        Self::ReplayReady,
        Self::ModerationTerminated,
    ];

    /// The wire name: `frequency.<action>`. Versioning lives in
    /// `schema_version`, per the taxonomy; the directive's `.v1` suffix is
    /// that field, not part of the type.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Created => "frequency.created",
            Self::Scheduled => "frequency.scheduled",
            Self::Started => "frequency.started",
            Self::Ended => "frequency.ended",
            Self::Cancelled => "frequency.cancelled",
            Self::Failed => "frequency.failed",
            Self::ListenerJoined => "frequency.listener_joined",
            Self::ListenerLeft => "frequency.listener_left",
            Self::SpeakerRequested => "frequency.speaker_requested",
            Self::SpeakerApproved => "frequency.speaker_approved",
            Self::SpeakerDeclined => "frequency.speaker_declined",
            Self::SpeakerJoined => "frequency.speaker_joined",
            Self::SpeakerLeft => "frequency.speaker_left",
            Self::SpeakerMuted => "frequency.speaker_muted",
            Self::HostChanged => "frequency.host_changed",
            Self::CohostAdded => "frequency.cohost_added",
            Self::CohostRemoved => "frequency.cohost_removed",
            Self::ParticipantRemoved => "frequency.participant_removed",
            Self::ParticipantBlocked => "frequency.participant_blocked",
            Self::Locked => "frequency.locked",
            Self::RequestsToggled => "frequency.requests_toggled",
            Self::RecordingStarted => "frequency.recording_started",
            Self::RecordingCompleted => "frequency.recording_completed",
            Self::ReplayReady => "frequency.replay_ready",
            Self::ModerationTerminated => "frequency.moderation_terminated",
        }
    }

    #[cfg_attr(not(test), allow(dead_code))]
    pub fn parse(s: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|t| t.as_str() == s)
    }
}

impl std::fmt::Display for EventType {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// An event as written to the outbox and as published. One struct for both
/// so the row and the wire cannot drift.
#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct Event {
    pub event_id: Uuid,
    pub event_type: String,
    pub schema_version: String,
    /// The actor as a bare PIAL uuid, or "service".
    pub pial_id: String,
    pub timestamp: DateTime<Utc>,
    pub frequency_id: Uuid,
    pub correlation_id: String,
    pub payload: Value,
}

impl Event {
    /// A new event, stamped now. `actor` is the bare PIAL uuid or
    /// [`ACTOR_SERVICE`]; never a handle.
    pub fn new(
        frequency_id: Uuid,
        event_type: EventType,
        actor: impl Into<String>,
        correlation_id: impl Into<String>,
        payload: Value,
    ) -> Self {
        Self {
            event_id: Uuid::new_v4(),
            event_type: event_type.as_str().to_string(),
            schema_version: SCHEMA_VERSION.to_string(),
            pial_id: actor.into(),
            timestamp: Utc::now(),
            frequency_id,
            correlation_id: correlation_id.into(),
            payload,
        }
    }

    pub fn wire(&self) -> Vec<u8> {
        serde_json::to_vec(self).expect("event serializes")
    }
}

/// The JSON Schema for one event type, in the repository's draft-07 house
/// style (events/schemas/track.uploaded.json). `payload` is left open per
/// type because its fields are the type's; the envelope is closed.
pub fn json_schema(t: EventType) -> Value {
    serde_json::json!({
        "$schema": "http://json-schema.org/draft-07/schema#",
        "title": t.as_str(),
        "description": format!("Emitted by Auralis (F33D3R Frequencies) on {}.", t.as_str()),
        "type": "object",
        "required": ["event_id", "event_type", "schema_version", "pial_id", "timestamp", "frequency_id", "correlation_id", "payload"],
        "properties": {
            "event_id":       { "type": "string", "format": "uuid" },
            "event_type":     { "type": "string", "const": t.as_str() },
            "schema_version": { "type": "string", "const": SCHEMA_VERSION },
            "pial_id":        { "type": "string", "description": "Actor PIAL UUID, or \"service\" when the platform acted" },
            "timestamp":      { "type": "string", "format": "date-time" },
            "frequency_id":   { "type": "string", "format": "uuid" },
            "correlation_id": { "type": "string" },
            "payload":        { "type": "object" }
        },
        "additionalProperties": false
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_type_round_trips() {
        for t in EventType::ALL {
            assert_eq!(EventType::parse(t.as_str()), Some(t));
            assert!(t.as_str().starts_with("frequency."));
        }
    }

    #[test]
    fn wire_carries_the_full_envelope() {
        let e = Event::new(
            Uuid::nil(),
            EventType::Started,
            "c0ffee00-0000-4000-8000-000000000001",
            "req-1",
            serde_json::json!({ "title": "t" }),
        );
        let v: Value = serde_json::from_slice(&e.wire()).unwrap();
        for k in [
            "event_id",
            "event_type",
            "schema_version",
            "pial_id",
            "timestamp",
            "frequency_id",
            "correlation_id",
            "payload",
        ] {
            assert!(v.get(k).is_some(), "missing {k}");
        }
        assert_eq!(v["event_type"], "frequency.started");
        assert_eq!(v["schema_version"], "1.0");
    }

    /// The checked-in schema files must match what this brain would generate.
    /// Regenerate with `cargo run --bin auralis -- --write-schemas <dir>` if
    /// this fails after a deliberate change.
    #[test]
    fn checked_in_schemas_match() {
        let dir = concat!(env!("CARGO_MANIFEST_DIR"), "/../events/schemas");
        for t in EventType::ALL {
            let path = format!("{dir}/{}.json", t.as_str());
            let on_disk = std::fs::read_to_string(&path)
                .unwrap_or_else(|e| panic!("{path}: {e} — run with --write-schemas"));
            let on_disk: Value = serde_json::from_str(&on_disk).unwrap();
            assert_eq!(on_disk, json_schema(t), "{path} differs from json_schema()");
        }
    }
}
