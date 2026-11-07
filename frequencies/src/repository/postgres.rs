//! Durable state. Every mutation of a Frequency is a compare-and-set on its
//! `version`, and every one that matters writes its event in the same
//! transaction.
//!
//! Functions take `&mut PgConnection` (a transaction or a plain connection)
//! where they may be part of a larger unit of work, and `&PgPool` where they
//! are a standalone read.

use chrono::{DateTime, Utc};
use serde_json::Value;
use sqlx::{PgConnection, PgPool, Postgres, QueryBuilder};
use uuid::Uuid;

use crate::domain::{
    Frequency, FrequencyRole, FrequencyState, Participant, ReplayStatus, RequestStatus,
    SpeakerRequest, Visibility,
};
use crate::error::{AuralisError, Result};
use crate::events::Event;
use crate::telemetry::metrics::Timed;

// ── frequencies ──────────────────────────────────────────────────────────────

pub struct NewFrequency {
    pub host_pial: String,
    pub title: String,
    pub description: String,
    pub state: FrequencyState,
    pub visibility: Visibility,
    pub language: String,
    pub adult_content: bool,
    pub speaker_verity_min_tier: i16,
    pub scheduled_at: Option<DateTime<Utc>>,
    pub recording_enabled: bool,
    pub max_speakers: i32,
    pub max_listeners: i32,
}

pub async fn insert_frequency(conn: &mut PgConnection, f: &NewFrequency) -> Result<Frequency> {
    let _t = Timed::db("insert_frequency");
    let row = sqlx::query_as::<_, Frequency>(
        "INSERT INTO frequencies
            (host_pial, title, description, state, visibility, language, adult_content,
             speaker_verity_min_tier, scheduled_at, recording_enabled, max_speakers, max_listeners)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
         RETURNING *",
    )
    .bind(&f.host_pial)
    .bind(&f.title)
    .bind(&f.description)
    .bind(f.state.as_str())
    .bind(f.visibility.as_str())
    .bind(&f.language)
    .bind(f.adult_content)
    .bind(f.speaker_verity_min_tier)
    .bind(f.scheduled_at)
    .bind(f.recording_enabled)
    .bind(f.max_speakers)
    .bind(f.max_listeners)
    .fetch_one(conn)
    .await?;
    Ok(row)
}

pub async fn get_frequency(conn: &mut PgConnection, id: Uuid) -> Result<Option<Frequency>> {
    let _t = Timed::db("get_frequency");
    Ok(
        sqlx::query_as::<_, Frequency>("SELECT * FROM frequencies WHERE id = $1")
            .bind(id)
            .fetch_optional(conn)
            .await?,
    )
}

pub async fn require_frequency(conn: &mut PgConnection, id: Uuid) -> Result<Frequency> {
    get_frequency(conn, id).await?.ok_or(AuralisError::NotFound)
}

/// Fields a compare-and-set may change. `None` leaves a column alone;
/// `Some(None)` on an optional column sets it NULL.
#[derive(Default)]
pub struct Patch {
    pub state: Option<FrequencyState>,
    pub title: Option<String>,
    pub description: Option<String>,
    pub visibility: Option<Visibility>,
    pub language: Option<String>,
    pub adult_content: Option<bool>,
    pub speaker_verity_min_tier: Option<i16>,
    pub scheduled_at: Option<Option<DateTime<Utc>>>,
    pub started_at: Option<Option<DateTime<Utc>>>,
    pub ended_at: Option<Option<DateTime<Utc>>>,
    pub end_reason: Option<Option<String>>,
    pub recording_enabled: Option<bool>,
    pub replay_status: Option<ReplayStatus>,
    pub max_speakers: Option<i32>,
    pub max_listeners: Option<i32>,
    pub requests_open: Option<bool>,
    pub locked: Option<bool>,
    pub clips_enabled: Option<bool>,
    pub media_node: Option<Option<String>>,
    pub host_pial: Option<String>,
}

impl Patch {
    pub fn state(s: FrequencyState) -> Self {
        Self {
            state: Some(s),
            ..Default::default()
        }
    }
}

