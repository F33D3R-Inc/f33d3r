//! The Frequency object: create, read, schedule, start, end, and the host's
//! switches.

use axum::{
    extract::{Path, Query, State},
    http::{HeaderMap, StatusCode},
    Json,
};
use chrono::{DateTime, Duration as ChronoDuration, Utc};
use metrics::counter;
use serde::Deserialize;
use serde_json::{json, Value};
use uuid::Uuid;

use super::ops::{self, Cause};
use super::view::{self, FrequencySummary};
use crate::auth::{optional_viewer, Actor, Service};
use crate::domain::frequency::{MAX_LISTENERS_CAP, MAX_SPEAKERS_CAP};
use crate::domain::lifecycle;
use crate::domain::{Action, FrequencyRole, FrequencyState, Visibility};
use crate::error::{AuralisError, Result};
use crate::events::EventType;
use crate::identity;
use crate::repository::postgres::{self as pg, NewFrequency, Patch};
use crate::security::rate_limit;
use crate::security::{abuse, tokens};
use crate::state::AppState;
use crate::telemetry::metrics as m;

fn idem_key(headers: &HeaderMap) -> Result<Option<String>> {
    abuse::idempotency_key(headers.get("Idempotency-Key").and_then(|v| v.to_str().ok()))
}

fn require_host(freq: &crate::domain::Frequency, actor: &Actor) -> Result<()> {
    if freq.host_pial == actor.pial {
        Ok(())
    } else {
        Err(AuralisError::Forbidden("only the host may do this".into()))
    }
}

/// Prove, from the stored role, that the actor may attempt `action` here.
async fn require_action(s: &AppState, id: Uuid, actor: &Actor, action: Action) -> Result<()> {
    let mut conn = s.pool.acquire().await?;
    let f = ops::load(&mut conn, id).await?;
    let role = ops::role_of(&mut conn, &f, &actor.pial).await?;
    match role {
        Some(r) if crate::domain::role::may(r, action) => Ok(()),
        Some(r) => Err(AuralisError::Forbidden(format!("{r} may not {action:?}"))),
        None => Err(AuralisError::Forbidden("not in this frequency".into())),
    }
}

fn scheduled_time(t: DateTime<Utc>) -> Result<DateTime<Utc>> {
    let now = Utc::now();
    if t < now - ChronoDuration::minutes(1) {
        return Err(AuralisError::BadRequest(
            "scheduled_at is in the past".into(),
        ));
    }
    if t > now + ChronoDuration::days(30) {
        return Err(AuralisError::BadRequest(
            "scheduled_at is more than 30 days out".into(),
        ));
    }
    Ok(t)
}

fn bounded(v: Option<i32>, default: i32, cap: i32, field: &str) -> Result<i32> {
    match v {
        None => Ok(default),
        Some(n) if (1..=cap).contains(&n) => Ok(n),
        Some(_) => Err(AuralisError::BadRequest(format!(
            "{field} must be between 1 and {cap}"
        ))),
    }
}

/// The session a browser needs to reach the media plane: minted here, burned
/// on first use at the signaling socket.
pub fn session_for(s: &AppState, freq_id: Uuid, pial: &str, role: FrequencyRole) -> Value {
    let now = tokens::now_unix();
    let claims = tokens::Claims {
        frequency_id: freq_id,
        pial_id: identity::bare_value(pial).to_string(),
        session_id: Uuid::new_v4(),
        role,
        permissions: tokens::Claims::permissions_for(role),
        iat: now,
        exp: now + s.config.signal_token_ttl.as_secs() as i64,
        nonce: tokens::new_nonce(),
        node_id: s.config.node_id.clone(),
    };
    json!({
        "session_id": claims.session_id,
        "token": s.signer.mint(&claims),
        "expires_at": claims.exp,
        "signal_path": format!("/frequencies/signal/{freq_id}"),
        "role": role,
        "permissions": claims.permissions,
    })
}

