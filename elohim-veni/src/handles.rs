//! The handle registry, merged in from registry-brain.
//!
//! A handle is a pointer; a PIAL is identity. Transferring a handle never
//! transfers followers, reputation, wallet or messages. elohim-veni governs the
//! `handle` namespace in Manhattan (manhattan 0010); every lifecycle transition
//! queues its name operation through trg_manhattan_sync_handle (0002), in the
//! same transaction as the change.
//!
//! What changed in the move:
//!   * every mutation, its handle_events row and its identity.events row commit
//!     together (registry-brain logged events after commit and swallowed
//!     failures);
//!   * a handle an account presents cannot be unbound, transferred or put up
//!     for auction (409 handle_in_use) — the account would be left holding a
//!     handle that points at someone else. Moderation (freeze, quarantine,
//!     reclaim, unfreeze) still applies to it;
//!   * bids are conditional updates, so two concurrent bids cannot both win,
//!     and a bid after the auction's end is refused;
//!   * one open auction per handle.

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sqlx::PgConnection;
use uuid::Uuid;

use crate::api::{ApiError, ApiResult};
use crate::handlers::AppState;
use crate::identity::{accounts::claimable_handle, events, vocab};

// ── Shared with account creation ──────────────────────────────────────────────

/// A unique violation becomes the conflict it means; anything else is internal.
pub fn map_unique(e: sqlx::Error, code: &'static str) -> ApiError {
    if let sqlx::Error::Database(db) = &e {
        if db.code().as_deref() == Some("23505") {
            return ApiError::conflict(code, "That handle is already taken. Try another.");
        }
    }
    ApiError::from(e)
}

/// Refuses a handle that is reserved or already registered.
pub async fn ensure_claimable(conn: &mut PgConnection, handle: &str) -> ApiResult<()> {
    let (reserved, taken): (bool, bool) = sqlx::query_as(
        "SELECT EXISTS (SELECT 1 FROM reserved_handles WHERE handle = $1::citext),
                EXISTS (SELECT 1 FROM handles WHERE handle = $1::citext)",
    )
    .bind(handle)
    .fetch_one(conn)
    .await?;
    if reserved {
        return Err(ApiError::conflict("handle_reserved", "That handle is reserved. Try another."));
    }
    if taken {
        return Err(ApiError::conflict("handle_taken", "That handle is already taken. Try another."));
    }
    Ok(())
}

/// Registers a handle to a PIAL, with its lifecycle event and identity event.
pub async fn insert_claim(
    conn: &mut PgConnection,
    handle: &str,
    pial_id: Uuid,
    tier: &str,
    metadata: Value,
) -> ApiResult<()> {
    sqlx::query("INSERT INTO handles (handle, pial_id, tier) VALUES ($1::citext, $2, $3)")
        .bind(handle)
        .bind(pial_id)
        .bind(tier)
        .execute(&mut *conn)
        .await
        .map_err(|e| map_unique(e, "handle_taken"))?;
    let mut meta = metadata;
    meta["tier"] = json!(tier);
    log_event(conn, handle, "register", None, Some(pial_id), meta.clone()).await?;
    events::emit_handle(conn, handle, "handle.registered", meta).await?;
    Ok(())
}

/// Appends to handle_events. An error fails the caller's transaction.
pub async fn log_event(
    conn: &mut PgConnection,
    handle: &str,
    event_type: &str,
    from_pial: Option<Uuid>,
    to_pial: Option<Uuid>,
    metadata: Value,
) -> anyhow::Result<()> {
    sqlx::query(
        "INSERT INTO handle_events (handle_id, event_type, from_pial_id, to_pial_id, metadata)
         SELECT id, $2, $3, $4, $5 FROM handles WHERE handle = $1::citext",
    )
    .bind(handle)
    .bind(event_type)
    .bind(from_pial)
    .bind(to_pial)
    .bind(metadata)
    .execute(conn)
    .await
    .map_err(|e| anyhow::anyhow!("handle_events: recording {event_type} for {handle}: {e}"))
    .and_then(|r| {
        if r.rows_affected() == 1 {
            Ok(())
        } else {
            Err(anyhow::anyhow!("handle_events: no handle {handle} to record {event_type} against"))
        }
    })
}