/// The compare-and-set. Returns the updated row, or `None` when `version`
/// did not match — the caller re-reads and decides again (§51).
pub async fn cas_update(
    conn: &mut PgConnection,
    id: Uuid,
    version: i64,
    p: &Patch,
) -> Result<Option<Frequency>> {
    let _t = Timed::db("cas_update");
    let mut qb: QueryBuilder<Postgres> =
        QueryBuilder::new("UPDATE frequencies SET version = version + 1, updated_at = NOW()");

    macro_rules! set {
        ($field:expr, $col:literal) => {
            if let Some(v) = &$field {
                qb.push(concat!(", ", $col, " = ")).push_bind(v.clone());
            }
        };
    }

    if let Some(s) = p.state {
        qb.push(", state = ").push_bind(s.as_str());
    }
    set!(p.title, "title");
    set!(p.description, "description");
    if let Some(v) = p.visibility {
        qb.push(", visibility = ").push_bind(v.as_str());
    }
    set!(p.language, "language");
    set!(p.adult_content, "adult_content");
    set!(p.speaker_verity_min_tier, "speaker_verity_min_tier");
    set!(p.scheduled_at, "scheduled_at");
    set!(p.started_at, "started_at");
    set!(p.ended_at, "ended_at");
    set!(p.end_reason, "end_reason");
    set!(p.recording_enabled, "recording_enabled");
    if let Some(r) = p.replay_status {
        let s = match r {
            ReplayStatus::None => "none",
            ReplayStatus::Processing => "processing",
            ReplayStatus::Ready => "ready",
            ReplayStatus::Failed => "failed",
            ReplayStatus::Deleted => "deleted",
        };
        qb.push(", replay_status = ").push_bind(s);
    }
    set!(p.max_speakers, "max_speakers");
    set!(p.max_listeners, "max_listeners");
    set!(p.requests_open, "requests_open");
    set!(p.locked, "locked");
    set!(p.clips_enabled, "clips_enabled");
    set!(p.media_node, "media_node");
    set!(p.host_pial, "host_pial");

    qb.push(" WHERE id = ")
        .push_bind(id)
        .push(" AND version = ")
        .push_bind(version)
        .push(" RETURNING *");

    Ok(qb
        .build_query_as::<Frequency>()
        .fetch_optional(conn)
        .await?)
}

pub async fn open_for_host(conn: &mut PgConnection, host_pial: &str) -> Result<Option<Frequency>> {
    let _t = Timed::db("open_for_host");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies
          WHERE host_pial = $1 AND state IN ('starting', 'live', 'ending')
          ORDER BY created_at DESC LIMIT 1",
    )
    .bind(host_pial)
    .fetch_optional(conn)
    .await?)
}

/// The live Frequency this person is joined to right now, if any. At most one:
/// a person is joined to one Frequency at a time by the partial unique index
/// on participants, and only live Frequencies count for the dock.
pub async fn current_for_participant(
    conn: &mut PgConnection,
    pial: &str,
) -> Result<Option<Frequency>> {
    let _t = Timed::db("current_for_participant");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT f.* FROM frequencies f
           JOIN frequency_participants p ON p.frequency_id = f.id
          WHERE p.pial = $1 AND p.state = 'joined' AND f.state = 'live'
          ORDER BY p.joined_at DESC LIMIT 1",
    )
    .bind(pial)
    .fetch_optional(conn)
    .await?)
}

pub async fn list_live(pool: &PgPool, limit: i64) -> Result<Vec<Frequency>> {
    let _t = Timed::db("list_live");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies WHERE state = 'live' ORDER BY started_at DESC LIMIT $1",
    )
    .bind(limit)
    .fetch_all(pool)
    .await?)
}

pub async fn list_scheduled(pool: &PgPool, limit: i64) -> Result<Vec<Frequency>> {
    let _t = Timed::db("list_scheduled");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies
          WHERE state = 'scheduled' AND scheduled_at >= NOW() - INTERVAL '1 hour'
          ORDER BY scheduled_at ASC LIMIT $1",
    )
    .bind(limit)
    .fetch_all(pool)
    .await?)
}

pub async fn list_ended(pool: &PgPool, limit: i64) -> Result<Vec<Frequency>> {
    let _t = Timed::db("list_ended");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies
          WHERE state IN ('ended', 'processing_replay', 'archived')
          ORDER BY ended_at DESC LIMIT $1",
    )
    .bind(limit)
    .fetch_all(pool)
    .await?)
}

/// Rooms a person is hosting now or has coming up.
pub async fn list_upcoming_by_host(pool: &PgPool, host_pial: &str, limit: i64) -> Result<Vec<Frequency>> {
    let _t = Timed::db("list_upcoming_by_host");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies
          WHERE host_pial = $1 AND (state = 'live'
                OR (state = 'scheduled' AND scheduled_at >= NOW() - INTERVAL '1 hour')
                OR (state = 'archived' AND replay_status = 'ready'))
          ORDER BY state = 'live' DESC, state = 'scheduled' DESC, scheduled_at ASC, ended_at DESC LIMIT $2",
    )
    .bind(host_pial)
    .bind(limit)
    .fetch_all(pool)
    .await?)
}

/// Rooms whose title or description holds the words, live first.
pub async fn search(pool: &PgPool, query: &str, limit: i64) -> Result<Vec<Frequency>> {
    let _t = Timed::db("search");
    let pattern = format!(
        "%{}%",
        query.replace('\\', "\\\\").replace('%', "\\%").replace('_', "\\_")
    );
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies
          WHERE (title ILIKE $1 OR description ILIKE $1)
            AND (state = 'live'
                 OR (state = 'scheduled' AND scheduled_at >= NOW() - INTERVAL '1 hour')
                 OR state IN ('ended', 'processing_replay', 'archived'))
          ORDER BY state = 'live' DESC, state = 'scheduled' DESC, COALESCE(ended_at, scheduled_at, created_at) DESC
          LIMIT $2",
    )
    .bind(pattern)
    .bind(limit)
    .fetch_all(pool)
    .await?)
}

pub async fn list_by_host(pool: &PgPool, host_pial: &str, limit: i64) -> Result<Vec<Frequency>> {
    let _t = Timed::db("list_by_host");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies WHERE host_pial = $1 ORDER BY created_at DESC LIMIT $2",
    )
    .bind(host_pial)
    .bind(limit)
    .fetch_all(pool)
    .await?)
}

