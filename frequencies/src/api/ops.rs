//! Operations shared by handlers and background tasks.
//!
//! Ending a Frequency is the same act whether the host pressed End, the host
//! never came back, or moderation pulled the plug; a participant is marked
//! left the same way whether they pressed Leave or their presence expired.
//! One implementation each, here.

use axum::http::StatusCode;
use metrics::counter;
use serde_json::{json, Value};
use sqlx::PgConnection;
use tracing::{info, warn};
use uuid::Uuid;

use crate::domain::lifecycle;
use crate::domain::{Frequency, FrequencyRole, FrequencyState, Participant};
use crate::error::{AuralisError, Result};
use crate::events::schemas::ACTOR_SERVICE;
use crate::events::{Event, EventType};
use crate::identity::bare_value;
use crate::repository::postgres::{self as pg, Patch};
use crate::state::AppState;
use crate::telemetry::metrics as m;

/// How many times a compare-and-set is retried after losing a race before the
/// conflict is surfaced. Five is far more than any real contention needs; a
/// sixth loss means something is spinning on the row.
pub const CAS_ATTEMPTS: usize = 5;

/// Who caused a change, for the event envelope and the moderation log.
#[derive(Clone, Debug)]
pub struct Cause {
    /// Bare PIAL uuid, or "service".
    pub actor: String,
    pub correlation_id: String,
}

impl Cause {
    pub fn person(pial_name: &str, correlation_id: &str) -> Self {
        Self {
            actor: bare_value(pial_name).to_string(),
            correlation_id: correlation_id.to_string(),
        }
    }

    pub fn service(correlation_id: &str) -> Self {
        Self {
            actor: ACTOR_SERVICE.to_string(),
            correlation_id: correlation_id.to_string(),
        }
    }

    pub fn event(&self, frequency_id: Uuid, t: EventType, payload: Value) -> Event {
        Event::new(
            frequency_id,
            t,
            self.actor.clone(),
            self.correlation_id.clone(),
            payload,
        )
    }
}

/// Load, or 404.
pub async fn load(conn: &mut PgConnection, id: Uuid) -> Result<Frequency> {
    pg::require_frequency(conn, id).await
}

/// The joined participant row for a person, or None if they are not in.
pub async fn standing(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
) -> Result<Option<Participant>> {
    pg::joined_one(conn, id, pial).await
}

/// The role a person holds right now: host by ownership, otherwise their
/// joined row's role. Someone not joined has no role and may do nothing but
/// Tune In.
pub async fn role_of(
    conn: &mut PgConnection,
    freq: &Frequency,
    pial: &str,
) -> Result<Option<FrequencyRole>> {
    if freq.host_pial == pial {
        return Ok(Some(FrequencyRole::Host));
    }
    Ok(standing(conn, freq.id, pial).await?.map(|p| p.role))
}

pub fn require_open(freq: &Frequency) -> Result<()> {
    if freq.state.is_open() {
        Ok(())
    } else {
        Err(AuralisError::Conflict("not_live"))
    }
}

pub fn require_live(freq: &Frequency) -> Result<()> {
    if freq.state == FrequencyState::Live {
        Ok(())
    } else {
        Err(AuralisError::Conflict("not_live"))
    }
}

/// A compare-and-set that re-reads and retries on a lost race. The closure
/// sees the fresh row and either returns a patch, or an error that ends the
/// attempt (an invalid transition, a refusal).
pub async fn cas<F>(
    state: &AppState,
    id: Uuid,
    op: &'static str,
    mut decide: F,
) -> Result<Frequency>
where
    F: FnMut(&Frequency) -> Result<Patch>,
{
    for _ in 0..CAS_ATTEMPTS {
        let mut conn = state.pool.acquire().await?;
        let freq = load(&mut conn, id).await?;
        let patch = decide(&freq)?;
        if let Some(updated) = pg::cas_update(&mut conn, id, freq.version, &patch).await? {
            return Ok(updated);
        }
        counter!(m::VERSION_CONFLICT_TOTAL, "op" => op).increment(1);
    }
    Err(AuralisError::VersionConflict)
}