async fn refuse_if_account_holds(conn: &mut PgConnection, handle: &str) -> ApiResult<()> {
    let held: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM accounts WHERE handle = $1::citext)")
        .bind(handle)
        .fetch_one(conn)
        .await?;
    if held {
        return Err(ApiError::conflict(
            "handle_in_use",
            "An account presents this handle; it cannot be unbound, transferred or auctioned.",
        ));
    }
    Ok(())
}

async fn require_live_pial(conn: &mut PgConnection, pial_id: Uuid) -> ApiResult<()> {
    let status: Option<String> = sqlx::query_scalar("SELECT status FROM pial_states WHERE pial_id = $1")
        .bind(pial_id)
        .fetch_optional(conn)
        .await?;
    match status {
        None => Err(ApiError::not_found("not_found", "No such PIAL.")),
        Some(s) if vocab::is_gone(&s) => Err(ApiError::conflict("pial_tombstoned", "This identity no longer exists.")),
        Some(_) => Ok(()),
    }
}

// ── Rows ──────────────────────────────────────────────────────────────────────

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct HandleRow {
    pub id: Uuid,
    pub handle: String,
    pub pial_id: Uuid,
    pub status: String,
    pub tier: String,
    pub created_at: DateTime<Utc>,
    pub last_bound_at: DateTime<Utc>,
}

const HANDLE_COLS: &str = "id, handle::text AS handle, pial_id, status, tier, created_at, last_bound_at";

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct AuctionRow {
    pub id: Uuid,
    pub handle_id: Uuid,
    pub start_time: DateTime<Utc>,
    pub end_time: DateTime<Utc>,
    pub starting_price_aet: i32,
    pub current_price_aet: i32,
    pub highest_bidder_pial: Option<Uuid>,
    pub status: String,
}

const AUCTION_COLS: &str =
    "id, handle_id, start_time, end_time, starting_price_aet, current_price_aet, highest_bidder_pial, status";

fn auction_json(a: &AuctionRow, handle: &str, bid_count: i64) -> Value {
    json!({
        "id": a.id,
        "handle": handle,
        "start_time": a.start_time,
        "end_time": a.end_time,
        "starting_price_aet": a.starting_price_aet,
        "current_price_aet": a.current_price_aet,
        "highest_bidder_pial": a.highest_bidder_pial,
        "status": a.status,
        "bid_count": bid_count,
    })
}

fn norm(handle: &str) -> String {
    vocab::normalize_handle(handle)
}

// ── POST /v1/handles ──────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct ClaimReq {
    pub handle: String,
    pub pial_id: Uuid,
    #[serde(default = "default_tier")]
    pub tier: String,
}
fn default_tier() -> String {
    "standard".into()
}

pub async fn claim(State(state): State<AppState>, Json(req): Json<ClaimReq>) -> ApiResult<impl IntoResponse> {
    let handle = claimable_handle(&req.handle)?;
    if !matches!(req.tier.as_str(), "standard" | "premium" | "elite") {
        return Err(ApiError::bad_request("tier_invalid", "tier must be standard, premium or elite."));
    }
    let mut tx = state.pool.begin().await?;
    require_live_pial(&mut tx, req.pial_id).await?;
    ensure_claimable(&mut tx, &handle).await?;
    insert_claim(&mut tx, &handle, req.pial_id, &req.tier, json!({ "source": "claim" })).await?;
    let row = fetch(&mut tx, &handle).await?;
    tx.commit().await?;
    Ok((StatusCode::CREATED, Json(row)))
}

async fn fetch(conn: &mut PgConnection, handle: &str) -> ApiResult<HandleRow> {
    sqlx::query_as::<_, HandleRow>(&format!("SELECT {HANDLE_COLS} FROM handles WHERE handle = $1::citext"))
        .bind(handle)
        .fetch_optional(conn)
        .await?
        .ok_or_else(|| ApiError::not_found("not_found", "No such handle."))
}