/// Every Frequency with a session open or half-open. Read on boot for
/// reconciliation and by the presence sweeper.
pub async fn list_open(pool: &PgPool) -> Result<Vec<Frequency>> {
    let _t = Timed::db("list_open");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies WHERE state IN ('starting', 'live', 'ending') ORDER BY created_at",
    )
    .fetch_all(pool)
    .await?)
}

// ── events ───────────────────────────────────────────────────────────────────

pub async fn insert_event(conn: &mut PgConnection, ev: &Event) -> Result<()> {
    let _t = Timed::db("insert_event");
    sqlx::query(
        "INSERT INTO frequency_events
            (event_id, frequency_id, event_type, schema_version, pial_id, correlation_id, payload, created_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
    )
    .bind(ev.event_id)
    .bind(ev.frequency_id)
    .bind(&ev.event_type)
    .bind(&ev.schema_version)
    .bind(&ev.pial_id)
    .bind(&ev.correlation_id)
    .bind(&ev.payload)
    .bind(ev.timestamp)
    .execute(conn)
    .await?;
    Ok(())
}

pub async fn list_events(pool: &PgPool, id: Uuid, limit: i64) -> Result<Vec<Event>> {
    let _t = Timed::db("list_events");
    Ok(sqlx::query_as::<_, Event>(
        "SELECT event_id, event_type, schema_version, pial_id, created_at AS timestamp,
                frequency_id, correlation_id, payload
           FROM frequency_events WHERE frequency_id = $1 ORDER BY seq DESC LIMIT $2",
    )
    .bind(id)
    .bind(limit)
    .fetch_all(pool)
    .await?)
}

pub async fn unpublished_events(pool: &PgPool) -> Result<i64> {
    let (n,): (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM frequency_events WHERE published_at IS NULL")
            .fetch_one(pool)
            .await?;
    Ok(n)
}

// ── participants ─────────────────────────────────────────────────────────────

/// Open a participation row, or return the one already open. The partial
/// unique index makes a double join a no-op rather than a second row.
pub async fn join(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    role: FrequencyRole,
) -> Result<(Participant, bool)> {
    let _t = Timed::db("join");
    let inserted = sqlx::query_as::<_, Participant>(
        // Everyone who can speak enters with the mic closed. A mute a host set on
        // an earlier visit is still in force.
        "INSERT INTO frequency_participants (frequency_id, pial, role, muted, moderator_muted)
         VALUES ($1, $2, $3, $3 <> 'listener', COALESCE((
             SELECT moderator_muted FROM frequency_participants
              WHERE frequency_id = $1 AND pial = $2
              ORDER BY joined_at DESC, id DESC LIMIT 1), FALSE))
         ON CONFLICT (frequency_id, pial) WHERE state = 'joined' DO NOTHING
         RETURNING *",
    )
    .bind(id)
    .bind(pial)
    .bind(role.as_str())
    .fetch_optional(&mut *conn)
    .await
    .map_err(|e| match &e {
        // Two Tune Ins raced and this person is now joined to another room.
        sqlx::Error::Database(d) if d.constraint() == Some("uq_frequency_participants_one_room") => {
            AuralisError::Conflict("in_another_room")
        }
        _ => AuralisError::from(e),
    })?;
    if let Some(p) = inserted {
        return Ok((p, true));
    }
    let existing = joined_one(conn, id, pial).await?.ok_or_else(|| {
        AuralisError::Internal(anyhow::anyhow!("join conflict without a joined row"))
    })?;
    Ok((existing, false))
}

pub async fn joined_one(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
) -> Result<Option<Participant>> {
    let _t = Timed::db("joined_one");
    Ok(sqlx::query_as::<_, Participant>(
        "SELECT * FROM frequency_participants
          WHERE frequency_id = $1 AND pial = $2 AND state = 'joined'",
    )
    .bind(id)
    .bind(pial)
    .fetch_optional(conn)
    .await?)
}

pub async fn joined(conn: &mut PgConnection, id: Uuid) -> Result<Vec<Participant>> {
    let _t = Timed::db("joined");
    Ok(sqlx::query_as::<_, Participant>(
        "SELECT * FROM frequency_participants
          WHERE frequency_id = $1 AND state = 'joined' ORDER BY joined_at",
    )
    .bind(id)
    .fetch_all(conn)
    .await?)
}

pub async fn leave(conn: &mut PgConnection, id: Uuid, pial: &str) -> Result<Option<Participant>> {
    let _t = Timed::db("leave");
    let left = sqlx::query_as::<_, Participant>(
        "UPDATE frequency_participants
            SET state = 'left', left_at = NOW()
          WHERE frequency_id = $1 AND pial = $2 AND state = 'joined'
         RETURNING *",
    )
    .bind(id)
    .bind(pial)
    .fetch_optional(&mut *conn)
    .await?;
    expire_requests_of(conn, id, pial).await?;
    Ok(left)
}

/// A person who is no longer in the room is no longer asking for its mic.
pub async fn expire_requests_of(conn: &mut PgConnection, id: Uuid, pial: &str) -> Result<u64> {
    let _t = Timed::db("expire_requests_of");
    Ok(sqlx::query(
        "UPDATE frequency_speaker_requests SET status = 'expired', resolved_at = NOW()
          WHERE frequency_id = $1 AND pial = $2 AND status = 'pending'",
    )
    .bind(id)
    .bind(pial)
    .execute(conn)
    .await?
    .rows_affected())
}

/// The room this person is joined to other than `id`, if any.
pub async fn other_room(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
) -> Result<Option<Frequency>> {
    let _t = Timed::db("other_room");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT f.* FROM frequencies f
           JOIN frequency_participants p ON p.frequency_id = f.id
          WHERE p.pial = $1 AND p.state = 'joined' AND f.id <> $2",
    )
    .bind(pial)
    .bind(id)
    .fetch_optional(conn)
    .await?)
}

