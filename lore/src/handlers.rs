use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::Json,
};
use manhattan_client::Manhattan;
use serde_json::{json, Value};
use tracing::{error, info, warn};

use crate::{
    db::{self, AwardOutcome},
    identity::{self, bare_value, ResolveError},
    manhattan_outbox,
    models::LoreEvent,
};

#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    pub herald_url: String,
    pub herald_client: reqwest::Client,
    /// The naming plane. Lore resolves identities through it and owns none.
    pub manhattan: Manhattan,
}

/// Every handler answers with a body, including on failure. A bare status code
/// tells an operator that something went wrong and nothing about what.
type Reply = Result<Json<Value>, (StatusCode, Json<Value>)>;

fn fail(code: StatusCode, message: impl Into<String>) -> (StatusCode, Json<Value>) {
    (code, Json(json!({ "error": message.into() })))
}

fn internal(context: &str, e: impl std::fmt::Display) -> (StatusCode, Json<Value>) {
    error!(error = %e, "{context}");
    fail(StatusCode::INTERNAL_SERVER_ERROR, context.to_string())
}

/// Resolve whatever the caller sent into the canonical `pial:<uuid>` name lore
/// stores. Malformed, unresolvable and unreachable stay three different answers.
async fn identity_of(state: &AppState, raw: &str) -> Result<String, (StatusCode, Json<Value>)> {
    identity::resolve(&state.manhattan, raw).await.map_err(|e| {
        let code = match &e {
            ResolveError::Malformed(_) => StatusCode::BAD_REQUEST,
            ResolveError::Unresolved(_) => StatusCode::NOT_FOUND,
            ResolveError::Unavailable(_) => StatusCode::SERVICE_UNAVAILABLE,
        };
        warn!(reference = %raw, status = code.as_u16(), error = %e, "identity not resolved");
        fail(code, e.to_string())
    })
}

pub async fn health(State(s): State<AppState>) -> Reply {
    // The backlog is the honest health signal: a drain that has stopped shows
    // up here as a number that climbs, rather than as a queue nobody reads.
    let backlog = manhattan_outbox::backlog(&s.pool)
        .await
        .map_err(|e| internal("manhattan outbox backlog", e))?;

    Ok(Json(json!({
        "status": "ok",
        "service": "lore",
        "manhattan": s.manhattan.configured(),
        "manhattan_outbox_backlog": backlog,
    })))
}

pub async fn list_achievements(State(s): State<AppState>) -> Reply {
    let defs = db::list_achievements(&s.pool)
        .await
        .map_err(|e| internal("list_achievements", e))?;
    Ok(Json(json!({ "achievements": defs })))
}

pub async fn get_user_achievements(
    State(s): State<AppState>,
    Path(identity_ref): Path<String>,
) -> Reply {
    let identity_name = identity_of(&s, &identity_ref).await?;

    let achievements = db::user_achievements(&s.pool, &identity_name)
        .await
        .map_err(|e| internal("user_achievements", e))?;
    let xp = db::get_user_xp(&s.pool, &identity_name)
        .await
        .map_err(|e| internal("get_user_xp", e))?;

    Ok(Json(json!({
        "identity": identity_name,
        "achievements": achievements,
        "xp": db::xp_status(xp.total_xp),
    })))
}

pub async fn award_achievement(
    State(s): State<AppState>,
    Path((identity_ref, achievement_id)): Path<(String, String)>,
) -> Reply {
    let identity_name = identity_of(&s, &identity_ref).await?;

    match db::award_achievement(&s.pool, &identity_name, &achievement_id)
        .await
        .map_err(|e| internal("award_achievement", e))?
    {
        AwardOutcome::Awarded(xp) => {
            info!(identity = %identity_name, achievement = %achievement_id, xp, "achievement awarded");
            notify_herald(&s, &identity_name, &achievement_id, xp).await;
            Ok(Json(json!({ "awarded": true, "xp_reward": xp })))
        }
        AwardOutcome::AlreadyEarned => Ok(Json(
            json!({ "awarded": false, "reason": "already_earned" }),
        )),
        AwardOutcome::UnknownAchievement => Err(fail(
            StatusCode::NOT_FOUND,
            format!("no achievement is defined as {achievement_id:?}"),
        )),
    }
}

