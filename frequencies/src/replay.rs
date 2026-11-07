//! Recording a room and keeping its replay.
//!
//! A host opts in before the room starts. Once someone is on the mic, LiveKit Egress
//! mixes the room into one audio file on the replay volume, and this module follows
//! that job until the file is ready. Everything is driven from what the database and
//! Egress say on each pass, so a restart at any point resumes rather than loses it.
//! A replay stays until its host deletes it.

use std::path::Path;

use anyhow::Result;
use serde_json::{json, Value};
use tracing::{info, warn};
use uuid::Uuid;

use crate::api::ops::{self, Cause};
use crate::domain::lifecycle;
use crate::domain::{Frequency, FrequencyState, ReplayStatus};
use crate::events::EventType;
use crate::herald;
use crate::repository::postgres::{self as pg, Patch};
use crate::state::AppState;

/// A room that keeps failing to record stops trying, instead of trying forever.
const MAX_ATTEMPTS: i64 = 3;

fn someone_is_on_the_mic(connected: &[Value]) -> bool {
    connected.iter().any(|p| {
        p["kind"].as_str().map_or(true, |k| k == "STANDARD")
            && p["tracks"].as_array().is_some_and(|t| !t.is_empty())
    })
}

/// Starts the recording of a live room when one is owed. It waits for someone to be
/// on the mic, so a replay does not open on minutes of silence.
pub async fn ensure_recording(s: &AppState, f: &Frequency, connected: &[Value]) -> Result<()> {
    if !f.recording_enabled || !someone_is_on_the_mic(connected) {
        return Ok(());
    }
    if pg::active_recording(&s.pool, f.id).await?.is_some()
        || pg::failed_recordings(&s.pool, f.id).await? >= MAX_ATTEMPTS
    {
        return Ok(());
    }
    let recording = Uuid::new_v4();
    let file = format!("{}-{recording}.mp4", f.id);
    let egress = s.media.start_recording(f.id, &file).await?;
    if !pg::insert_recording(&s.pool, recording, f.id, &egress, &file).await? {
        // Another pass got there first. This egress is surplus.
        s.media.stop_egress(&egress).await?;
    }
    info!(frequency_id = %f.id, %egress, "recording started");
    Ok(())
}

/// Tells Egress to finish the room's recording. Called before the room closes, so the
/// file is finalised rather than cut off.
pub async fn stop_recording(s: &AppState, id: Uuid) -> Result<()> {
    if let Some((_, egress, _)) = pg::active_recording(&s.pool, id).await? {
        s.media.stop_egress(&egress).await?;
    }
    Ok(())
}

/// Follows every recording in progress to its end, and settles the rooms they belong to.
pub async fn sweep(s: &AppState) -> Result<()> {
    for (recording, id, egress, _) in pg::recordings_in_progress(&s.pool).await? {
        match s.media.egress_outcome(&egress).await? {
            crate::media::EgressOutcome::Running => {}
            crate::media::EgressOutcome::Finished { duration_ms } => {
                pg::finish_recording(&s.pool, recording, "ready", Some(duration_ms), None).await?;
                info!(frequency_id = %id, "recording finished");
            }
            crate::media::EgressOutcome::Failed(reason) => {
                warn!(frequency_id = %id, %egress, %reason, "recording failed");
                pg::finish_recording(&s.pool, recording, "failed", None, Some(&reason)).await?;
            }
        }
    }
    for f in pg::rooms_awaiting_replay(&s.pool).await? {
        if let Err(e) = settle(s, &f).await {
            warn!(frequency_id = %f.id, error = %e, "settling a replay");
        }
    }
    Ok(())
}

/// Moves a finished room on: waiting while a recording is still being written, then
/// to a replay if one came out of it, or to no replay if none did.
async fn settle(s: &AppState, f: &Frequency) -> crate::error::Result<()> {
    let cause = Cause::service("replay-sweep");
    if pg::active_recording(&s.pool, f.id).await?.is_some() {
        if f.state == FrequencyState::Ended {
            let (tx, _) = ops::cas_tx(s, f.id, "replay_processing", |row| {
                lifecycle::transition(row.state, FrequencyState::ProcessingReplay)?;
                let mut p = Patch::state(FrequencyState::ProcessingReplay);
                p.replay_status = Some(ReplayStatus::Processing);
                Ok(p)
            })
            .await?;
            tx.commit().await?;
        }
        return Ok(());
    }
    let ready = pg::latest_ready_recording(&s.pool, f.id).await?;
    let status = if ready.is_some() { ReplayStatus::Ready } else { ReplayStatus::Failed };
    let (mut tx, updated) = ops::cas_tx(s, f.id, "replay_settled", |row| {
        lifecycle::transition(row.state, FrequencyState::Archived)?;
        let mut p = Patch::state(FrequencyState::Archived);
        p.replay_status = Some(status);
        Ok(p)
    })
    .await?;
    pg::insert_event(&mut tx, &cause.event(f.id, EventType::RecordingCompleted, json!({ "replay": ready.is_some() }))).await?;
    if ready.is_some() {
        pg::insert_event(&mut tx, &cause.event(f.id, EventType::ReplayReady, json!({}))).await?;
        herald::notify(
            &mut tx,
            &updated.host_pial,
            herald::KIND_REPLAY_READY,
            &f.id.to_string(),
            &format!("Your replay of {} is ready", updated.title),
            "It stays up until you delete it.",
            &format!("/frequencies/{}", f.id),
        )
        .await?;
    }
    tx.commit().await?;
    Ok(())
}

/// Removes a replay's file. A file already gone is what was asked for.
pub async fn remove_file(dir: &str, file: &str) -> std::io::Result<()> {
    let Some(name) = Path::new(file).file_name() else {
        return Ok(());
    };
    match tokio::fs::remove_file(Path::new(dir).join(name)).await {
        Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(e),
        _ => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn silence_does_not_start_a_recording() {
        assert!(!someone_is_on_the_mic(&[]));
        assert!(!someone_is_on_the_mic(&[json!({ "kind": "STANDARD", "tracks": [] })]));
        assert!(!someone_is_on_the_mic(&[json!({ "kind": "EGRESS", "tracks": [{}] })]));
        assert!(someone_is_on_the_mic(&[json!({ "kind": "STANDARD", "tracks": [{}] })]));
        assert!(someone_is_on_the_mic(&[json!({ "tracks": [{}] })]));
    }
}