/// Record an invitation. True when a new one was created.
pub async fn invite(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    by: &str,
) -> Result<Option<Uuid>> {
    let _t = Timed::db("invite");
    let row: Option<(Uuid,)> = sqlx::query_as(
        "INSERT INTO frequency_invites (frequency_id, pial, role, invited_by)
         VALUES ($1, $2, 'listener', $3)
         ON CONFLICT (frequency_id, pial) WHERE accepted_at IS NULL AND declined_at IS NULL
         DO NOTHING
         RETURNING id",
    )
    .bind(id)
    .bind(pial)
    .bind(by)
    .fetch_optional(conn)
    .await?;
    Ok(row.map(|r| r.0))
}

pub async fn is_invited(conn: &mut PgConnection, id: Uuid, pial: &str) -> Result<bool> {
    let _t = Timed::db("is_invited");
    let r: Option<(i32,)> = sqlx::query_as(
        "SELECT 1 FROM frequency_invites
          WHERE frequency_id = $1 AND pial = $2 AND declined_at IS NULL LIMIT 1",
    )
    .bind(id)
    .bind(pial)
    .fetch_optional(conn)
    .await?;
    Ok(r.is_some())
}

pub async fn remove(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    by: &str,
    reason: &str,
) -> Result<Option<Participant>> {
    let _t = Timed::db("remove");
    let removed = sqlx::query_as::<_, Participant>(
        "UPDATE frequency_participants
            SET state = 'removed', removed_at = NOW(), removed_by = $3, remove_reason = $4
          WHERE frequency_id = $1 AND pial = $2 AND state = 'joined'
         RETURNING *",
    )
    .bind(id)
    .bind(pial)
    .bind(by)
    .bind(reason)
    .fetch_optional(&mut *conn)
    .await?;
    expire_requests_of(conn, id, pial).await?;
    Ok(removed)
}

pub async fn set_role(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    role: FrequencyRole,
) -> Result<Option<Participant>> {
    let _t = Timed::db("set_role");
    Ok(sqlx::query_as::<_, Participant>(
        // Coming up from the audience, the mic starts closed: being given the
        // ability to speak must never put someone on air before they choose to.
        "UPDATE frequency_participants
            SET muted = CASE WHEN role = 'listener' AND $3 <> 'listener' THEN TRUE ELSE muted END,
                role = $3
          WHERE frequency_id = $1 AND pial = $2 AND state = 'joined'
         RETURNING *",
    )
    .bind(id)
    .bind(pial)
    .bind(role.as_str())
    .fetch_optional(conn)
    .await?)
}

pub async fn set_muted(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    muted: bool,
) -> Result<Option<Participant>> {
    let _t = Timed::db("set_muted");
    Ok(sqlx::query_as::<_, Participant>(
        "UPDATE frequency_participants SET muted = $3
          WHERE frequency_id = $1 AND pial = $2 AND state = 'joined'
         RETURNING *",
    )
    .bind(id)
    .bind(pial)
    .bind(muted)
    .fetch_optional(conn)
    .await?)
}

/// A host or co-host mutes or unmutes someone. Lifting it leaves the mic closed:
/// only the person themself opens their own microphone.
pub async fn set_moderator_muted(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    muted: bool,
) -> Result<Option<Participant>> {
    let _t = Timed::db("set_moderator_muted");
    Ok(sqlx::query_as::<_, Participant>(
        "UPDATE frequency_participants SET moderator_muted = $3, muted = TRUE
          WHERE frequency_id = $1 AND pial = $2 AND state = 'joined'
         RETURNING *",
    )
    .bind(id)
    .bind(pial)
    .bind(muted)
    .fetch_optional(conn)
    .await?)
}

/// Everyone still joined is marked left. Called when a Frequency ends.
pub async fn leave_all(conn: &mut PgConnection, id: Uuid) -> Result<u64> {
    let _t = Timed::db("leave_all");
    Ok(sqlx::query(
        "UPDATE frequency_participants SET state = 'left', left_at = NOW()
          WHERE frequency_id = $1 AND state = 'joined'",
    )
    .bind(id)
    .execute(conn)
    .await?
    .rows_affected())
}

// ── roles ────────────────────────────────────────────────────────────────────