// ── create ───────────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct CreateBody {
    pub title: String,
    #[serde(default)]
    pub description: String,
    pub visibility: Option<String>,
    pub language: Option<String>,
    #[serde(default)]
    pub adult_content: bool,
    #[serde(default)]
    pub recording_enabled: bool,
    pub scheduled_at: Option<DateTime<Utc>>,
    pub max_speakers: Option<i32>,
    pub max_listeners: Option<i32>,
    pub speaker_verity_min_tier: Option<i16>,
}

pub async fn create(
    State(s): State<AppState>,
    actor: Actor,
    headers: HeaderMap,
    Json(body): Json<CreateBody>,
) -> Result<(StatusCode, Json<Value>)> {
    ops::limit(&s, rate_limit::CREATE, &actor.pial).await?;
    let key = idem_key(&headers)?;

    let title = abuse::title(&body.title)?;
    let description = abuse::description(&body.description)?;
    let language = abuse::language(body.language.as_deref().unwrap_or("en"))?;
    let visibility = match body.visibility.as_deref() {
        None => Visibility::Public,
        Some(v) => Visibility::parse(v).ok_or_else(|| {
            AuralisError::BadRequest(
                "visibility must be public, followers, subscribers or private".into(),
            )
        })?,
    };
    let scheduled_at = body.scheduled_at.map(scheduled_time).transpose()?;
    let max_speakers = bounded(body.max_speakers, 10, MAX_SPEAKERS_CAP, "max_speakers")?;
    let max_listeners = bounded(body.max_listeners, 1000, MAX_LISTENERS_CAP, "max_listeners")?;
    let tier = body.speaker_verity_min_tier.unwrap_or(0);
    if !(0..=3).contains(&tier) {
        return Err(AuralisError::BadRequest(
            "speaker_verity_min_tier must be 0..3".into(),
        ));
    }

    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    ops::idempotent(&s, &actor.pial, key, || async {
        let new = NewFrequency {
            host_pial: actor.pial.clone(),
            title,
            description,
            state: if scheduled_at.is_some() { FrequencyState::Scheduled } else { FrequencyState::Draft },
            visibility,
            language,
            adult_content: body.adult_content,
            speaker_verity_min_tier: tier,
            scheduled_at,
            recording_enabled: body.recording_enabled,
            max_speakers,
            max_listeners,
        };
        let mut tx = s.pool.begin().await?;
        let freq = pg::insert_frequency(&mut tx, &new).await?;
        pg::insert_event(
            &mut tx,
            &cause.event(
                freq.id,
                EventType::Created,
                json!({
                    "title": freq.title, "visibility": freq.visibility, "language": freq.language,
                    "adult_content": freq.adult_content, "recording_enabled": freq.recording_enabled,
                    "scheduled_at": freq.scheduled_at, "state": freq.state,
                }),
            ),
        )
        .await?;
        if let Some(at) = freq.scheduled_at {
            pg::insert_event(&mut tx, &cause.event(freq.id, EventType::Scheduled, json!({ "scheduled_at": at }))).await?;
        }
        tx.commit().await?;
        counter!(m::FREQUENCIES_CREATED_TOTAL).increment(1);

        let mut conn = s.pool.acquire().await?;
        let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
        Ok((StatusCode::CREATED, json!({ "frequency": v })))
    })
    .await
}

// ── read ─────────────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct ListQuery {
    pub lane: Option<String>,
    pub limit: Option<i64>,
}

