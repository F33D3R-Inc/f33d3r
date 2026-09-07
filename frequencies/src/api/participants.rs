//! Tune In, Leave, heartbeat, Request Mic.

use axum::{
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    Json,
};
use metrics::counter;
use serde::Deserialize;
use serde_json::{json, Value};
use uuid::Uuid;

use super::frequencies::session_for;
use super::ops::{self, Cause};
use super::view::{self, RequestView};
use crate::auth::Actor;
use crate::domain::permissions;
use crate::domain::role::may;
use crate::domain::{Action, RequestStatus};
use crate::error::{AuralisError, Result};
use crate::events::EventType;
use crate::identity::bare_value;
use crate::repository::postgres as pg;
use crate::security::abuse;
use crate::security::rate_limit;
use crate::state::AppState;
use crate::telemetry::metrics::{self as m, Timed};

fn idem_key(headers: &HeaderMap) -> Result<Option<String>> {
    abuse::idempotency_key(headers.get("Idempotency-Key").and_then(|v| v.to_str().ok()))
}

/// Tune In (§12, §15). Admission is decided here, once, from the durable row
/// and the live counts; the session token that comes back is the only thing
/// the browser will ever present to the media plane.
pub async fn tune_in(
    State(s): State<AppState>,
    actor: Actor,
    headers: HeaderMap,
    Path(id): Path<Uuid>,
) -> Result<(StatusCode, Json<Value>)> {
    let _t = Timed::named(m::JOIN_AUTH_LATENCY_SECONDS, "tune_in");
    ops::limit(&s, rate_limit::JOIN, &actor.pial).await?;
    let key = idem_key(&headers)?;
    let cause = Cause::person(&actor.pial, &actor.correlation_id);

    ops::idempotent(&s, &actor.pial, key, || async {
        let mut conn = s.pool.acquire().await?;
        let freq = ops::load(&mut conn, id).await?;
        let granted = pg::granted_role(&mut conn, id, &actor.pial).await?;
        let role = permissions::role_on_entry(&freq, &actor.pial, granted);
        let blocked = pg::is_blocked(&mut conn, id, &actor.pial).await?;
        let (listeners, speakers) = if freq.state.is_open() {
            s.hot.counts(id).await?
        } else {
            (0, 0)
        };

        if let Err(refusal) = permissions::admit(&freq, role, blocked, listeners, speakers) {
            counter!(m::JOIN_REFUSED_TOTAL, "reason" => refusal.code()).increment(1);
            return Err(AuralisError::Conflict(refusal.code()));
        }
        drop(conn);

        let mut tx = s.pool.begin().await?;
        let (row, fresh) = pg::join(&mut tx, id, &actor.pial, role).await?;
        // A rejoin after a role change (approved while away) carries the
        // granted role; the open row is corrected rather than duplicated.
        let row = if row.role != role {
            pg::set_role(&mut tx, id, &actor.pial, role)
                .await?
                .unwrap_or(row)
        } else {
            row
        };
        if fresh {
            let t = if role.speaks() {
                EventType::SpeakerJoined
            } else {
                EventType::ListenerJoined
            };
            pg::insert_event(
                &mut tx,
                &cause.event(id, t, json!({ "pial_id": actor.pial_id(), "role": role })),
            )
            .await?;
        }
        tx.commit().await?;

        s.hot.touch(id, &actor.pial, row.role).await?;
        counter!(m::JOIN_TOTAL, "role" => row.role.as_str()).increment(1);

        let mut conn = s.pool.acquire().await?;
        let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
        let session = session_for(&s, id, &actor.pial, row.role);
        Ok((
            StatusCode::OK,
            json!({ "frequency": v, "session": session, "rejoined": !fresh }),
        ))
    })
    .await
}

pub async fn leave(
    State(s): State<AppState>,
    actor: Actor,
    Path(id): Path<Uuid>,
) -> Result<Json<Value>> {
    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    let mut tx = s.pool.begin().await?;
    let freq = ops::load(&mut tx, id).await?;
    let left = ops::depart(&s, &mut tx, &freq, &actor.pial, &cause, "leave").await?;
    tx.commit().await?;
    let mut conn = s.pool.acquire().await?;
    let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
    Ok(Json(json!({ "frequency": v, "left": left.is_some() })))
}

/// Presence beat. Cheap by design: one Redis pipeline, no PostgreSQL write.
/// Answers the live counts so a caller rendering a count needs nothing else.
pub async fn heartbeat(
    State(s): State<AppState>,
    actor: Actor,
    Path(id): Path<Uuid>,
) -> Result<Json<Value>> {
    ops::limit(&s, rate_limit::HEARTBEAT, &actor.pial).await?;
    let mut conn = s.pool.acquire().await?;
    let freq = ops::load(&mut conn, id).await?;
    if !freq.state.is_open() {
        return Ok(Json(json!({ "state": freq.state, "present": false })));
    }
    let Some(row) = ops::standing(&mut conn, id, &actor.pial).await? else {
        return Err(AuralisError::Conflict("not_joined"));
    };
    drop(conn);
    s.hot.touch(id, &actor.pial, row.role).await?;
    let (listeners, speakers) = s.hot.counts(id).await?;
    Ok(Json(json!({
        "state": freq.state,
        "present": true,
        "role": row.role,
        "muted": row.muted,
        "counts": { "listeners": listeners, "speakers": speakers, "participants": listeners + speakers },
    })))
}