pub async fn granted_role(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
) -> Result<Option<FrequencyRole>> {
    let _t = Timed::db("granted_role");
    let r: Option<(String,)> = sqlx::query_as(
        "SELECT role FROM frequency_roles
          WHERE frequency_id = $1 AND pial = $2 AND revoked_at IS NULL",
    )
    .bind(id)
    .bind(pial)
    .fetch_optional(conn)
    .await?;
    Ok(r.and_then(|(s,)| FrequencyRole::parse(&s)))
}

pub async fn grant_role(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    role: FrequencyRole,
    by: &str,
) -> Result<()> {
    let _t = Timed::db("grant_role");
    sqlx::query(
        "UPDATE frequency_roles SET revoked_at = NOW(), revoked_by = $3
          WHERE frequency_id = $1 AND pial = $2 AND revoked_at IS NULL",
    )
    .bind(id)
    .bind(pial)
    .bind(by)
    .execute(&mut *conn)
    .await?;
    sqlx::query(
        "INSERT INTO frequency_roles (frequency_id, pial, role, granted_by) VALUES ($1, $2, $3, $4)",
    )
    .bind(id)
    .bind(pial)
    .bind(role.as_str())
    .bind(by)
    .execute(conn)
    .await?;
    Ok(())
}

pub async fn revoke_role(conn: &mut PgConnection, id: Uuid, pial: &str, by: &str) -> Result<bool> {
    let _t = Timed::db("revoke_role");
    Ok(sqlx::query(
        "UPDATE frequency_roles SET revoked_at = NOW(), revoked_by = $3
          WHERE frequency_id = $1 AND pial = $2 AND revoked_at IS NULL",
    )
    .bind(id)
    .bind(pial)
    .bind(by)
    .execute(conn)
    .await?
    .rows_affected()
        > 0)
}

pub async fn cohosts(conn: &mut PgConnection, id: Uuid) -> Result<Vec<String>> {
    let _t = Timed::db("cohosts");
    let rows: Vec<(String,)> = sqlx::query_as(
        "SELECT pial FROM frequency_roles
          WHERE frequency_id = $1 AND role = 'cohost' AND revoked_at IS NULL ORDER BY granted_at",
    )
    .bind(id)
    .fetch_all(conn)
    .await?;
    Ok(rows.into_iter().map(|(p,)| p).collect())
}

// ── speaker requests ─────────────────────────────────────────────────────────

pub async fn create_request(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    reason: &str,
) -> Result<Option<SpeakerRequest>> {
    let _t = Timed::db("create_request");
    Ok(sqlx::query_as::<_, SpeakerRequest>(
        "INSERT INTO frequency_speaker_requests (frequency_id, pial, reason)
         VALUES ($1, $2, $3)
         ON CONFLICT (frequency_id, pial) WHERE status = 'pending' DO NOTHING
         RETURNING *",
    )
    .bind(id)
    .bind(pial)
    .bind(reason)
    .fetch_optional(conn)
    .await?)
}

pub async fn get_request(conn: &mut PgConnection, req_id: Uuid) -> Result<Option<SpeakerRequest>> {
    let _t = Timed::db("get_request");
    Ok(sqlx::query_as::<_, SpeakerRequest>(
        "SELECT * FROM frequency_speaker_requests WHERE id = $1",
    )
    .bind(req_id)
    .fetch_optional(conn)
    .await?)
}

pub async fn pending_request_for(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
) -> Result<Option<SpeakerRequest>> {
    let _t = Timed::db("pending_request_for");
    Ok(sqlx::query_as::<_, SpeakerRequest>(
        "SELECT * FROM frequency_speaker_requests
          WHERE frequency_id = $1 AND pial = $2 AND status = 'pending'",
    )
    .bind(id)
    .bind(pial)
    .fetch_optional(conn)
    .await?)
}

/// The queue, best reasons first: upvotes, then age. The host reads this.
pub async fn pending_requests(conn: &mut PgConnection, id: Uuid) -> Result<Vec<SpeakerRequest>> {
    let _t = Timed::db("pending_requests");
    Ok(sqlx::query_as::<_, SpeakerRequest>(
        "SELECT * FROM frequency_speaker_requests
          WHERE frequency_id = $1 AND status = 'pending'
          ORDER BY upvotes DESC, created_at ASC",
    )
    .bind(id)
    .fetch_all(conn)
    .await?)
}

/// Resolve a pending request. `None` means it was no longer pending — the
/// other moderator won.
pub async fn resolve_request(
    conn: &mut PgConnection,
    req_id: Uuid,
    to: RequestStatus,
    by: Option<&str>,
) -> Result<Option<SpeakerRequest>> {
    let _t = Timed::db("resolve_request");
    Ok(sqlx::query_as::<_, SpeakerRequest>(
        "UPDATE frequency_speaker_requests
            SET status = $2, resolved_at = NOW(), resolved_by = $3
          WHERE id = $1 AND status = 'pending'
         RETURNING *",
    )
    .bind(req_id)
    .bind(to.as_str())
    .bind(by)
    .fetch_optional(conn)
    .await?)
}

