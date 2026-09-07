//! Push notifications through Herald (§22).
//!
//! Herald is an HTTP sink with no deduplication of its own, so the cooldown
//! lives here: one notification of a kind per person per
//! `AURALIS_NOTIFY_COOLDOWN_SECS`, counted in Redis so every node agrees. A
//! host that starts and stops five times in a minute produces one push.
//!
//! Delivery is best effort by design — a push that did not go out must never
//! undo the approval that caused it — but a failure is logged and counted,
//! never swallowed.

use metrics::counter;
use serde_json::json;
use tracing::warn;

use crate::identity::bare_value;
use crate::state::AppState;
use crate::telemetry::metrics as m;

pub const KIND_SPEAKER_ACCEPTED: &str = "frequency_speaker_accepted";
pub const KIND_COHOST: &str = "frequency_cohost";

/// `target` is a canonical `pial:` name. Herald's field is spelled
/// `pial_shard_id`, but what every caller puts in it is a raw PIAL; this
/// brain does the same rather than spread a second vocabulary.
pub async fn notify(
    state: &AppState,
    target: &str,
    kind: &'static str,
    title: &str,
    body: &str,
    action_url: &str,
) {
    match state
        .hot
        .notify_allowed(target, kind, state.config.notify_cooldown)
        .await
    {
        Ok(true) => {}
        Ok(false) => {
            counter!(m::HERALD_NOTIFY_TOTAL, "kind" => kind, "outcome" => "cooldown").increment(1);
            return;
        }
        Err(e) => {
            // If the cooldown store is down, sending is the safer failure:
            // one duplicate push beats a silently missed acceptance.
            warn!(error = %e, kind, "herald cooldown check failed; sending anyway");
        }
    }

    let url = format!("{}/v1/notify", state.config.herald_url);
    let result = state
        .herald_client
        .post(&url)
        .json(&json!({
            "pial_shard_id": bare_value(target),
            "notification_type": kind,
            "title": title,
            "body": body,
            "action_url": action_url,
        }))
        .send()
        .await;

    match result {
        Ok(resp) if resp.status().is_success() => {
            counter!(m::HERALD_NOTIFY_TOTAL, "kind" => kind, "outcome" => "sent").increment(1);
        }
        Ok(resp) => {
            counter!(m::HERALD_NOTIFY_TOTAL, "kind" => kind, "outcome" => "refused").increment(1);
            warn!(
                kind,
                status = resp.status().as_u16(),
                "herald refused the notification"
            );
        }
        Err(e) => {
            counter!(m::HERALD_NOTIFY_TOTAL, "kind" => kind, "outcome" => "failed").increment(1);
            warn!(kind, error = %e, "herald notification failed");
        }
    }
}