pub async fn list(
    State(s): State<AppState>,
    _svc: Service,
    headers: HeaderMap,
    Query(q): Query<ListQuery>,
) -> Result<Json<Value>> {
    let limit = q.limit.unwrap_or(24).clamp(1, 100);
    let viewer = optional_viewer(&s, &headers).await?;
    let rows = match q.lane.as_deref().unwrap_or("live") {
        "live" => pg::list_live(&s.pool, limit).await?,
        "scheduled" => pg::list_scheduled(&s.pool, limit).await?,
        "ended" => pg::list_ended(&s.pool, limit).await?,
        "mine" => match &viewer {
            Some(v) => pg::list_by_host(&s.pool, v, limit).await?,
            None => {
                return Err(AuralisError::BadRequest(
                    "lane=mine needs X-Pial-Identity".into(),
                ))
            }
        },
        other => return Err(AuralisError::BadRequest(format!("unknown lane {other:?}"))),
    };
    let mut items = Vec::with_capacity(rows.len());
    for f in &rows {
        let (listeners, speakers) = if f.state.is_open() {
            s.hot.counts(f.id).await?
        } else {
            (0, 0)
        };
        items.push(json!({
            "frequency": FrequencySummary::from(f),
            "counts": { "listeners": listeners, "speakers": speakers, "participants": listeners + speakers },
        }));
    }
    Ok(Json(
        json!({ "lane": q.lane.unwrap_or_else(|| "live".into()), "items": items }),
    ))
}

pub async fn get(
    State(s): State<AppState>,
    _svc: Service,
    headers: HeaderMap,
    Path(id): Path<Uuid>,
) -> Result<Json<Value>> {
    let viewer = optional_viewer(&s, &headers).await?;
    let mut conn = s.pool.acquire().await?;
    let freq = ops::load(&mut conn, id).await?;
    let v = view::build(&mut conn, &s.hot, &freq, viewer.as_deref()).await?;
    Ok(Json(json!({ "frequency": v })))
}

pub async fn events(
    State(s): State<AppState>,
    _svc: Service,
    Path(id): Path<Uuid>,
) -> Result<Json<Value>> {
    let mut conn = s.pool.acquire().await?;
    ops::load(&mut conn, id).await?;
    let evs = pg::list_events(&s.pool, id, 200).await?;
    Ok(Json(json!({ "events": evs })))
}

pub async fn host_open(
    State(s): State<AppState>,
    _svc: Service,
    Path(pial): Path<String>,
) -> Result<Json<Value>> {
    let name = identity::resolve(&s.manhattan, &pial).await?;
    let mut conn = s.pool.acquire().await?;
    let open = pg::open_for_host(&mut conn, &name).await?;
    Ok(Json(
        json!({ "frequency": open.as_ref().map(FrequencySummary::from) }),
    ))
}

/// The live Frequency this person is in, as their own viewer-specific view,
/// or null. Nantar calls this once per full page render so the Shell can
/// render the dock wherever the person navigates.
pub async fn participant_current(
    State(s): State<AppState>,
    _svc: Service,
    Path(pial): Path<String>,
) -> Result<Json<Value>> {
    let name = identity::resolve(&s.manhattan, &pial).await?;
    let mut conn = s.pool.acquire().await?;
    let Some(freq) = pg::current_for_participant(&mut conn, &name).await? else {
        return Ok(Json(json!({ "frequency": null })));
    };
    let v = view::build(&mut conn, &s.hot, &freq, Some(&name)).await?;
    Ok(Json(json!({ "frequency": v })))
}

// ── host mutations ───────────────────────────────────────────────────────────

#[derive(Deserialize, Default)]
pub struct UpdateBody {
    pub title: Option<String>,
    pub description: Option<String>,
    pub visibility: Option<String>,
    pub language: Option<String>,
    pub adult_content: Option<bool>,
    pub recording_enabled: Option<bool>,
    pub max_speakers: Option<i32>,
    pub max_listeners: Option<i32>,
    pub speaker_verity_min_tier: Option<i16>,
}