pub async fn expire_requests(conn: &mut PgConnection, id: Uuid) -> Result<u64> {
    let _t = Timed::db("expire_requests");
    Ok(sqlx::query(
        "UPDATE frequency_speaker_requests SET status = 'expired', resolved_at = NOW()
          WHERE frequency_id = $1 AND status = 'pending'",
    )
    .bind(id)
    .execute(conn)
    .await?
    .rows_affected())
}

/// One upvote per person per request. True if it counted.
pub async fn upvote_request(conn: &mut PgConnection, req_id: Uuid, pial: &str) -> Result<bool> {
    let _t = Timed::db("upvote_request");
    let n = sqlx::query(
        "INSERT INTO frequency_request_upvotes (request_id, pial) VALUES ($1, $2)
         ON CONFLICT DO NOTHING",
    )
    .bind(req_id)
    .bind(pial)
    .execute(&mut *conn)
    .await?
    .rows_affected();
    if n == 0 {
        return Ok(false);
    }
    sqlx::query("UPDATE frequency_speaker_requests SET upvotes = upvotes + 1 WHERE id = $1 AND status = 'pending'")
        .bind(req_id)
        .execute(conn)
        .await?;
    Ok(true)
}

// ── blocks and moderation log ────────────────────────────────────────────────

pub async fn blocked(conn: &mut PgConnection, id: Uuid) -> Result<Vec<String>> {
    let _t = Timed::db("blocked");
    let rows: Vec<(String,)> =
        sqlx::query_as("SELECT pial FROM frequency_blocks WHERE frequency_id = $1 ORDER BY pial")
            .bind(id)
            .fetch_all(conn)
            .await?;
    Ok(rows.into_iter().map(|(p,)| p).collect())
}

// ── pins ─────────────────────────────────────────────────────────────────────

/// A room shows a short list, not a reading list.
pub const MAX_PINS: i64 = 10;

pub async fn pins(conn: &mut PgConnection, id: Uuid) -> Result<Vec<(Uuid, String)>> {
    let _t = Timed::db("pins");
    Ok(sqlx::query_as(
        "SELECT work_id, pinned_by FROM frequency_pins WHERE frequency_id = $1 ORDER BY created_at",
    )
    .bind(id)
    .fetch_all(conn)
    .await?)
}

/// False when the room already has its limit. Pinning a work twice changes nothing.
pub async fn add_pin(conn: &mut PgConnection, id: Uuid, work: Uuid, by: &str) -> Result<bool> {
    let _t = Timed::db("add_pin");
    let (held,): (i64,) = sqlx::query_as("SELECT COUNT(*) FROM frequency_pins WHERE frequency_id = $1")
        .bind(id)
        .fetch_one(&mut *conn)
        .await?;
    if held >= MAX_PINS {
        return Ok(false);
    }
    sqlx::query(
        "INSERT INTO frequency_pins (frequency_id, work_id, pinned_by) VALUES ($1, $2, $3)
         ON CONFLICT DO NOTHING",
    )
    .bind(id)
    .bind(work)
    .bind(by)
    .execute(conn)
    .await?;
    Ok(true)
}

/// Who pinned this work, if it is pinned.
pub async fn pinned_by(conn: &mut PgConnection, id: Uuid, work: Uuid) -> Result<Option<String>> {
    let _t = Timed::db("pinned_by");
    let row: Option<(String,)> =
        sqlx::query_as("SELECT pinned_by FROM frequency_pins WHERE frequency_id = $1 AND work_id = $2")
            .bind(id)
            .bind(work)
            .fetch_optional(conn)
            .await?;
    Ok(row.map(|(p,)| p))
}

pub async fn remove_pin(conn: &mut PgConnection, id: Uuid, work: Uuid) -> Result<()> {
    let _t = Timed::db("remove_pin");
    sqlx::query("DELETE FROM frequency_pins WHERE frequency_id = $1 AND work_id = $2")
        .bind(id)
        .bind(work)
        .execute(conn)
        .await?;
    Ok(())
}

// ── recordings ───────────────────────────────────────────────────────────────