#[derive(Deserialize, Default)]
pub struct RequestMicBody {
    #[serde(default)]
    pub reason: String,
    /// The requester's Verity tier, as Nantar read it from PIAL state. Nantar
    /// is a service caller; this is not a browser claim.
    pub verity_tier: Option<i16>,
}

pub async fn request_mic(
    State(s): State<AppState>,
    actor: Actor,
    Path(id): Path<Uuid>,
    body: Option<Json<RequestMicBody>>,
) -> Result<(StatusCode, Json<Value>)> {
    ops::limit(&s, rate_limit::REQUEST_MIC, &actor.pial).await?;
    let body = body.map(|Json(b)| b).unwrap_or_default();
    let reason = abuse::reason(&body.reason)?;
    let cause = Cause::person(&actor.pial, &actor.correlation_id);

    let mut conn = s.pool.acquire().await?;
    let freq = ops::load(&mut conn, id).await?;
    ops::require_live(&freq)?;
    let Some(row) = ops::standing(&mut conn, id, &actor.pial).await? else {
        return Err(AuralisError::Conflict("not_joined"));
    };
    if !may(row.role, Action::RequestMic) {
        return Err(AuralisError::Forbidden(
            "only a listener requests the mic".into(),
        ));
    }
    if !freq.requests_open {
        counter!(m::SPEAKER_REQUESTS_TOTAL, "outcome" => "closed").increment(1);
        return Err(AuralisError::Conflict("requests_closed"));
    }
    if body.verity_tier.unwrap_or(0) < freq.speaker_verity_min_tier {
        counter!(m::SPEAKER_REQUESTS_TOTAL, "outcome" => "tier").increment(1);
        return Err(AuralisError::Conflict("verity_tier_too_low"));
    }
    drop(conn);

    let mut tx = s.pool.begin().await?;
    let (sr, created) = match pg::create_request(&mut tx, id, &actor.pial, &reason).await? {
        Some(r) => (r, true),
        None => {
            let existing = pg::pending_request_for(&mut tx, id, &actor.pial)
                .await?
                .ok_or_else(|| {
                    AuralisError::Internal(anyhow::anyhow!(
                        "pending conflict without a pending row"
                    ))
                })?;
            (existing, false)
        }
    };
    if created {
        pg::insert_event(
            &mut tx,
            &cause.event(
                id,
                EventType::SpeakerRequested,
                json!({ "request_id": sr.id, "pial_id": actor.pial_id(), "reason": sr.reason }),
            ),
        )
        .await?;
        counter!(m::SPEAKER_REQUESTS_TOTAL, "outcome" => "queued").increment(1);
    }
    tx.commit().await?;

    let status = if created {
        StatusCode::CREATED
    } else {
        StatusCode::OK
    };
    Ok((
        status,
        Json(json!({ "request": RequestView::from(&sr), "created": created })),
    ))
}

pub async fn withdraw(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, rid)): Path<(Uuid, Uuid)>,
) -> Result<Json<Value>> {
    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    let mut tx = s.pool.begin().await?;
    ops::load(&mut tx, id).await?;
    let sr = pg::get_request(&mut tx, rid)
        .await?
        .ok_or(AuralisError::NotFound)?;
    if sr.frequency_id != id {
        return Err(AuralisError::NotFound);
    }
    if sr.pial != actor.pial {
        return Err(AuralisError::Forbidden("not your request".into()));
    }
    let resolved =
        pg::resolve_request(&mut tx, rid, RequestStatus::Withdrawn, Some(&actor.pial)).await?;
    if let Some(r) = &resolved {
        pg::insert_event(
            &mut tx,
            &cause.event(
                id,
                EventType::SpeakerDeclined,
                json!({ "request_id": r.id, "pial_id": bare_value(&r.pial), "by": "self", "outcome": "withdrawn" }),
            ),
        )
        .await?;
        counter!(m::SPEAKER_REQUESTS_TOTAL, "outcome" => "withdrawn").increment(1);
    }
    tx.commit().await?;
    Ok(Json(json!({ "withdrawn": resolved.is_some() })))
}

pub async fn upvote(
    State(s): State<AppState>,
    actor: Actor,
    Path((id, rid)): Path<(Uuid, Uuid)>,
) -> Result<Json<Value>> {
    ops::limit(&s, rate_limit::UPVOTE, &actor.pial).await?;
    let mut tx = s.pool.begin().await?;
    let freq = ops::load(&mut tx, id).await?;
    ops::require_live(&freq)?;
    if ops::standing(&mut tx, id, &actor.pial).await?.is_none() {
        return Err(AuralisError::Conflict("not_joined"));
    }
    let sr = pg::get_request(&mut tx, rid)
        .await?
        .ok_or(AuralisError::NotFound)?;
    if sr.frequency_id != id || sr.status != RequestStatus::Pending {
        return Err(AuralisError::NotFound);
    }
    if sr.pial == actor.pial {
        return Err(AuralisError::Conflict("own_request"));
    }
    let counted = pg::upvote_request(&mut tx, rid, &actor.pial).await?;
    let after = pg::get_request(&mut tx, rid)
        .await?
        .map(|r| r.upvotes)
        .unwrap_or(sr.upvotes);
    tx.commit().await?;
    Ok(Json(json!({ "counted": counted, "upvotes": after })))
}