/// Mark a participant left, in PostgreSQL and Redis, and record the event.
/// `cause_label` is for the leave counter: "leave", "presence_expired",
/// "ended", "removed".
pub async fn depart(
    state: &AppState,
    conn: &mut PgConnection,
    freq: &Frequency,
    pial: &str,
    cause: &Cause,
    cause_label: &'static str,
) -> Result<Option<Participant>> {
    let left = pg::leave(conn, freq.id, pial).await?;
    if let Some(p) = &left {
        let t = if p.role.speaks() {
            EventType::SpeakerLeft
        } else {
            EventType::ListenerLeft
        };
        pg::insert_event(
            conn,
            &cause.event(
                freq.id,
                t,
                json!({ "pial_id": bare_value(pial), "role": p.role, "cause": cause_label }),
            ),
        )
        .await?;
        counter!(m::LEAVE_TOTAL, "cause" => cause_label).increment(1);
    }
    state.hot.forget(freq.id, pial).await?;
    Ok(left)
}

/// End a Frequency (§50). Idempotent: a Frequency already past `live`
/// returns as it is, so two simultaneous End requests produce one ending.
///
/// Phase one: `live → ending` under compare-and-set, refusing new joins from
/// that moment. Phase two: close the session — presence, requests, lease —
/// then `ending → <terminal>` with the event. A crash between the phases
/// leaves `ending`, which boot reconciliation completes.
pub async fn end(
    state: &AppState,
    id: Uuid,
    cause: &Cause,
    reason: &str,
    terminal: FrequencyState,
) -> Result<Frequency> {
    debug_assert!(matches!(
        terminal,
        FrequencyState::Ended | FrequencyState::ModerationTerminated
    ));

    let freq = {
        let mut conn = state.pool.acquire().await?;
        load(&mut conn, id).await?
    };
    if freq.state.is_over() {
        // Already over: nothing to do, answer as it is.
        return Ok(freq);
    }
    if freq.state == FrequencyState::Ending {
        // A previous attempt (or a crash) got half way. Finish it.
        return complete_ending(state, &freq, cause, reason).await;
    }

    // Moderation skips the graceful tail: it is one hop to terminal.
    let to = if terminal == FrequencyState::ModerationTerminated {
        FrequencyState::ModerationTerminated
    } else {
        FrequencyState::Ending
    };

    // Phase one, under compare-and-set. Losing the race to another End is
    // not an error: whoever won is completing it, and this call answers with
    // the row as it stands.
    let moved = match cas(state, id, "end_phase1", |f| {
        if f.state.is_over() || f.state == FrequencyState::Ending {
            return Err(AuralisError::Conflict("already_ending"));
        }
        lifecycle::transition(f.state, to)?;
        let mut p = Patch::state(to);
        if to == FrequencyState::ModerationTerminated {
            p.ended_at = Some(Some(chrono::Utc::now()));
            p.end_reason = Some(Some(reason.to_string()));
        }
        Ok(p)
    })
    .await
    {
        Ok(f) => f,
        Err(AuralisError::Conflict("already_ending")) => {
            let mut conn = state.pool.acquire().await?;
            let f = load(&mut conn, id).await?;
            if f.state == FrequencyState::Ending {
                // Both callers may run phase two; it is compare-and-set safe.
                return complete_ending(state, &f, cause, reason).await;
            }
            return Ok(f);
        }
        Err(e) => return Err(e),
    };

    complete_ending(state, &moved, cause, reason).await
}

/// Refuse above a limit, counting the refusal.
pub async fn limit(
    state: &AppState,
    l: crate::security::rate_limit::Limit,
    key: &str,
) -> Result<()> {
    let n = state.hot.hit(l.scope, key, l.window).await?;
    if crate::security::rate_limit::allowed(n, l) {
        Ok(())
    } else {
        counter!(m::RATE_LIMITED_TOTAL, "scope" => l.scope).increment(1);
        Err(AuralisError::RateLimited(l.scope))
    }
}

/// Run a mutation once per (actor, Idempotency-Key). A replay answers with
/// what the first run answered and produces no second effect. Only a
/// completed run is stored; an error is not, so a retry after a failure
/// really retries.
pub async fn idempotent<F, Fut>(
    state: &AppState,
    actor: &str,
    key: Option<String>,
    run: F,
) -> Result<(StatusCode, axum::Json<Value>)>
where
    F: FnOnce() -> Fut,
    Fut: std::future::Future<Output = Result<Answer>>,
{
    if let Some(k) = &key {
        let mut conn = state.pool.acquire().await?;
        if let Some((status, body)) = pg::idempotent_get(&mut conn, actor, k).await? {
            counter!(m::IDEMPOTENT_REPLAY_TOTAL).increment(1);
            let status = StatusCode::from_u16(status as u16).unwrap_or(StatusCode::OK);
            return Ok((status, axum::Json(body)));
        }
    }
    let (status, body) = run().await?;
    if let Some(k) = &key {
        let mut conn = state.pool.acquire().await?;
        pg::idempotent_put(&mut conn, actor, k, status.as_u16() as i16, &body).await?;
    }
    Ok((status, axum::Json(body)))
}