/// The recording in progress for a room: id, egress id, file.
pub async fn active_recording(pool: &PgPool, id: Uuid) -> Result<Option<(Uuid, String, String)>> {
    let _t = Timed::db("active_recording");
    Ok(sqlx::query_as(
        "SELECT id, egress_id, object_key FROM frequency_recordings
          WHERE frequency_id = $1 AND processing_state = 'recording'",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?)
}

pub async fn failed_recordings(pool: &PgPool, id: Uuid) -> Result<i64> {
    let _t = Timed::db("failed_recordings");
    let (n,): (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM frequency_recordings WHERE frequency_id = $1 AND processing_state = 'failed'",
    )
    .bind(id)
    .fetch_one(pool)
    .await?;
    Ok(n)
}

/// False when the room already has a recording in progress.
pub async fn insert_recording(pool: &PgPool, id: Uuid, room: Uuid, egress: &str, file: &str) -> Result<bool> {
    let _t = Timed::db("insert_recording");
    Ok(sqlx::query(
        "INSERT INTO frequency_recordings (id, frequency_id, egress_id, object_key, codec)
         VALUES ($1, $2, $3, $4, 'aac')
         ON CONFLICT (frequency_id) WHERE processing_state = 'recording' DO NOTHING",
    )
    .bind(id)
    .bind(room)
    .bind(egress)
    .bind(file)
    .execute(pool)
    .await?
    .rows_affected()
        == 1)
}

/// Every recording in progress: recording id, room, egress id, file.
pub async fn recordings_in_progress(pool: &PgPool) -> Result<Vec<(Uuid, Uuid, String, String)>> {
    let _t = Timed::db("recordings_in_progress");
    Ok(sqlx::query_as(
        "SELECT id, frequency_id, egress_id, object_key FROM frequency_recordings
          WHERE processing_state = 'recording'",
    )
    .fetch_all(pool)
    .await?)
}

pub async fn finish_recording(
    pool: &PgPool,
    id: Uuid,
    state: &str,
    duration_ms: Option<i64>,
    failure: Option<&str>,
) -> Result<()> {
    let _t = Timed::db("finish_recording");
    sqlx::query(
        "UPDATE frequency_recordings
            SET processing_state = $2, duration_ms = $3, failure_reason = $4,
                ended_at = NOW(), updated_at = NOW()
          WHERE id = $1",
    )
    .bind(id)
    .bind(state)
    .bind(duration_ms)
    .bind(failure)
    .execute(pool)
    .await?;
    Ok(())
}

/// Finished rooms that opted into recording and whose replay is not settled. A room
/// removed by moderation is not among them: it never gets a public replay.
pub async fn rooms_awaiting_replay(pool: &PgPool) -> Result<Vec<Frequency>> {
    let _t = Timed::db("rooms_awaiting_replay");
    Ok(sqlx::query_as::<_, Frequency>(
        "SELECT * FROM frequencies
          WHERE recording_enabled AND state IN ('ended', 'processing_replay')",
    )
    .fetch_all(pool)
    .await?)
}

/// The file of a room's newest finished recording.
pub async fn latest_ready_recording(pool: &PgPool, id: Uuid) -> Result<Option<String>> {
    let _t = Timed::db("latest_ready_recording");
    let row: Option<(String,)> = sqlx::query_as(
        "SELECT object_key FROM frequency_recordings
          WHERE frequency_id = $1 AND processing_state = 'ready'
          ORDER BY started_at DESC LIMIT 1",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|(f,)| f))
}

/// Marks a room's finished recordings deleted and answers their files.
pub async fn delete_recordings(conn: &mut PgConnection, id: Uuid) -> Result<Vec<String>> {
    let _t = Timed::db("delete_recordings");
    let rows: Vec<(String,)> = sqlx::query_as(
        "UPDATE frequency_recordings SET processing_state = 'deleted', updated_at = NOW()
          WHERE frequency_id = $1 AND processing_state = 'ready'
         RETURNING object_key",
    )
    .bind(id)
    .fetch_all(conn)
    .await?;
    Ok(rows.into_iter().map(|(f,)| f).collect())
}

// ── reminders ────────────────────────────────────────────────────────────────

pub async fn set_reminder(conn: &mut PgConnection, id: Uuid, pial: &str, on: bool) -> Result<()> {
    let _t = Timed::db("set_reminder");
    let query = if on {
        "INSERT INTO frequency_reminders (frequency_id, pial) VALUES ($1, $2) ON CONFLICT DO NOTHING"
    } else {
        "DELETE FROM frequency_reminders WHERE frequency_id = $1 AND pial = $2"
    };
    sqlx::query(query).bind(id).bind(pial).execute(conn).await?;
    Ok(())
}

pub async fn reminders(conn: &mut PgConnection, id: Uuid) -> Result<Vec<String>> {
    let _t = Timed::db("reminders");
    let rows: Vec<(String,)> =
        sqlx::query_as("SELECT pial FROM frequency_reminders WHERE frequency_id = $1")
            .bind(id)
            .fetch_all(conn)
            .await?;
    Ok(rows.into_iter().map(|(p,)| p).collect())
}

pub async fn reminder_count(conn: &mut PgConnection, id: Uuid) -> Result<i64> {
    let _t = Timed::db("reminder_count");
    let (n,): (i64,) = sqlx::query_as("SELECT COUNT(*) FROM frequency_reminders WHERE frequency_id = $1")
        .bind(id)
        .fetch_one(conn)
        .await?;
    Ok(n)
}

pub async fn has_reminder(conn: &mut PgConnection, id: Uuid, pial: &str) -> Result<bool> {
    let _t = Timed::db("has_reminder");
    let r: Option<(i32,)> =
        sqlx::query_as("SELECT 1 FROM frequency_reminders WHERE frequency_id = $1 AND pial = $2")
            .bind(id)
            .bind(pial)
            .fetch_optional(conn)
            .await?;
    Ok(r.is_some())
}

/// A new time owes everyone a new "starts soon" and the host a new nudge.
pub async fn clear_schedule_notices(conn: &mut PgConnection, id: Uuid) -> Result<()> {
    let _t = Timed::db("clear_schedule_notices");
    sqlx::query("UPDATE frequencies SET soon_notified_at = NULL, start_due_notified_at = NULL WHERE id = $1")
        .bind(id)
        .execute(conn)
        .await?;
    Ok(())
}

/// Scheduled rooms starting within the window that have not had their notice. The
/// marks are set here, in the caller's transaction, with whatever the notice writes.
pub async fn take_soon(conn: &mut PgConnection, window_secs: i64) -> Result<Vec<Uuid>> {
    let _t = Timed::db("take_soon");
    let rows: Vec<(Uuid,)> = sqlx::query_as(
        "UPDATE frequencies SET soon_notified_at = NOW()
          WHERE state = 'scheduled' AND soon_notified_at IS NULL
            AND scheduled_at <= NOW() + make_interval(secs => $1::double precision)
         RETURNING id",
    )
    .bind(window_secs)
    .fetch_all(conn)
    .await?;
    Ok(rows.into_iter().map(|(id,)| id).collect())
}

/// Scheduled rooms whose time has come and whose host has not been nudged.
pub async fn take_start_due(conn: &mut PgConnection) -> Result<Vec<(Uuid, String, String)>> {
    let _t = Timed::db("take_start_due");
    Ok(sqlx::query_as(
        "UPDATE frequencies SET start_due_notified_at = NOW()
          WHERE state = 'scheduled' AND start_due_notified_at IS NULL AND scheduled_at <= NOW()
         RETURNING id, host_pial, title",
    )
    .fetch_all(conn)
    .await?)
}

/// Scheduled rooms the host never started, long past their time.
pub async fn list_missed(pool: &PgPool, grace_secs: i64) -> Result<Vec<Uuid>> {
    let _t = Timed::db("list_missed");
    let rows: Vec<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM frequencies
          WHERE state = 'scheduled' AND scheduled_at < NOW() - make_interval(secs => $1::double precision)",
    )
    .bind(grace_secs)
    .fetch_all(pool)
    .await?;
    Ok(rows.into_iter().map(|(id,)| id).collect())
}

pub async fn is_blocked(conn: &mut PgConnection, id: Uuid, pial: &str) -> Result<bool> {
    let _t = Timed::db("is_blocked");
    let r: Option<(i32,)> =
        sqlx::query_as("SELECT 1 FROM frequency_blocks WHERE frequency_id = $1 AND pial = $2")
            .bind(id)
            .bind(pial)
            .fetch_optional(conn)
            .await?;
    Ok(r.is_some())
}

pub async fn block(
    conn: &mut PgConnection,
    id: Uuid,
    pial: &str,
    by: &str,
    reason: &str,
) -> Result<bool> {
    let _t = Timed::db("block");
    Ok(sqlx::query(
        "INSERT INTO frequency_blocks (frequency_id, pial, blocked_by, reason) VALUES ($1, $2, $3, $4)
         ON CONFLICT DO NOTHING",
    )
    .bind(id)
    .bind(pial)
    .bind(by)
    .bind(reason)
    .execute(conn)
    .await?
    .rows_affected()
        > 0)
}

pub async fn unblock(conn: &mut PgConnection, id: Uuid, pial: &str) -> Result<bool> {
    let _t = Timed::db("unblock");
    Ok(
        sqlx::query("DELETE FROM frequency_blocks WHERE frequency_id = $1 AND pial = $2")
            .bind(id)
            .bind(pial)
            .execute(conn)
            .await?
            .rows_affected()
            > 0,
    )
}

pub async fn log_moderation(
    conn: &mut PgConnection,
    id: Uuid,
    actor: &str,
    target: Option<&str>,
    action: &str,
    reason: &str,
) -> Result<()> {
    let _t = Timed::db("log_moderation");
    sqlx::query(
        "INSERT INTO frequency_moderation_actions (frequency_id, actor, target, action, reason)
         VALUES ($1, $2, $3, $4, $5)",
    )
    .bind(id)
    .bind(actor)
    .bind(target)
    .bind(action)
    .bind(reason)
    .execute(conn)
    .await?;
    Ok(())
}

// ── idempotency ──────────────────────────────────────────────────────────────

pub async fn idempotent_get(
    conn: &mut PgConnection,
    actor: &str,
    key: &str,
) -> Result<Option<(i16, Value)>> {
    let _t = Timed::db("idempotent_get");
    Ok(sqlx::query_as::<_, (i16, Value)>(
        "SELECT status, response FROM frequency_idempotency WHERE actor = $1 AND idempotency_key = $2",
    )
    .bind(actor)
    .bind(key)
    .fetch_optional(conn)
    .await?)
}

pub async fn idempotent_put(
    conn: &mut PgConnection,
    actor: &str,
    key: &str,
    status: i16,
    response: &Value,
) -> Result<()> {
    let _t = Timed::db("idempotent_put");
    sqlx::query(
        "INSERT INTO frequency_idempotency (actor, idempotency_key, status, response)
         VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING",
    )
    .bind(actor)
    .bind(key)
    .bind(status)
    .bind(response)
    .execute(conn)
    .await?;
    Ok(())
}

pub async fn idempotent_prune(pool: &PgPool, older_than_hours: i64) -> Result<u64> {
    let _t = Timed::db("idempotent_prune");
    Ok(sqlx::query(
        "DELETE FROM frequency_idempotency WHERE created_at < NOW() - ($1 * INTERVAL '1 hour')",
    )
    .bind(older_than_hours)
    .execute(pool)
    .await?
    .rows_affected())
}