// ── GET /v1/handles/:handle ───────────────────────────────────────────────────

pub async fn resolve(State(state): State<AppState>, Path(handle): Path<String>) -> ApiResult<Json<Value>> {
    let handle = norm(&handle);
    let mut conn = state.pool.acquire().await?;
    let row = fetch(&mut conn, &handle).await?;
    let account_id: Option<Uuid> = sqlx::query_scalar("SELECT id FROM accounts WHERE handle = $1::citext")
        .bind(&handle)
        .fetch_optional(&mut *conn)
        .await?;
    Ok(Json(json!({
        "handle": row.handle,
        "pial_id": row.pial_id,
        "status": row.status,
        "tier": row.tier,
        "cached": false,
        "account_id": account_id,
    })))
}

// ── Transitions ───────────────────────────────────────────────────────────────

/// Runs one UPDATE on a handle and records it, or answers 404 with `missing`.
async fn transition(
    state: &AppState,
    handle: &str,
    update_sql: &str,
    binds: (Option<Uuid>, Option<Uuid>, Option<String>),
    missing: &'static str,
    event: (&str, Option<Uuid>, Option<Uuid>, Value),
    guard_account: bool,
) -> ApiResult<Json<HandleRow>> {
    let mut tx = state.pool.begin().await?;
    if guard_account {
        refuse_if_account_holds(&mut tx, handle).await?;
    }
    let mut q = sqlx::query(update_sql).bind(handle);
    if let Some(a) = binds.0 {
        q = q.bind(a);
    }
    if let Some(b) = binds.1 {
        q = q.bind(b);
    }
    if let Some(s) = binds.2 {
        q = q.bind(s);
    }
    let n = q.execute(&mut *tx).await?.rows_affected();
    if n == 0 {
        return Err(ApiError::not_found("not_found", missing));
    }
    let (event_type, from, to, meta) = event;
    log_event(&mut tx, handle, event_type, from, to, meta.clone()).await?;
    events::emit_handle(&mut tx, handle, &format!("handle.{event_type}"), meta).await?;
    let row = fetch(&mut tx, handle).await?;
    tx.commit().await?;
    Ok(Json(row))
}

#[derive(Debug, Deserialize)]
pub struct BindReq {
    pub pial_id: Uuid,
    pub requester: Uuid,
}

/// Binds an inactive or quarantined handle to a PIAL.
pub async fn bind(State(state): State<AppState>, Path(handle): Path<String>, Json(req): Json<BindReq>) -> ApiResult<Json<HandleRow>> {
    let handle = norm(&handle);
    {
        let mut conn = state.pool.acquire().await?;
        require_live_pial(&mut conn, req.pial_id).await?;
    }
    transition(
        &state,
        &handle,
        "UPDATE handles SET pial_id = $2, status = 'active', last_bound_at = NOW(), updated_at = NOW()
          WHERE handle = $1::citext AND status IN ('inactive','quarantined')",
        (Some(req.pial_id), None, None),
        "Handle not found or not bindable.",
        ("bind", None, Some(req.pial_id), json!({ "requester": req.requester })),
        true,
    )
    .await
}

#[derive(Debug, Deserialize)]
pub struct UnbindReq {
    pub requester: Uuid,
}

pub async fn unbind(State(state): State<AppState>, Path(handle): Path<String>, Json(req): Json<UnbindReq>) -> ApiResult<Json<HandleRow>> {
    let handle = norm(&handle);
    transition(
        &state,
        &handle,
        "UPDATE handles SET status = 'inactive', updated_at = NOW()
          WHERE handle = $1::citext AND pial_id = $2 AND status = 'active'",
        (Some(req.requester), None, None),
        "Handle not found or not owned by requester.",
        ("unbind", Some(req.requester), None, json!({})),
        true,
    )
    .await
}

#[derive(Debug, Deserialize)]
pub struct TransferReq {
    pub from_pial: Uuid,
    pub to_pial: Uuid,
}

