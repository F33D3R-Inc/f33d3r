//! The audio plane, LiveKit, kept in step with this brain.
//!
//! Auralis decides who may speak and LiveKit carries the sound. A browser gets a
//! short-lived token for its current standing, and every commit wakes a pass that
//! compares the room in LiveKit with the participants in PostgreSQL and corrects
//! any difference. A periodic sweep runs the same pass, so a failed call or a change
//! made by a background task converges within seconds.
//!
//! A muted speaker holds no publish permission at all. Revoking it makes LiveKit
//! drop their track, so a mute holds even against a modified browser.

use std::collections::{HashMap, HashSet};
use std::time::Duration;

use anyhow::{anyhow, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
use chrono::Utc;
use hmac::{Hmac, Mac};
use reqwest::StatusCode;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use tokio::sync::mpsc;
use tokio::task::JoinSet;
use tracing::{info, warn};
use uuid::Uuid;

use crate::domain::{FrequencyRole, FrequencyState, Participant};
use crate::identity::bare_value;
use crate::replay;
use crate::repository::postgres as pg;
use crate::state::AppState;

/// Long enough to join with; a connected client is refreshed by LiveKit itself.
const JOIN_TOKEN_TTL_SECS: i64 = 600;
const ADMIN_TOKEN_TTL_SECS: i64 = 60;
const CALL_TIMEOUT: Duration = Duration::from_secs(5);
const SWEEP_EVERY: Duration = Duration::from_secs(5);
/// A live room with nobody connected stays open; the Frequency's own lifecycle ends it.
const ROOM_EMPTY_TIMEOUT_SECS: u32 = 86_400;
const MIN_SECRET_LEN: usize = 32;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Grant {
    pub publish: bool,
    /// Listeners are invisible to the room, so a large audience costs nothing per join.
    pub hidden: bool,
}

impl Grant {
    pub fn of(role: FrequencyRole, muted: bool) -> Self {
        Self {
            publish: role.speaks() && !muted,
            hidden: !role.speaks(),
        }
    }

    pub fn for_participant(p: &Participant) -> Self {
        Self::of(p.role, p.muted || p.moderator_muted)
    }
}

/// The name a person goes by in one room's audio. Opaque, and different per room,
/// so LiveKit and its logs never hold a PIAL.
pub fn identity(room: Uuid, pial: &str) -> String {
    let mut h = Sha256::new();
    h.update(room.as_bytes());
    h.update(bare_value(pial).as_bytes());
    format!("m{}", &hex::encode(h.finalize())[..32])
}

/// Where Egress writes a replay, on its mount of the shared volume.
const EGRESS_REPLAY_DIR: &str = "/home/egress/replays";

/// How a recording job stands.
#[derive(Debug, PartialEq)]
pub enum EgressOutcome {
    Running,
    Finished { duration_ms: i64 },
    Failed(String),
}

/// Reads one job's answer from ListEgress. A job that wrote nothing has failed, however
/// it ended. Protobuf sends 64-bit numbers as strings.
fn egress_outcome_of(item: &Value) -> EgressOutcome {
    let status = item["status"].as_str().unwrap_or("EGRESS_STARTING");
    match status {
        "EGRESS_STARTING" | "EGRESS_ACTIVE" | "EGRESS_ENDING" => EgressOutcome::Running,
        "EGRESS_COMPLETE" | "EGRESS_LIMIT_REACHED" => {
            let file = &item["file_results"][0];
            let number = |v: &Value| v.as_i64().or_else(|| v.as_str().and_then(|n| n.parse().ok())).unwrap_or(0);
            if number(&file["size"]) > 0 {
                EgressOutcome::Finished { duration_ms: number(&file["duration"]) / 1_000_000 }
            } else {
                EgressOutcome::Failed("no audio was recorded".into())
            }
        }
        _ => EgressOutcome::Failed(item["error"].as_str().unwrap_or(status).to_string()),
    }
}

#[derive(Clone)]
pub struct Media {
    http: reqwest::Client,
    url: String,
    key: String,
    secret: String,
    wake: mpsc::UnboundedSender<Uuid>,
}

impl std::fmt::Debug for Media {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Media(<redacted>)")
    }
}

