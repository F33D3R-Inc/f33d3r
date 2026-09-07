//! Host and co-host controls (§16). Every rule is checked here against the
//! stored role, whatever the browser showed.

use axum::{
    extract::{Path, State},
    Json,
};
use metrics::counter;
use serde::Deserialize;
use serde_json::{json, Value};
use uuid::Uuid;

use super::ops::{self, Cause};
use super::view;
use crate::auth::Actor;
use crate::domain::permissions;
use crate::domain::role::may;
use crate::domain::{Action, Frequency, FrequencyRole, RequestStatus};
use crate::error::{AuralisError, Result};
use crate::events::EventType;
use crate::herald;
use crate::identity::{self, bare_value};
use crate::repository::postgres as pg;
use crate::security::abuse;
use crate::security::rate_limit;
use crate::state::AppState;
use crate::telemetry::metrics::{self as m, Timed};

#[derive(Deserialize, Default)]
pub struct ReasonBody {
    #[serde(default)]
    pub reason: String,
}

struct Moderator {
    role: FrequencyRole,
    cause: Cause,
}

/// Load the Frequency and prove the actor may attempt `action` in it.
async fn moderator(
    s: &AppState,
    conn: &mut sqlx::PgConnection,
    id: Uuid,
    actor: &Actor,
    action: Action,
) -> Result<(Frequency, Moderator)> {
    ops::limit(s, rate_limit::MODERATION, &actor.pial).await?;
    let freq = ops::load(conn, id).await?;
    ops::require_open(&freq)?;
    let role = ops::role_of(conn, &freq, &actor.pial)
        .await?
        .ok_or_else(|| AuralisError::Forbidden("not in this frequency".into()))?;
    if !may(role, action) {
        return Err(AuralisError::Forbidden(format!(
            "{role} may not {action:?}"
        )));
    }
    Ok((
        freq,
        Moderator {
            role,
            cause: Cause::person(&actor.pial, &actor.correlation_id),
        },
    ))
}

/// Resolve the `:pial` path segment to a canonical name and check rank.
async fn target_of(
    s: &AppState,
    conn: &mut sqlx::PgConnection,
    freq: &Frequency,
    moderator_role: FrequencyRole,
    raw: &str,
) -> Result<(String, Option<FrequencyRole>)> {
    let name = identity::resolve(&s.manhattan, raw).await?;
    let target_role = ops::role_of(conn, freq, &name).await?;
    if let Some(tr) = target_role {
        if !moderator_role.outranks(tr) {
            return Err(AuralisError::Forbidden(format!(
                "{moderator_role} may not act on a {tr}"
            )));
        }
    }
    Ok((name, target_role))
}

async fn respond(s: &AppState, freq: &Frequency, actor: &Actor) -> Result<Json<Value>> {
    let mut conn = s.pool.acquire().await?;
    let f = ops::load(&mut conn, freq.id).await?;
    let v = view::build(&mut conn, &s.hot, &f, Some(&actor.pial)).await?;
    Ok(Json(json!({ "frequency": v })))
}

// ── requests ─────────────────────────────────────────────────────────────────

pub async fn approve(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, rid)): Path<(Uuid, Uuid)>,
) -> Result<Json<Value>> {
    let _t = Timed::named(m::SPEAKER_PROMOTION_LATENCY_SECONDS, "approve");
    let mut tx = s.pool.begin().await?;
    let (freq, moderator) = moderator(&s, &mut tx, id, &actor, Action::ApproveSpeaker).await?;
    ops::require_live(&freq)?;
    let sr = pg::get_request(&mut tx, rid)
        .await?
        .ok_or(AuralisError::NotFound)?;
    if sr.frequency_id != id {
        return Err(AuralisError::NotFound);
    }
    let (_, speakers_now) = s.hot.counts(id).await?;
    if !permissions::can_add_speaker(&freq, speakers_now) {
        return Err(AuralisError::Conflict("speakers_full"));
    }
    let Some(resolved) =
        pg::resolve_request(&mut tx, rid, RequestStatus::Approved, Some(&actor.pial)).await?
    else {
        // Somebody else resolved it first. Not an error; nothing to do.
        tx.rollback().await?;
        return respond(&s, &freq, &actor).await;
    };
    pg::grant_role(
        &mut tx,
        id,
        &resolved.pial,
        FrequencyRole::Speaker,
        &actor.pial,
    )
    .await?;
    let seated = pg::set_role(&mut tx, id, &resolved.pial, FrequencyRole::Speaker).await?;
    pg::insert_event(
        &mut tx,
        &moderator.cause.event(
            id,
            EventType::SpeakerApproved,
            json!({ "request_id": resolved.id, "pial_id": bare_value(&resolved.pial), "by_role": moderator.role }),
        ),
    )
    .await?;
    tx.commit().await?;

    // Presence: the person is now in the speakers set, if they are here.
    if seated.is_some() && s.hot.present(id, &resolved.pial).await? {
        s.hot
            .touch(id, &resolved.pial, FrequencyRole::Speaker)
            .await?;
    }
    counter!(m::SPEAKER_PROMOTIONS_TOTAL).increment(1);
    counter!(m::SPEAKER_REQUESTS_TOTAL, "outcome" => "approved").increment(1);

    let title = format!("You have the mic in {}", freq.title);
    herald::notify(
        &s,
        &resolved.pial,
        herald::KIND_SPEAKER_ACCEPTED,
        &title,
        "Your request to speak was accepted.",
        &format!("/frequencies/{id}"),
    )
    .await;

    respond(&s, &freq, &actor).await
}