pub async fn transfer(State(state): State<AppState>, Path(handle): Path<String>, Json(req): Json<TransferReq>) -> ApiResult<Json<HandleRow>> {
    let handle = norm(&handle);
    {
        let mut conn = state.pool.acquire().await?;
        require_live_pial(&mut conn, req.to_pial).await?;
    }
    transition(
        &state,
        &handle,
        "UPDATE handles SET pial_id = $2, last_bound_at = NOW(), updated_at = NOW()
          WHERE handle = $1::citext AND pial_id = $3 AND status = 'active'",
        (Some(req.to_pial), Some(req.from_pial), None),
        "Handle not found or not owned by from_pial.",
        (
            "transfer",
            Some(req.from_pial),
            Some(req.to_pial),
            json!({ "note": "followers/reputation/wallet remain with from_pial" }),
        ),
        true,
    )
    .await
}

#[derive(Debug, Deserialize)]
pub struct AdminReq {
    pub action: String,
    pub reason: String,
    pub admin_id: String,
}

/// Moderation: freeze | unfreeze | quarantine | reclaim. Applies to a handle an
/// account presents as well — that is the point of moderation.
pub async fn admin(State(state): State<AppState>, Path(handle): Path<String>, Json(req): Json<AdminReq>) -> ApiResult<Json<HandleRow>> {
    let handle = norm(&handle);
    let new_status = match req.action.as_str() {
        "freeze" => "frozen",
        "unfreeze" => "active",
        "quarantine" => "quarantined",
        "reclaim" => "reserved",
        _ => {
            return Err(ApiError::bad_request(
                "action_invalid",
                "action must be freeze, unfreeze, quarantine or reclaim.",
            ))
        }
    };
    let action = req.action.clone();
    transition(
        &state,
        &handle,
        "UPDATE handles SET status = $2, updated_at = NOW() WHERE handle = $1::citext",
        (None, None, Some(new_status.to_string())),
        "Handle not found.",
        (&action, None, None, json!({ "admin": req.admin_id, "reason": req.reason })),
        false,
    )
    .await
}

// ── Auctions ──────────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct AuctionReq {
    pub duration_hours: u32,
    pub starting_price_aet: i32,
    pub requester: Uuid,
}

pub async fn create_auction(
    State(state): State<AppState>,
    Path(handle): Path<String>,
    Json(req): Json<AuctionReq>,
) -> ApiResult<impl IntoResponse> {
    let handle = norm(&handle);
    if req.starting_price_aet <= 0 {
        return Err(ApiError::bad_request("price_invalid", "starting_price_aet must be positive."));
    }
    let hours = req.duration_hours.clamp(1, 168) as i32;
    let mut tx = state.pool.begin().await?;
    refuse_if_account_holds(&mut tx, &handle).await?;
    let handle_id: Uuid = sqlx::query_scalar(
        "UPDATE handles SET status = 'auction', updated_at = NOW()
          WHERE handle = $1::citext AND pial_id = $2 AND status = 'active'
         RETURNING id",
    )
    .bind(&handle)
    .bind(req.requester)
    .fetch_optional(&mut *tx)
    .await?
    .ok_or_else(|| ApiError::not_found("not_found", "Handle not found or not owned by requester."))?;
    let auction = sqlx::query_as::<_, AuctionRow>(&format!(
        "INSERT INTO handle_auctions (handle_id, end_time, starting_price_aet, current_price_aet)
         VALUES ($1, NOW() + $2 * INTERVAL '1 hour', $3, $3) RETURNING {AUCTION_COLS}"
    ))
    .bind(handle_id)
    .bind(hours)
    .bind(req.starting_price_aet)
    .fetch_one(&mut *tx)
    .await?;
    let meta = json!({ "auction_id": auction.id, "duration_hours": hours });
    log_event(&mut tx, &handle, "auction_start", Some(req.requester), None, meta.clone()).await?;
    events::emit_handle(&mut tx, &handle, "handle.auction_start", meta).await?;
    tx.commit().await?;
    Ok((StatusCode::CREATED, Json(auction_json(&auction, &handle, 0))))
}

