//! Notifications to Herald, queued through this brain's durable outbox.
//!
//! A notification is written in the same transaction as the action that caused
//! it, so the two commit together and an outage between here and Herald delays
//! delivery without losing it. `action_key` names that one action. The outbox
//! and Herald both treat a repeated event id as already done, so a retried
//! action notifies once, while a later action of the same kind notifies again.

use metrics::counter;
use serde_json::json;
use sqlx::PgConnection;

use crate::error::Result;
use crate::identity::bare_value;
use crate::telemetry::metrics as m;

pub const KIND_SPEAKER_ACCEPTED: &str = "frequency_speaker_accepted";
pub const KIND_COHOST: &str = "frequency_cohost";
pub const KIND_INVITE: &str = "frequency_invite";
pub const KIND_REPLAY_READY: &str = "frequency_replay_ready";
pub const KIND_HOST_START: &str = "frequency_host_start";

/// `target` is a canonical `pial:` name. Herald's field is spelled
/// `pial_shard_id`, but what every caller puts in it is a raw PIAL; this
/// brain does the same rather than spread a second vocabulary.
pub async fn notify(
    conn: &mut PgConnection,
    target: &str,
    kind: &'static str,
    action_key: &str,
    title: &str,
    body: &str,
    action_url: &str,
) -> Result<()> {
    let event = crate::notifications::Event {
        actor_account: None,
        recipient_account: None,
        event_id: format!("auralis:{kind}:{}:{action_key}", bare_value(target)),
        recipient_pial: bare_value(target).to_string(),
        kind: kind.into(),
        target_id: action_url.to_string(),
        target_type: "frequency".into(),
        payload: json!({ "preview": title }),
        push_title: title.into(),
        push_body: body.into(),
        push_url: action_url.into(),
    };
    crate::notifications::enqueue(conn, &event).await?;
    counter!(m::HERALD_NOTIFY_TOTAL, "kind" => kind, "outcome" => "queued").increment(1);
    Ok(())
}