pub async fn decline(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, rid)): Path<(Uuid, Uuid)>,
) -> Result<Json<Value>> {
    let mut tx = s.pool.begin().await?;
    let (freq, moderator) = moderator(&s, &mut tx, id, &actor, Action::DeclineSpeaker).await?;
    let sr = pg::get_request(&mut tx, rid)
        .await?
        .ok_or(AuralisError::NotFound)?;
    if sr.frequency_id != id {
        return Err(AuralisError::NotFound);
    }
    if let Some(r) =
        pg::resolve_request(&mut tx, rid, RequestStatus::Declined, Some(&actor.pial)).await?
    {
        pg::insert_event(
            &mut tx,
            &moderator.cause.event(
                id,
                EventType::SpeakerDeclined,
                json!({ "request_id": r.id, "pial_id": bare_value(&r.pial), "by": "moderator", "outcome": "declined" }),
            ),
        )
        .await?;
        counter!(m::SPEAKER_REQUESTS_TOTAL, "outcome" => "declined").increment(1);
    }
    tx.commit().await?;
    respond(&s, &freq, &actor).await
}

// ── participants ─────────────────────────────────────────────────────────────

async fn set_mute(
    s: AppState,
    actor: Actor,
    id: Uuid,
    raw_target: &str,
    muted: bool,
) -> Result<Json<Value>> {
    let mut tx = s.pool.begin().await?;
    let freq = ops::load(&mut tx, id).await?;
    ops::require_open(&freq)?;
    let target = identity::resolve(&s.manhattan, raw_target).await?;
    let my_role = ops::role_of(&mut tx, &freq, &actor.pial)
        .await?
        .ok_or_else(|| AuralisError::Forbidden("not in this frequency".into()))?;
    let is_self = target == actor.pial;
    let action = if is_self {
        Action::MuteSelf
    } else {
        Action::MuteOther
    };
    if !may(my_role, action) {
        return Err(AuralisError::Forbidden(format!(
            "{my_role} may not {action:?}"
        )));
    }
    if !is_self {
        ops::limit(&s, rate_limit::MODERATION, &actor.pial).await?;
        let target_role = ops::role_of(&mut tx, &freq, &target).await?;
        if let Some(tr) = target_role {
            if !my_role.outranks(tr) {
                return Err(AuralisError::Forbidden(format!(
                    "{my_role} may not mute a {tr}"
                )));
            }
        }
    }
    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    let Some(row) = pg::set_muted(&mut tx, id, &target, muted).await? else {
        return Err(AuralisError::Conflict("not_joined"));
    };
    pg::insert_event(
        &mut tx,
        &cause.event(
            id,
            EventType::SpeakerMuted,
            json!({ "pial_id": bare_value(&target), "muted": muted, "self": is_self, "role": row.role }),
        ),
    )
    .await?;
    if !is_self {
        pg::log_moderation(
            &mut tx,
            id,
            &cause.actor,
            Some(&target),
            if muted { "mute" } else { "unmute" },
            "",
        )
        .await?;
        counter!(m::MODERATION_ACTIONS_TOTAL, "action" => if muted { "mute" } else { "unmute" })
            .increment(1);
    }
    tx.commit().await?;
    respond(&s, &freq, &actor).await
}

pub async fn mute(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, pial)): Path<(Uuid, String)>,
) -> Result<Json<Value>> {
    set_mute(s, actor, id, &pial, true).await
}

pub async fn unmute(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, pial)): Path<(Uuid, String)>,
) -> Result<Json<Value>> {
    set_mute(s, actor, id, &pial, false).await
}