async fn load_auction(conn: &mut PgConnection, id: Uuid, lock: bool) -> ApiResult<(AuctionRow, String, i64)> {
    let a = sqlx::query_as::<_, AuctionRow>(&format!(
        "SELECT {AUCTION_COLS} FROM handle_auctions WHERE id = $1{}",
        if lock { " FOR UPDATE" } else { "" }
    ))
    .bind(id)
    .fetch_optional(&mut *conn)
    .await?
    .ok_or_else(|| ApiError::not_found("not_found", "No such auction."))?;
    let handle: String = sqlx::query_scalar("SELECT handle::text FROM handles WHERE id = $1")
        .bind(a.handle_id)
        .fetch_one(&mut *conn)
        .await?;
    let bids: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM auction_bids WHERE auction_id = $1")
        .bind(id)
        .fetch_one(&mut *conn)
        .await?;
    Ok((a, handle, bids))
}

pub async fn get_auction(State(state): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    let (a, handle, bids) = load_auction(&mut conn, id, false).await?;
    Ok(Json(auction_json(&a, &handle, bids)))
}

#[derive(Debug, Deserialize)]
pub struct BidReq {
    pub bidder_pial: Uuid,
    pub amount_aet: i32,
}

pub async fn bid(State(state): State<AppState>, Path(id): Path<Uuid>, Json(req): Json<BidReq>) -> ApiResult<Json<Value>> {
    let mut tx = state.pool.begin().await?;
    let (a, handle, _) = load_auction(&mut tx, id, true).await?;
    if a.status != "active" || a.end_time <= Utc::now() {
        return Err(ApiError::conflict("auction_closed", "The auction is not open."));
    }
    if req.amount_aet <= a.current_price_aet {
        return Err(ApiError::bad_request("bid_too_low", "A bid must exceed the current price."));
    }
    sqlx::query("INSERT INTO auction_bids (auction_id, bidder_pial, amount_aet) VALUES ($1, $2, $3)")
        .bind(id)
        .bind(req.bidder_pial)
        .bind(req.amount_aet)
        .execute(&mut *tx)
        .await?;
    sqlx::query("UPDATE handle_auctions SET current_price_aet = $1, highest_bidder_pial = $2 WHERE id = $3")
        .bind(req.amount_aet)
        .bind(req.bidder_pial)
        .bind(id)
        .execute(&mut *tx)
        .await?;
    let (a, _, bids) = load_auction(&mut tx, id, false).await?;
    tx.commit().await?;
    Ok(Json(auction_json(&a, &handle, bids)))
}

/// Settles an auction: the handle goes to the highest bidder, or back to its
/// owner when nobody bid. The caller collects the AET through Ain Soph.
pub async fn settle(State(state): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let mut tx = state.pool.begin().await?;
    let (a, handle, _) = load_auction(&mut tx, id, true).await?;
    if a.status != "active" {
        return Err(ApiError::conflict("auction_closed", "The auction is already settled or cancelled."));
    }
    let new_status = if a.highest_bidder_pial.is_some() { "settled" } else { "ended" };
    sqlx::query("UPDATE handle_auctions SET status = $1 WHERE id = $2")
        .bind(new_status)
        .bind(id)
        .execute(&mut *tx)
        .await?;
    match a.highest_bidder_pial {
        Some(winner) => {
            let from: Uuid = sqlx::query_scalar(
                "UPDATE handles h SET pial_id = $1, status = 'active', last_bound_at = NOW(), updated_at = NOW()
                   FROM handles old WHERE h.id = $2 AND old.id = h.id RETURNING old.pial_id",
            )
            .bind(winner)
            .bind(a.handle_id)
            .fetch_one(&mut *tx)
            .await?;
            let meta = json!({ "auction_id": id, "price_aet": a.current_price_aet });
            log_event(&mut tx, &handle, "auction_settle", Some(from), Some(winner), meta.clone()).await?;
            events::emit_handle(&mut tx, &handle, "handle.auction_settle", meta).await?;
        }
        None => {
            sqlx::query("UPDATE handles SET status = 'active', updated_at = NOW() WHERE id = $1")
                .bind(a.handle_id)
                .execute(&mut *tx)
                .await?;
            let meta = json!({ "auction_id": id, "bids": 0 });
            log_event(&mut tx, &handle, "auction_end", None, None, meta.clone()).await?;
            events::emit_handle(&mut tx, &handle, "handle.auction_end", meta).await?;
        }
    }
    tx.commit().await?;
    Ok(Json(json!({
        "status": new_status,
        "handle": handle,
        "winner": a.highest_bidder_pial,
        "price_aet": a.current_price_aet,
    })))
}

