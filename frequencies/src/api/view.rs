//! What Nantar renders from. The authoritative read model of one Frequency:
//! the row, the live counts, who is speaking, the request queue, and — when
//! the caller names a viewer — that viewer's own standing.
//!
//! Identities leave this brain as bare PIAL uuids under `*_pial_id`. Nantar
//! resolves them to public representations; this brain never sees a handle
//! and never emits one.

use chrono::{DateTime, Utc};
use serde::Serialize;
use sqlx::PgConnection;
use uuid::Uuid;

use crate::domain::role::may;
use crate::domain::{
    Action, Frequency, FrequencyRole, FrequencyState, Participant, ReplayStatus, SpeakerRequest,
    Visibility,
};
use crate::error::Result;
use crate::identity::bare_value;
use crate::repository::{postgres as pg, redis::Hot};

#[derive(Debug, Serialize)]
pub struct FrequencySummary {
    pub id: Uuid,
    pub version: i64,
    pub host_pial_id: String,
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

impl From<&Frequency> for FrequencySummary {
    fn from(f: &Frequency) -> Self {
        Self {
            id: f.id,
            version: f.version,
            host_pial_id: bare_value(&f.host_pial).to_string(),
            title: f.title.clone(),
            description: f.description.clone(),
            state: f.state,
            visibility: f.visibility,
            language: f.language.clone(),
            adult_content: f.adult_content,
            speaker_verity_min_tier: f.speaker_verity_min_tier,
            scheduled_at: f.scheduled_at,
            started_at: f.started_at,
            ended_at: f.ended_at,
            end_reason: f.end_reason.clone(),
            recording_enabled: f.recording_enabled,
            replay_status: f.replay_status,
            max_speakers: f.max_speakers,
            max_listeners: f.max_listeners,
            requests_open: f.requests_open,
            locked: f.locked,
            media_node: f.media_node.clone(),
            created_at: f.created_at,
            updated_at: f.updated_at,
        }
    }
}

#[derive(Debug, Serialize)]
pub struct Counts {
    /// Present listeners.
    pub listeners: i64,
    /// Present speaking roles other than the host.
    pub speakers: i64,
    /// Everyone present, host included.
    pub participants: i64,
}

#[derive(Debug, Serialize)]
pub struct ParticipantView {
    pub pial_id: String,
    pub role: FrequencyRole,
    pub muted: bool,
    pub present: bool,
    pub joined_at: DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct RequestView {
    pub id: Uuid,
    pub pial_id: String,
    pub reason: String,
    pub upvotes: i32,
    pub created_at: DateTime<Utc>,
}

impl From<&SpeakerRequest> for RequestView {
    fn from(r: &SpeakerRequest) -> Self {
        Self {
            id: r.id,
            pial_id: bare_value(&r.pial).to_string(),
            reason: r.reason.clone(),
            upvotes: r.upvotes,
            created_at: r.created_at,
        }
    }
}

#[derive(Debug, Serialize)]
pub struct ViewerView {
    pub pial_id: String,
    /// None when the viewer is not in the Frequency.
    pub role: Option<FrequencyRole>,
    pub muted: bool,
    pub present: bool,
    pub blocked: bool,
    pub request: Option<RequestView>,
    /// What the viewer may do, computed here so Nantar renders controls from
    /// the same rule Auralis enforces.
    pub can_request_mic: bool,
    pub can_moderate: bool,
    pub can_end: bool,
    pub can_speak: bool,
    pub can_listen: bool,
}

#[derive(Debug, Serialize)]
pub struct FrequencyView {
    pub frequency: FrequencySummary,
    pub counts: Counts,
    /// Everyone in a speaking role, present or not, host first.
    pub speakers: Vec<ParticipantView>,
    pub cohost_pial_ids: Vec<String>,
    /// Pending requests, best first. Nantar shows this to moderators only.
    pub requests: Vec<RequestView>,
    /// Bare PIALs of everyone currently present — the fan-out list for FA
    /// Live mutations.
    pub present_pial_ids: Vec<String>,
    pub lease_node: Option<String>,
    pub viewer: Option<ViewerView>,
}

/// Build the read model. `viewer` is a canonical `pial:` name or None.
pub async fn build(
    conn: &mut PgConnection,
    hot: &Hot,
    freq: &Frequency,
    viewer: Option<&str>,
) -> Result<FrequencyView> {
    let joined = pg::joined(conn, freq.id).await?;
    let cohosts = pg::cohosts(conn, freq.id).await?;
    let requests = if freq.state.is_open() {
        pg::pending_requests(conn, freq.id).await?
    } else {
        Vec::new()
    };

    let names: Vec<String> = joined.iter().map(|p| p.pial.clone()).collect();
    let alive = if freq.state.is_open() {
        hot.alive(freq.id, &names).await?
    } else {
        vec![false; names.len()]
    };

    let mut speakers = Vec::new();
    let mut present_pial_ids = Vec::new();
    let (mut listeners_now, mut speakers_now) = (0i64, 0i64);
    let mut host_present = false;
    for (p, present) in joined.iter().zip(alive.iter().copied()) {
        if present {
            present_pial_ids.push(bare_value(&p.pial).to_string());
            // The card reads "@host + N speakers · M listening": the host is
            // neither a speaker nor a listener in these counts, and is counted
            // once in `participants`.
            match p.role {
                FrequencyRole::Host => host_present = true,
                r if r.speaks() => speakers_now += 1,
                _ => listeners_now += 1,
            }
        }
        if p.role.speaks() {
            speakers.push(ParticipantView {
                pial_id: bare_value(&p.pial).to_string(),
                role: p.role,
                muted: p.muted,
                present,
                joined_at: p.joined_at,
            });
        }
    }
    speakers.sort_by_key(|s| match s.role {
        FrequencyRole::Host => 0,
        FrequencyRole::CoHost => 1,
        _ => 2,
    });

    let viewer_view = match viewer {
        None => None,
        Some(v) => {
            let me: Option<&Participant> = joined.iter().find(|p| p.pial == v);
            let present = me
                .and_then(|p| joined.iter().position(|q| q.pial == p.pial))
                .and_then(|i| alive.get(i).copied())
                .unwrap_or(false);
            let blocked = pg::is_blocked(conn, freq.id, v).await?;
            let request = pg::pending_request_for(conn, freq.id, v)
                .await?
                .as_ref()
                .map(RequestView::from);
            let role = me.map(|p| p.role);
            Some(ViewerView {
                pial_id: bare_value(v).to_string(),
                role,
                muted: me.map(|p| p.muted).unwrap_or(false),
                present,
                blocked,
                can_request_mic: role.map(|r| may(r, Action::RequestMic)).unwrap_or(false)
                    && freq.requests_open
                    && freq.state == FrequencyState::Live
                    && request.is_none(),
                can_moderate: role
                    .map(|r| may(r, Action::ApproveSpeaker))
                    .unwrap_or(false),
                can_end: role.map(|r| may(r, Action::EndFrequency)).unwrap_or(false),
                can_speak: role.map(|r| may(r, Action::Speak)).unwrap_or(false),
                can_listen: role.map(|r| may(r, Action::Listen)).unwrap_or(false),
                request,
            })
        }
    };

    let lease_node = if freq.state.is_open() {
        hot.lease_holder(freq.id).await?
    } else {
        None
    };

    Ok(FrequencyView {
        frequency: FrequencySummary::from(freq),
        counts: Counts {
            listeners: listeners_now,
            speakers: speakers_now,
            participants: listeners_now + speakers_now + i64::from(host_present),
        },
        speakers,
        cohost_pial_ids: cohosts.iter().map(|c| bare_value(c).to_string()).collect(),
        requests: requests.iter().map(RequestView::from).collect(),
        present_pial_ids,
        lease_node,
        viewer: viewer_view,
    })
}