/// Speaker back to listener. Their granted role is revoked so a rejoin does
/// not restore it.
pub async fn demote(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, pial)): Path<(Uuid, String)>,
) -> Result<Json<Value>> {
    let mut tx = s.pool.begin().await?;
    let (freq, moderator) = moderator(&s, &mut tx, id, &actor, Action::DemoteSpeaker).await?;
    let (target, target_role) = target_of(&s, &mut tx, &freq, moderator.role, &pial).await?;
    if target_role != Some(FrequencyRole::Speaker) {
        return Err(AuralisError::Conflict("not_a_speaker"));
    }
    pg::revoke_role(&mut tx, id, &target, &actor.pial).await?;
    pg::set_role(&mut tx, id, &target, FrequencyRole::Listener).await?;
    pg::insert_event(
        &mut tx,
        &moderator.cause.event(
            id,
            EventType::SpeakerLeft,
            json!({ "pial_id": bare_value(&target), "role": FrequencyRole::Speaker, "cause": "demoted" }),
        ),
    )
    .await?;
    pg::log_moderation(
        &mut tx,
        id,
        &moderator.cause.actor,
        Some(&target),
        "demote",
        "",
    )
    .await?;
    tx.commit().await?;
    if s.hot.present(id, &target).await? {
        s.hot.touch(id, &target, FrequencyRole::Listener).await?;
    }
    counter!(m::MODERATION_ACTIONS_TOTAL, "action" => "demote").increment(1);
    respond(&s, &freq, &actor).await
}

pub async fn remove(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, pial)): Path<(Uuid, String)>,
    body: Option<Json<ReasonBody>>,
) -> Result<Json<Value>> {
    let reason = abuse::reason(&body.map(|Json(b)| b.reason).unwrap_or_default())?;
    let mut tx = s.pool.begin().await?;
    let (freq, moderator) = moderator(&s, &mut tx, id, &actor, Action::RemoveParticipant).await?;
    let (target, _) = target_of(&s, &mut tx, &freq, moderator.role, &pial).await?;
    let Some(removed) = pg::remove(&mut tx, id, &target, &actor.pial, &reason).await? else {
        return Err(AuralisError::Conflict("not_joined"));
    };
    if removed.role == FrequencyRole::Speaker {
        pg::revoke_role(&mut tx, id, &target, &actor.pial).await?;
    }
    pg::insert_event(
        &mut tx,
        &moderator.cause.event(
            id,
            EventType::ParticipantRemoved,
            json!({ "pial_id": bare_value(&target), "role": removed.role, "reason": reason }),
        ),
    )
    .await?;
    pg::log_moderation(
        &mut tx,
        id,
        &moderator.cause.actor,
        Some(&target),
        "remove",
        &reason,
    )
    .await?;
    tx.commit().await?;
    s.hot.forget(id, &target).await?;
    counter!(m::MODERATION_ACTIONS_TOTAL, "action" => "remove").increment(1);
    counter!(m::LEAVE_TOTAL, "cause" => "removed").increment(1);
    respond(&s, &freq, &actor).await
}

/// Block: removed now and refused at every future Tune In (§23 "prevent
/// rejoin").
pub async fn block(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, pial)): Path<(Uuid, String)>,
    body: Option<Json<ReasonBody>>,
) -> Result<Json<Value>> {
    let reason = abuse::reason(&body.map(|Json(b)| b.reason).unwrap_or_default())?;
    let mut tx = s.pool.begin().await?;
    let (freq, moderator) = moderator(&s, &mut tx, id, &actor, Action::BlockParticipant).await?;
    let (target, _) = target_of(&s, &mut tx, &freq, moderator.role, &pial).await?;
    if target == freq.host_pial {
        return Err(AuralisError::Forbidden("the host cannot be blocked".into()));
    }
    pg::block(&mut tx, id, &target, &actor.pial, &reason).await?;
    let removed = pg::remove(&mut tx, id, &target, &actor.pial, &reason).await?;
    pg::revoke_role(&mut tx, id, &target, &actor.pial).await?;
    if let Some(r) = pg::pending_request_for(&mut tx, id, &target).await? {
        pg::resolve_request(&mut tx, r.id, RequestStatus::Declined, Some(&actor.pial)).await?;
    }
    pg::insert_event(
        &mut tx,
        &moderator.cause.event(
            id,
            EventType::ParticipantBlocked,
            json!({ "pial_id": bare_value(&target), "was_joined": removed.is_some(), "reason": reason }),
        ),
    )
    .await?;
    pg::log_moderation(
        &mut tx,
        id,
        &moderator.cause.actor,
        Some(&target),
        "block",
        &reason,
    )
    .await?;
    tx.commit().await?;
    s.hot.forget(id, &target).await?;
    counter!(m::MODERATION_ACTIONS_TOTAL, "action" => "block").increment(1);
    respond(&s, &freq, &actor).await
}