// ── Search and the reserved namespace ─────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct SearchQuery {
    pub q: String,
}

pub async fn search(State(state): State<AppState>, Query(params): Query<SearchQuery>) -> ApiResult<Json<Value>> {
    let q = norm(&params.q);
    if q.is_empty() || q.len() > 30 {
        return Err(ApiError::bad_request("query_invalid", "q must be 1–30 characters."));
    }
    let mut conn = state.pool.acquire().await?;
    // The prefix is matched literally: LIKE metacharacters in it are escaped.
    let pattern = format!("{}%", q.replace('\\', "\\\\").replace('%', "\\%").replace('_', "\\_"));
    let rows = sqlx::query_as::<_, HandleRow>(&format!(
        "SELECT {HANDLE_COLS} FROM handles WHERE handle::text LIKE $1 ORDER BY handle LIMIT 20"
    ))
    .bind(&pattern)
    .fetch_all(&mut *conn)
    .await?;
    let reserved: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM reserved_handles WHERE handle = $1::citext)")
        .bind(&q)
        .fetch_one(&mut *conn)
        .await?;
    let mut results: Vec<Value> = Vec::with_capacity(rows.len() + 1);
    if !reserved && vocab::handle_is_valid(&q) && !rows.iter().any(|r| r.handle == q) {
        results.push(json!({ "handle": q, "status": "available", "tier": "standard", "available": true }));
    }
    results.extend(rows.iter().map(|r| json!({ "handle": r.handle, "status": r.status, "tier": r.tier, "available": false })));
    Ok(Json(Value::Array(results)))
}

pub async fn list_reserved(State(state): State<AppState>) -> ApiResult<Json<Value>> {
    let rows: Vec<(String, String)> =
        sqlx::query_as("SELECT handle::text, reason FROM reserved_handles ORDER BY handle")
            .fetch_all(&state.pool)
            .await?;
    Ok(Json(json!({
        "reserved": rows.into_iter().map(|(h, r)| json!({ "handle": h, "reason": r })).collect::<Vec<_>>()
    })))
}

#[derive(Debug, Deserialize)]
pub struct ReserveReq {
    pub handle: String,
    #[serde(default = "default_reason")]
    pub reason: String,
}
fn default_reason() -> String {
    "admin".into()
}

/// Reserves a handle against future claims. A handle already registered keeps
/// its holder; reclaiming it is the admin `reclaim` action.
pub async fn reserve(State(state): State<AppState>, Json(req): Json<ReserveReq>) -> ApiResult<StatusCode> {
    let handle = norm(&req.handle);
    if handle.is_empty() || handle.len() > 30 {
        return Err(ApiError::bad_request("invalid_handle", "A handle is 1–30 characters."));
    }
    if !matches!(req.reason.as_str(), "system" | "trademark" | "route" | "admin" | "offensive") {
        return Err(ApiError::bad_request(
            "reason_invalid",
            "reason must be system, trademark, route, admin or offensive.",
        ));
    }
    sqlx::query(
        "INSERT INTO reserved_handles (handle, reason) VALUES ($1::citext, $2)
         ON CONFLICT (handle) DO UPDATE SET reason = EXCLUDED.reason",
    )
    .bind(&handle)
    .bind(&req.reason)
    .execute(&state.pool)
    .await?;
    Ok(StatusCode::NO_CONTENT)
}