pub async fn get_xp(State(s): State<AppState>, Path(identity_ref): Path<String>) -> Reply {
    let identity_name = identity_of(&s, &identity_ref).await?;
    let xp = db::get_user_xp(&s.pool, &identity_name)
        .await
        .map_err(|e| internal("get_xp", e))?;
    Ok(Json(json!(db::xp_status(xp.total_xp))))
}

pub async fn receive_event(State(s): State<AppState>, Json(event): Json<LoreEvent>) -> Reply {
    let identity_name = identity_of(&s, &event.identity).await?;
    info!(event_type = %event.event_type, identity = %identity_name, "lore event received");

    // Map event types to achievement IDs — expand as triggers are wired up
    let achievement_id = match event.event_type.as_str() {
        "user.registered" => Some("first_pulse"),
        "avatar.uploaded" => Some("face_reveal"),
        "profile.completed" => Some("signal_found"),
        "verification.completed" => Some("echo_online"),
        "message.sent" => Some("first_contact"),
        "audio.uploaded" => Some("mic_check"),
        "video.uploaded" => Some("directors_cut"),
        "song.published" => Some("bedroom_producer"),
        "sale.completed" => Some("first_sale"),
        "payout.received" => Some("paid_creator"),
        "user.early_adopter" => Some("founding_member"),
        _ => {
            warn!(event_type = %event.event_type, "no achievement mapping");
            None
        }
    };

    let Some(ach_id) = achievement_id else {
        return Ok(Json(
            json!({ "received": true, "identity": identity_name, "awarded": false }),
        ));
    };

    match db::award_achievement(&s.pool, &identity_name, ach_id)
        .await
        .map_err(|e| internal("award from event", e))?
    {
        AwardOutcome::Awarded(xp) => {
            info!(achievement = ach_id, xp, "achievement triggered by event");
            notify_herald(&s, &identity_name, ach_id, xp).await;
            Ok(Json(
                json!({ "received": true, "identity": identity_name, "awarded": true }),
            ))
        }
        AwardOutcome::AlreadyEarned => Ok(Json(
            json!({ "received": true, "identity": identity_name, "awarded": false }),
        )),
        // The event map above names an achievement the seed does not define.
        // That is a defect in this file, not in the caller's request.
        AwardOutcome::UnknownAchievement => Err(internal(
            "event mapping names an undefined achievement",
            ach_id,
        )),
    }
}

/// Tell Herald to push the grant to the person's devices.
///
/// Herald's field is spelled `pial_shard_id`, but what every existing caller
/// puts in it is a raw PIAL — feed-engine's `HeraldNotify` sends `pialID` under
/// that key. So lore sends the PIAL too, taken from the name it resolved.
/// Correcting Herald's spelling is Herald's change to make, not lore's; naming
/// it wrong here as well would only spread the second vocabulary further.
///
/// Delivery is best effort by design — a push that did not go out must never
/// undo an achievement that was earned — but a failure is logged, never
/// swallowed.
async fn notify_herald(s: &AppState, identity_name: &str, achievement_id: &str, xp: i32) {
    let url = format!("{}/v1/notify/achievement", s.herald_url);
    let result = s
        .herald_client
        .post(&url)
        .json(&json!({
            "pial_shard_id": bare_value(identity_name),
            "achievement_id": achievement_id,
            "xp_reward": xp,
        }))
        .send()
        .await;

    match result {
        Ok(resp) if resp.status().is_success() => {}
        Ok(resp) => warn!(
            identity = %identity_name,
            achievement = %achievement_id,
            status = resp.status().as_u16(),
            "herald refused the achievement notification"
        ),
        Err(e) => warn!(
            identity = %identity_name,
            achievement = %achievement_id,
            error = %e,
            "herald notification failed"
        ),
    }
}