pub async fn unblock(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, pial)): Path<(Uuid, String)>,
) -> Result<Json<Value>> {
    let mut tx = s.pool.begin().await?;
    let (freq, moderator) = moderator(&s, &mut tx, id, &actor, Action::BlockParticipant).await?;
    let target = identity::resolve(&s.manhattan, &pial).await?;
    let done = pg::unblock(&mut tx, id, &target).await?;
    if done {
        pg::log_moderation(
            &mut tx,
            id,
            &moderator.cause.actor,
            Some(&target),
            "unblock",
            "",
        )
        .await?;
    }
    tx.commit().await?;
    counter!(m::MODERATION_ACTIONS_TOTAL, "action" => "unblock").increment(1);
    respond(&s, &freq, &actor).await
}

// ── co-hosts ─────────────────────────────────────────────────────────────────

pub async fn add_cohost(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, pial)): Path<(Uuid, String)>,
) -> Result<Json<Value>> {
    ops::limit(&s, rate_limit::ROLE_TOGGLE, &actor.pial).await?;
    let mut tx = s.pool.begin().await?;
    let (freq, moderator) = moderator(&s, &mut tx, id, &actor, Action::AddCoHost).await?;
    let target = identity::resolve(&s.manhattan, &pial).await?;
    if target == freq.host_pial {
        return Err(AuralisError::Conflict("is_host"));
    }
    if pg::is_blocked(&mut tx, id, &target).await? {
        return Err(AuralisError::Conflict("blocked"));
    }
    pg::grant_role(&mut tx, id, &target, FrequencyRole::CoHost, &actor.pial).await?;
    let seated = pg::set_role(&mut tx, id, &target, FrequencyRole::CoHost).await?;
    if let Some(r) = pg::pending_request_for(&mut tx, id, &target).await? {
        pg::resolve_request(&mut tx, r.id, RequestStatus::Approved, Some(&actor.pial)).await?;
    }
    pg::insert_event(
        &mut tx,
        &moderator.cause.event(
            id,
            EventType::CohostAdded,
            json!({ "pial_id": bare_value(&target) }),
        ),
    )
    .await?;
    pg::log_moderation(
        &mut tx,
        id,
        &moderator.cause.actor,
        Some(&target),
        "cohost_added",
        "",
    )
    .await?;
    tx.commit().await?;
    if seated.is_some() && s.hot.present(id, &target).await? {
        s.hot.touch(id, &target, FrequencyRole::CoHost).await?;
    }
    counter!(m::MODERATION_ACTIONS_TOTAL, "action" => "cohost_added").increment(1);
    herald::notify(
        &s,
        &target,
        herald::KIND_COHOST,
        &format!("You are co-hosting {}", freq.title),
        "The host made you a co-host.",
        &format!("/frequencies/{id}"),
    )
    .await;
    respond(&s, &freq, &actor).await
}

pub async fn remove_cohost(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, pial)): Path<(Uuid, String)>,
) -> Result<Json<Value>> {
    ops::limit(&s, rate_limit::ROLE_TOGGLE, &actor.pial).await?;
    let mut tx = s.pool.begin().await?;
    let (freq, moderator) = moderator(&s, &mut tx, id, &actor, Action::RemoveCoHost).await?;
    let target = identity::resolve(&s.manhattan, &pial).await?;
    let was = pg::granted_role(&mut tx, id, &target).await?;
    if was != Some(FrequencyRole::CoHost) {
        return Err(AuralisError::Conflict("not_a_cohost"));
    }
    pg::revoke_role(&mut tx, id, &target, &actor.pial).await?;
    // A former co-host keeps the microphone: they were trusted to speak.
    let seated = pg::set_role(&mut tx, id, &target, FrequencyRole::Speaker).await?;
    if seated.is_some() {
        pg::grant_role(&mut tx, id, &target, FrequencyRole::Speaker, &actor.pial).await?;
    }
    pg::insert_event(
        &mut tx,
        &moderator.cause.event(
            id,
            EventType::CohostRemoved,
            json!({ "pial_id": bare_value(&target) }),
        ),
    )
    .await?;
    pg::log_moderation(
        &mut tx,
        id,
        &moderator.cause.actor,
        Some(&target),
        "cohost_removed",
        "",
    )
    .await?;
    tx.commit().await?;
    if seated.is_some() && s.hot.present(id, &target).await? {
        s.hot.touch(id, &target, FrequencyRole::Speaker).await?;
    }
    counter!(m::MODERATION_ACTIONS_TOTAL, "action" => "cohost_removed").increment(1);
    respond(&s, &freq, &actor).await
}