pub async fn update(
    State(s): State<AppState>,
    actor: Actor,
    Path(id): Path<Uuid>,
    Json(body): Json<UpdateBody>,
) -> Result<Json<Value>> {
    let mut patch = Patch::default();
    if let Some(t) = &body.title {
        patch.title = Some(abuse::title(t)?);
    }
    if let Some(d) = &body.description {
        patch.description = Some(abuse::description(d)?);
    }
    if let Some(v) = &body.visibility {
        patch.visibility = Some(
            Visibility::parse(v)
                .ok_or_else(|| AuralisError::BadRequest("bad visibility".into()))?,
        );
    }
    if let Some(l) = &body.language {
        patch.language = Some(abuse::language(l)?);
    }
    patch.adult_content = body.adult_content;
    patch.max_speakers = body
        .max_speakers
        .map(|n| bounded(Some(n), 10, MAX_SPEAKERS_CAP, "max_speakers"))
        .transpose()?;
    patch.max_listeners = body
        .max_listeners
        .map(|n| bounded(Some(n), 1000, MAX_LISTENERS_CAP, "max_listeners"))
        .transpose()?;
    if let Some(t) = body.speaker_verity_min_tier {
        if !(0..=3).contains(&t) {
            return Err(AuralisError::BadRequest(
                "speaker_verity_min_tier must be 0..3".into(),
            ));
        }
        patch.speaker_verity_min_tier = Some(t);
    }

    let freq = ops::cas(&s, id, "update", |f| {
        require_host(f, &actor)?;
        if f.state.is_over() {
            return Err(AuralisError::Conflict("over"));
        }
        // Recording cannot be switched on mid-session without the consent
        // indicator path (§25); it is a pre-start decision until Phase 5.
        let recording_enabled = match body.recording_enabled {
            Some(r) if f.state.is_open() && r != f.recording_enabled => {
                return Err(AuralisError::Conflict("recording_locked_while_live"));
            }
            other => other,
        };
        Ok(Patch {
            title: patch.title.clone(),
            description: patch.description.clone(),
            visibility: patch.visibility,
            language: patch.language.clone(),
            adult_content: patch.adult_content,
            max_speakers: patch.max_speakers,
            max_listeners: patch.max_listeners,
            speaker_verity_min_tier: patch.speaker_verity_min_tier,
            recording_enabled,
            ..Default::default()
        })
    })
    .await?;
    let mut conn = s.pool.acquire().await?;
    let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
    Ok(Json(json!({ "frequency": v })))
}

#[derive(Deserialize)]
pub struct ScheduleBody {
    pub scheduled_at: Option<DateTime<Utc>>,
}

pub async fn schedule(
    State(s): State<AppState>,
    actor: Actor,
    Path(id): Path<Uuid>,
    Json(body): Json<ScheduleBody>,
) -> Result<Json<Value>> {
    let at = body.scheduled_at.map(scheduled_time).transpose()?;
    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    let freq = ops::cas(&s, id, "schedule", |f| {
        require_host(f, &actor)?;
        let to = if at.is_some() {
            FrequencyState::Scheduled
        } else {
            FrequencyState::Draft
        };
        if f.state != to {
            lifecycle::transition(f.state, to)?;
        }
        let mut p = Patch::state(to);
        p.scheduled_at = Some(at);
        Ok(p)
    })
    .await?;
    let mut tx = s.pool.begin().await?;
    pg::insert_event(
        &mut tx,
        &cause.event(freq.id, EventType::Scheduled, json!({ "scheduled_at": at })),
    )
    .await?;
    tx.commit().await?;
    let mut conn = s.pool.acquire().await?;
    let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
    Ok(Json(json!({ "frequency": v })))
}