impl Media {
    /// Refuses a missing or short secret, so the brain never signs with nothing.
    pub fn new(url: &str, key: &str, secret: &str) -> Result<(Self, mpsc::UnboundedReceiver<Uuid>)> {
        if url.is_empty() || key.is_empty() {
            anyhow::bail!("LIVEKIT_URL and LIVEKIT_API_KEY must be set");
        }
        if secret.len() < MIN_SECRET_LEN {
            anyhow::bail!("LIVEKIT_API_SECRET must be at least {MIN_SECRET_LEN} characters");
        }
        let (wake, rx) = mpsc::unbounded_channel();
        let http = reqwest::Client::builder().timeout(CALL_TIMEOUT).build()?;
        Ok((
            Self {
                http,
                url: url.trim_end_matches('/').to_string(),
                key: key.to_string(),
                secret: secret.to_string(),
                wake,
            },
            rx,
        ))
    }

    /// Asks for this room to be brought in line with the database.
    pub fn kick(&self, room: Uuid) {
        if self.wake.send(room).is_err() {
            warn!(%room, "media reconciler is gone");
        }
    }

    fn sign(&self, claims: &Value) -> String {
        let header = B64.encode(br#"{"alg":"HS256","typ":"JWT"}"#);
        let body = B64.encode(claims.to_string());
        let signing_input = format!("{header}.{body}");
        let mut mac = Hmac::<Sha256>::new_from_slice(self.secret.as_bytes())
            .expect("hmac accepts any key length");
        mac.update(signing_input.as_bytes());
        format!("{signing_input}.{}", B64.encode(mac.finalize().into_bytes()))
    }

    /// The token a browser joins with. It may publish only its microphone and may
    /// send no data, so the room carries nothing but voices.
    pub fn join_token(&self, room: Uuid, identity: &str, grant: Grant) -> String {
        let now = Utc::now().timestamp();
        self.sign(&json!({
            "iss": self.key,
            "sub": identity,
            "nbf": now - 5,
            "exp": now + JOIN_TOKEN_TTL_SECS,
            "video": {
                "room": room.to_string(),
                "roomJoin": true,
                "canSubscribe": true,
                "canPublish": grant.publish,
                "canPublishData": false,
                "canPublishSources": ["microphone"],
                "hidden": grant.hidden,
            },
        }))
    }

    /// One RoomService call. None means LiveKit has no such room or participant.
    async fn call(&self, method: &str, room: Option<Uuid>, body: Value) -> Result<Option<Value>> {
        let mut video = json!({ "roomCreate": true, "roomList": true });
        if let Some(room) = room {
            video["room"] = json!(room.to_string());
            video["roomAdmin"] = json!(true);
        }
        self.twirp("livekit.RoomService", method, video, body).await
    }

    async fn egress_call(&self, method: &str, body: Value) -> Result<Option<Value>> {
        self.twirp("livekit.Egress", method, json!({ "roomRecord": true }), body).await
    }

    /// Starts mixing a room into one audio file. Audio-only with no layout, so Egress
    /// does it without a browser. MP4 means AAC, which every browser plays.
    pub async fn start_recording(&self, room: Uuid, file: &str) -> Result<String> {
        let reply = self
            .egress_call(
                "StartRoomCompositeEgress",
                json!({
                    "room_name": room.to_string(),
                    "audio_only": true,
                    "file_outputs": [{
                        "file_type": "MP4",
                        "filepath": format!("{EGRESS_REPLAY_DIR}/{file}"),
                        "disable_manifest": true,
                    }],
                }),
            )
            .await?
            .ok_or_else(|| anyhow!("no such room to record"))?;
        reply["egress_id"]
            .as_str()
            .map(str::to_string)
            .ok_or_else(|| anyhow!("egress answered without an id"))
    }

    /// Asks a recording to finish. One already over has nothing left to stop.
    pub async fn stop_egress(&self, egress: &str) -> Result<()> {
        match self.egress_call("StopEgress", json!({ "egress_id": egress })).await {
            Err(e) if e.to_string().contains("failed_precondition") => Ok(()),
            other => other.map(|_| ()),
        }
    }

    pub async fn egress_outcome(&self, egress: &str) -> Result<EgressOutcome> {
        let reply = self
            .egress_call("ListEgress", json!({ "egress_id": egress }))
            .await?
            .unwrap_or_default();
        Ok(match reply["items"].get(0) {
            Some(item) => egress_outcome_of(item),
            None => EgressOutcome::Failed("egress has no record of this job".into()),
        })
    }

    async fn twirp(&self, service: &str, method: &str, video: Value, body: Value) -> Result<Option<Value>> {
        let now = Utc::now().timestamp();
        let token = self.sign(&json!({
            "iss": self.key,
            "sub": "auralis",
            "nbf": now - 5,
            "exp": now + ADMIN_TOKEN_TTL_SECS,
            "video": video,
        }));
        let resp = self
            .http
            .post(format!("{}/twirp/{service}/{method}", self.url))
            .bearer_auth(token)
            .json(&body)
            .send()
            .await?;
        let status = resp.status();
        if status == StatusCode::NOT_FOUND {
            return Ok(None);
        }
        if !status.is_success() {
            return Err(anyhow!("{method} answered {status}: {}", resp.text().await.unwrap_or_default()));
        }
        Ok(Some(resp.json().await?))
    }

    /// Opens the room. Rooms are never created by a client joining, and asking for a
    /// room that already exists returns it.
    pub async fn ensure_room(&self, room: Uuid) -> Result<()> {
        self.call(
            "CreateRoom",
            None,
            json!({ "name": room.to_string(), "empty_timeout": ROOM_EMPTY_TIMEOUT_SECS }),
        )
        .await?;
        Ok(())
    }

    async fn delete_room(&self, room: Uuid) -> Result<()> {
        self.call("DeleteRoom", Some(room), json!({ "room": room.to_string() })).await?;
        Ok(())
    }

    async fn room_names(&self) -> Result<Vec<String>> {
        let reply = self.call("ListRooms", None, json!({})).await?.unwrap_or_default();
        Ok(reply["rooms"]
            .as_array()
            .map(|rooms| {
                rooms
                    .iter()
                    .filter_map(|r| r["name"].as_str().map(str::to_string))
                    .collect()
            })
            .unwrap_or_default())
    }

    async fn participants(&self, room: Uuid) -> Result<Vec<Value>> {
        let reply = self
            .call("ListParticipants", Some(room), json!({ "room": room.to_string() }))
            .await?
            .unwrap_or_default();
        Ok(reply["participants"].as_array().cloned().unwrap_or_default())
    }

    async fn remove(&self, room: Uuid, identity: &str) -> Result<()> {
        self.call(
            "RemoveParticipant",
            Some(room),
            json!({ "room": room.to_string(), "identity": identity }),
        )
        .await?;
        Ok(())
    }

    async fn set_grant(&self, room: Uuid, identity: &str, grant: Grant) -> Result<()> {
        self.call(
            "UpdateParticipant",
            Some(room),
            json!({
                "room": room.to_string(),
                "identity": identity,
                "permission": {
                    "can_subscribe": true,
                    "can_publish": grant.publish,
                    "can_publish_data": false,
                    "can_publish_sources": ["MICROPHONE"],
                    "hidden": grant.hidden,
                },
            }),
        )
        .await?;
        Ok(())
    }
}

/// What must change in a room. Pure, so the rule is pinned without a running server.
fn corrections(
    connected: &[Value],
    wanted: &HashMap<String, Grant>,
) -> (Vec<String>, Vec<(String, Grant)>) {
    let mut remove = Vec::new();
    let mut update = Vec::new();
    for p in connected {
        // A recorder or other service in the room is not a person with a standing.
        if p["kind"].as_str().is_some_and(|k| k != "STANDARD") {
            continue;
        }
        let Some(identity) = p["identity"].as_str() else {
            continue;
        };
        match wanted.get(identity) {
            None => remove.push(identity.to_string()),
            Some(grant) => {
                let held = Grant {
                    publish: p["permission"]["can_publish"].as_bool().unwrap_or(false),
                    hidden: p["permission"]["hidden"].as_bool().unwrap_or(false),
                };
                if held != *grant {
                    update.push((identity.to_string(), *grant));
                }
            }
        }
    }
    (remove, update)
}

async fn reconcile(s: &AppState, room: Uuid) -> Result<()> {
    let mut conn = s.pool.acquire().await?;
    let freq = pg::get_frequency(&mut conn, room).await?;
    let Some(freq) = freq.filter(|f| f.state == FrequencyState::Live) else {
        drop(conn);
        // Finish the recording first, so its file is closed rather than cut off.
        replay::stop_recording(s, room).await?;
        return s.media.delete_room(room).await;
    };
    let joined = pg::joined(&mut conn, room).await?;
    drop(conn);

    let wanted: HashMap<String, Grant> = joined
        .iter()
        .map(|p| (identity(room, &p.pial), Grant::for_participant(p)))
        .collect();
    let connected = s.media.participants(room).await?;
    let (remove, update) = corrections(&connected, &wanted);
    for who in remove {
        s.media.remove(room, &who).await?;
    }
    for (who, grant) in update {
        s.media.set_grant(room, &who, grant).await?;
    }
    replay::ensure_recording(s, &freq, &connected).await?;
    Ok(())
}

/// Wakes every room LiveKit holds and every room that should be live.
async fn sweep(s: &AppState) -> Result<()> {
    let mut rooms: HashSet<Uuid> = pg::list_open(&s.pool)
        .await?
        .iter()
        .filter(|f| f.state == FrequencyState::Live)
        .map(|f| f.id)
        .collect();
    rooms.extend(
        s.media
            .room_names()
            .await?
            .iter()
            .filter_map(|n| Uuid::parse_str(n).ok()),
    );
    for room in rooms {
        s.media.kick(room);
    }
    replay::sweep(s).await?;
    Ok(())
}

/// One pass at a time, with requests for the same room merged. Each commit wakes a
/// pass that reads after it, so a pass can never leave the room on an older state.
pub fn spawn(s: AppState, mut wake: mpsc::UnboundedReceiver<Uuid>) {
    let swept = s.clone();
    tokio::spawn(async move {
        let mut tick = tokio::time::interval(SWEEP_EVERY);
        loop {
            tick.tick().await;
            if let Err(e) = sweep(&swept).await {
                warn!(error = %e, "media sweep failed; retrying next tick");
            }
        }
    });
    tokio::spawn(async move {
        info!("media reconciler running");
        while let Some(first) = wake.recv().await {
            let mut rooms = HashSet::from([first]);
            while let Ok(next) = wake.try_recv() {
                rooms.insert(next);
            }
            let mut passes = JoinSet::new();
            for room in rooms {
                let s = s.clone();
                passes.spawn(async move {
                    if let Err(e) = reconcile(&s, room).await {
                        warn!(%room, error = %e, "media reconcile failed; the sweep retries it");
                    }
                });
            }
            while passes.join_next().await.is_some() {}
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    const LISTENING: Grant = Grant { publish: false, hidden: true };
    const SPEAKING: Grant = Grant { publish: true, hidden: false };
    const MUTED: Grant = Grant { publish: false, hidden: false };

    fn person(identity: &str, publish: bool, hidden: bool) -> Value {
        json!({ "identity": identity, "kind": "STANDARD", "permission": { "can_publish": publish, "hidden": hidden } })
    }

    #[test]
    fn grants_follow_role_and_mute() {
        assert_eq!(Grant::of(FrequencyRole::Listener, false), LISTENING);
        assert_eq!(Grant::of(FrequencyRole::Speaker, false), SPEAKING);
        assert_eq!(Grant::of(FrequencyRole::Host, true), MUTED);
    }

    #[test]
    fn identity_is_opaque_and_per_room() {
        let (a, b) = (Uuid::new_v4(), Uuid::new_v4());
        let pial = "11111111-1111-1111-1111-111111111111";
        assert_eq!(identity(a, pial), identity(a, pial));
        assert_ne!(identity(a, pial), identity(b, pial));
        assert!(!identity(a, pial).contains(pial));
    }

    #[test]
    fn someone_no_longer_joined_is_removed() {
        let (remove, update) = corrections(&[person("gone", false, true)], &HashMap::new());
        assert_eq!(remove, ["gone"]);
        assert!(update.is_empty());
    }

    #[test]
    fn a_held_grant_that_differs_is_corrected() {
        let wanted = HashMap::from([("a".to_string(), MUTED), ("b".to_string(), SPEAKING)]);
        let (remove, update) = corrections(&[person("a", true, false), person("b", true, false)], &wanted);
        assert!(remove.is_empty());
        assert_eq!(update, [("a".to_string(), MUTED)]);
    }

    #[test]
    fn services_in_the_room_are_left_alone() {
        let recorder = json!({ "identity": "EG_x", "kind": "EGRESS" });
        let (remove, update) = corrections(&[recorder], &HashMap::new());
        assert!(remove.is_empty() && update.is_empty());
    }

    #[test]
    fn a_recording_counts_only_if_it_wrote_something() {
        let done = json!({ "status": "EGRESS_COMPLETE", "file_results": [{ "size": "2048", "duration": "5000000000" }] });
        assert_eq!(egress_outcome_of(&done), EgressOutcome::Finished { duration_ms: 5000 });
        let empty = json!({ "status": "EGRESS_COMPLETE", "file_results": [{ "size": "0" }] });
        assert!(matches!(egress_outcome_of(&empty), EgressOutcome::Failed(_)));
        assert_eq!(egress_outcome_of(&json!({ "status": "EGRESS_ACTIVE" })), EgressOutcome::Running);
        assert!(matches!(egress_outcome_of(&json!({ "status": "EGRESS_FAILED", "error": "x" })), EgressOutcome::Failed(_)));
    }

    #[test]
    fn a_short_secret_is_refused() {
        assert!(Media::new("http://livekit:7880", "key", "short").is_err());
    }
}