/// Phase two of ending, also run by boot reconciliation for rows found in
/// `ending`, and directly for `moderation_terminated` rows that still have a
/// session to close.
pub async fn complete_ending(
    state: &AppState,
    freq: &Frequency,
    cause: &Cause,
    reason: &str,
) -> Result<Frequency> {
    let mut tx = state.pool.begin().await?;

    let departed = pg::leave_all(&mut tx, freq.id).await?;
    let expired = pg::expire_requests(&mut tx, freq.id).await?;

    let (t, final_state, final_reason) = match freq.state {
        FrequencyState::ModerationTerminated => (
            EventType::ModerationTerminated,
            FrequencyState::ModerationTerminated,
            freq.end_reason
                .clone()
                .unwrap_or_else(|| reason.to_string()),
        ),
        _ => (EventType::Ended, FrequencyState::Ended, reason.to_string()),
    };

    let updated = if freq.state == FrequencyState::Ending {
        lifecycle::transition(FrequencyState::Ending, FrequencyState::Ended)?;
        let mut p = Patch::state(FrequencyState::Ended);
        p.ended_at = Some(Some(chrono::Utc::now()));
        p.end_reason = Some(Some(final_reason.clone()));
        p.media_node = Some(None);
        match pg::cas_update(&mut tx, freq.id, freq.version, &p).await? {
            Some(f) => f,
            None => {
                // Somebody completed it between our read and now. Their
                // transaction wrote the event; ours writes nothing.
                tx.rollback().await?;
                let mut conn = state.pool.acquire().await?;
                return load(&mut conn, freq.id).await;
            }
        }
    } else {
        let p = Patch {
            media_node: Some(None),
            ..Default::default()
        };
        pg::cas_update(&mut tx, freq.id, freq.version, &p)
            .await?
            .unwrap_or_else(|| freq.clone())
    };

    pg::insert_event(
        &mut tx,
        &cause.event(
            freq.id,
            t,
            json!({
                "reason": final_reason,
                "state": final_state,
                "started_at": updated.started_at,
                "ended_at": updated.ended_at,
                "participants_departed": departed,
                "requests_expired": expired,
            }),
        ),
    )
    .await?;
    if final_state == FrequencyState::ModerationTerminated {
        pg::log_moderation(
            &mut tx,
            freq.id,
            &cause.actor,
            None,
            "terminate",
            &final_reason,
        )
        .await?;
    }
    tx.commit().await?;

    // Transient state last: if this fails the durable end already happened
    // and the sweeper will find nothing live to keep these for.
    if let Err(e) = state.hot.clear(freq.id).await {
        warn!(frequency_id = %freq.id, error = %e, "clearing hot state after end");
    }
    if let Err(e) = state.hot.release_lease(freq.id).await {
        warn!(frequency_id = %freq.id, error = %e, "releasing lease after end");
    }

    counter!(m::FREQUENCIES_ENDED_TOTAL, "reason" => final_reason.clone()).increment(1);
    info!(
        frequency_id = %freq.id,
        state = %final_state,
        reason = %final_reason,
        departed,
        "frequency ended"
    );
    Ok(updated)
}

/// Fail a Frequency that never made it out of `starting`.
pub async fn fail_starting(
    state: &AppState,
    freq: &Frequency,
    cause: &Cause,
    reason: &str,
) -> Result<()> {
    let mut tx = state.pool.begin().await?;
    lifecycle::transition(freq.state, FrequencyState::Failed)?;
    let mut p = Patch::state(FrequencyState::Failed);
    p.ended_at = Some(Some(chrono::Utc::now()));
    p.end_reason = Some(Some(reason.to_string()));
    p.media_node = Some(None);
    if pg::cas_update(&mut tx, freq.id, freq.version, &p)
        .await?
        .is_none()
    {
        tx.rollback().await?;
        return Ok(());
    }
    pg::leave_all(&mut tx, freq.id).await?;
    pg::insert_event(
        &mut tx,
        &cause.event(freq.id, EventType::Failed, json!({ "reason": reason })),
    )
    .await?;
    tx.commit().await?;
    let _ = state.hot.clear(freq.id).await;
    let _ = state.hot.release_lease(freq.id).await;
    counter!(m::FREQUENCIES_ENDED_TOTAL, "reason" => "failed").increment(1);
    Ok(())
}

/// Answer shape for a mutation: status plus the JSON the idempotency store
/// will replay.
pub type Answer = (StatusCode, Value);