/// Start (§48): validate host, allocate the media session (the lease), move
/// `starting`, seat the host, move `live`, publish `frequency.started`.
pub async fn start(
    State(s): State<AppState>,
    actor: Actor,
    headers: HeaderMap,
    Path(id): Path<Uuid>,
) -> Result<(StatusCode, Json<Value>)> {
    ops::limit(&s, rate_limit::START, &actor.pial).await?;
    let key = idem_key(&headers)?;
    let cause = Cause::person(&actor.pial, &actor.correlation_id);

    ops::idempotent(&s, &actor.pial, key, || async {
        // Already live: answer with it. Pressing Start twice is not two
        // sessions.
        {
            let mut conn = s.pool.acquire().await?;
            let f = ops::load(&mut conn, id).await?;
            require_host(&f, &actor)?;
            if f.state == FrequencyState::Live {
                let v = view::build(&mut conn, &s.hot, &f, Some(&actor.pial)).await?;
                let session = session_for(&s, f.id, &actor.pial, FrequencyRole::Host);
                return Ok((StatusCode::OK, json!({ "frequency": v, "session": session })));
            }
            if let Some(open) = pg::open_for_host(&mut conn, &actor.pial).await? {
                if open.id != id {
                    return Err(AuralisError::Conflict("host_already_live"));
                }
            }
        }

        let starting = ops::cas(&s, id, "start", |f| {
            require_host(f, &actor)?;
            lifecycle::transition(f.state, FrequencyState::Starting)?;
            let mut p = Patch::state(FrequencyState::Starting);
            p.media_node = Some(Some(s.config.node_id.clone()));
            Ok(p)
        })
        .await?;

        // Media allocation. Phase 1 has no SFU; the allocation is the
        // ownership lease, which is what Phase 3's socket will check.
        if !s.hot.acquire_lease(id).await? {
            ops::fail_starting(&s, &starting, &cause, "lease_unavailable").await?;
            return Err(AuralisError::Conflict("media_unavailable"));
        }

        let mut tx = s.pool.begin().await?;
        let live = match pg::cas_update(
            &mut tx,
            id,
            starting.version,
            &{
                let mut p = Patch::state(FrequencyState::Live);
                p.started_at = Some(Some(Utc::now()));
                p
            },
        )
        .await?
        {
            Some(f) => f,
            None => {
                tx.rollback().await?;
                return Err(AuralisError::VersionConflict);
            }
        };
        let (host_row, _) = pg::join(&mut tx, id, &actor.pial, FrequencyRole::Host).await?;
        pg::insert_event(
            &mut tx,
            &cause.event(
                id,
                EventType::Started,
                json!({
                    "title": live.title, "host_pial_id": actor.pial_id(), "started_at": live.started_at,
                    "visibility": live.visibility, "adult_content": live.adult_content,
                    "recording_enabled": live.recording_enabled, "media_node": live.media_node,
                }),
            ),
        )
        .await?;
        if live.recording_enabled {
            pg::insert_event(&mut tx, &cause.event(id, EventType::RecordingStarted, json!({ "consent_shown": true }))).await?;
        }
        tx.commit().await?;

        s.hot.touch(id, &actor.pial, host_row.role).await?;
        counter!(m::JOIN_TOTAL, "role" => "host").increment(1);

        let mut conn = s.pool.acquire().await?;
        let v = view::build(&mut conn, &s.hot, &live, Some(&actor.pial)).await?;
        let session = session_for(&s, live.id, &actor.pial, FrequencyRole::Host);
        Ok((StatusCode::OK, json!({ "frequency": v, "session": session })))
    })
    .await
}

pub async fn end(
    State(s): State<AppState>,
    actor: Actor,
    headers: HeaderMap,
    Path(id): Path<Uuid>,
) -> Result<(StatusCode, Json<Value>)> {
    let key = idem_key(&headers)?;
    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    ops::idempotent(&s, &actor.pial, key, || async {
        {
            let mut conn = s.pool.acquire().await?;
            let f = ops::load(&mut conn, id).await?;
            let _ = f;
        }
        require_action(&s, id, &actor, Action::EndFrequency).await?;
        let freq = ops::end(&s, id, &cause, "host_ended", FrequencyState::Ended).await?;
        let mut conn = s.pool.acquire().await?;
        let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
        Ok((StatusCode::OK, json!({ "frequency": v })))
    })
    .await
}

pub async fn cancel(
    State(s): State<AppState>,
    actor: Actor,
    Path(id): Path<Uuid>,
) -> Result<Json<Value>> {
    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    let freq = ops::cas(&s, id, "cancel", |f| {
        require_host(f, &actor)?;
        if f.state == FrequencyState::Cancelled {
            return Err(AuralisError::Conflict("already_cancelled"));
        }
        lifecycle::transition(f.state, FrequencyState::Cancelled)?;
        let mut p = Patch::state(FrequencyState::Cancelled);
        p.ended_at = Some(Some(Utc::now()));
        p.end_reason = Some(Some("cancelled".into()));
        Ok(p)
    })
    .await;
    let freq = match freq {
        Ok(f) => {
            let mut tx = s.pool.begin().await?;
            pg::insert_event(&mut tx, &cause.event(f.id, EventType::Cancelled, json!({}))).await?;
            tx.commit().await?;
            f
        }
        Err(AuralisError::Conflict("already_cancelled")) => {
            let mut conn = s.pool.acquire().await?;
            ops::load(&mut conn, id).await?
        }
        Err(e) => return Err(e),
    };
    let mut conn = s.pool.acquire().await?;
    let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
    Ok(Json(json!({ "frequency": v })))
}

#[derive(Deserialize)]
pub struct LockBody {
    pub locked: bool,
}

pub async fn lock(
    State(s): State<AppState>,
    actor: Actor,
    Path(id): Path<Uuid>,
    Json(body): Json<LockBody>,
) -> Result<Json<Value>> {
    ops::limit(&s, rate_limit::MODERATION, &actor.pial).await?;
    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    require_action(&s, id, &actor, Action::LockFrequency).await?;
    let freq = ops::cas(&s, id, "lock", |f| {
        ops::require_open(f)?;
        Ok(Patch {
            locked: Some(body.locked),
            ..Default::default()
        })
    })
    .await?;
    let mut tx = s.pool.begin().await?;
    pg::insert_event(
        &mut tx,
        &cause.event(freq.id, EventType::Locked, json!({ "locked": body.locked })),
    )
    .await?;
    pg::log_moderation(
        &mut tx,
        freq.id,
        &cause.actor,
        None,
        if body.locked { "lock" } else { "unlock" },
        "",
    )
    .await?;
    tx.commit().await?;
    counter!(m::MODERATION_ACTIONS_TOTAL, "action" => if body.locked { "lock" } else { "unlock" })
        .increment(1);
    let mut conn = s.pool.acquire().await?;
    let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
    Ok(Json(json!({ "frequency": v })))
}

#[derive(Deserialize)]
pub struct RequestsOpenBody {
    pub open: bool,
}

pub async fn requests_open(
    State(s): State<AppState>,
    actor: Actor,
    Path(id): Path<Uuid>,
    Json(body): Json<RequestsOpenBody>,
) -> Result<Json<Value>> {
    ops::limit(&s, rate_limit::MODERATION, &actor.pial).await?;
    let cause = Cause::person(&actor.pial, &actor.correlation_id);
    require_action(&s, id, &actor, Action::ToggleRequests).await?;
    let freq = ops::cas(&s, id, "requests_open", |f| {
        ops::require_open(f)?;
        Ok(Patch {
            requests_open: Some(body.open),
            ..Default::default()
        })
    })
    .await?;
    let mut tx = s.pool.begin().await?;
    pg::insert_event(
        &mut tx,
        &cause.event(
            freq.id,
            EventType::RequestsToggled,
            json!({ "open": body.open }),
        ),
    )
    .await?;
    pg::log_moderation(
        &mut tx,
        freq.id,
        &cause.actor,
        None,
        if body.open {
            "requests_opened"
        } else {
            "requests_closed"
        },
        "",
    )
    .await?;
    tx.commit().await?;
    let mut conn = s.pool.acquire().await?;
    let v = view::build(&mut conn, &s.hot, &freq, Some(&actor.pial)).await?;
    Ok(Json(json!({ "frequency": v })))
}
