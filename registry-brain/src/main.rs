mod db;
mod manhattan_outbox;
mod models;
mod observ;

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::Json,
    routing::{get, post},
    Router,
};
use serde::Deserialize;
use serde_json::{json, Value};
use sqlx::PgPool;
use std::sync::Arc;
use tracing::info;
use uuid::Uuid;

use manhattan_client::Manhattan;
use models::*;

#[derive(Clone)]
struct AppState {
    pool: PgPool,
    /// The naming plane, reached by name. This brain is the authority for the
    /// `handle` namespace, so every handle lifecycle transition is also a name
    /// operation here — delivered durably through `manhattan_outbox`, never
    /// from a request handler.
    manhattan: Manhattan,
}

type Res<T> = Result<Json<T>, (StatusCode, Json<Value>)>;

fn err(status: StatusCode, msg: &str) -> (StatusCode, Json<Value>) {
    (status, Json(json!({ "error": msg })))
}

fn validate_handle(handle: &str) -> Result<(), (StatusCode, Json<Value>)> {
    if handle.is_empty() || handle.len() > 30 {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "handle must be 1–30 characters",
        ));
    }
    if !handle
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || c == '_')
    {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "handle may only contain a-z A-Z 0-9 _",
        ));
    }
    Ok(())
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("registrar")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8094".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Registry-Brain schema up to date");

    let manhattan = Manhattan::from_env("registry-brain")?;
    manhattan_outbox::spawn(database_url.clone(), manhattan.clone());

    let state = Arc::new(AppState { pool, manhattan });

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(health))
        .route("/v1/handles", post(register_handle))
        .route("/v1/handles/:handle", get(resolve_handle))
        .route("/v1/handles/:handle/bind", post(bind_handle))
        .route("/v1/handles/:handle/unbind", post(unbind_handle))
        .route("/v1/handles/:handle/transfer", post(transfer_handle))
        .route("/v1/handles/:handle/admin", post(admin_action))
        .route("/v1/handles/:handle/auction", post(create_auction))
        .route("/v1/auctions/:id", get(get_auction))
        .route("/v1/auctions/:id/bid", post(place_bid))
        .route("/v1/auctions/:id/settle", post(settle_auction))
        .route("/v1/search", get(search_handles))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware));

    let addr = format!("0.0.0.0:{port}");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    info!("Registry-Brain listening on {addr}");
    axum::serve(listener, app).await?;
    Ok(())
}

// ── Health ─────────────────────────────────────────────────────────────────────

async fn health(State(state): State<Arc<AppState>>) -> Json<Value> {
    // The outbox backlog is the honest measure of whether handle resolution
    // across the platform still agrees with this brain. A depth that only grows
    // means the naming plane is drifting, so it is reported here rather than
    // left to be discovered.
    match db::manhattan_outbox_pending(&state.pool).await {
        Ok(pending) => Json(json!({
            "status":                   "ok",
            "service":                  "registry-brain",
            "manhattan":                state.manhattan.configured(),
            "manhattan_outbox_pending": pending,
        })),
        Err(e) => Json(json!({
            "status":                 "degraded",
            "service":                "registry-brain",
            "manhattan":              state.manhattan.configured(),
            "manhattan_outbox_error": e.to_string(),
        })),
    }
}

// ── POST /v1/handles ──────────────────────────────────────────────────────────

async fn register_handle(
    State(state): State<Arc<AppState>>,
    Json(req): Json<RegisterHandleReq>,
) -> Res<HandleResp> {
    let handle = req.handle.to_lowercase();
    validate_handle(&handle)?;

    if db::is_reserved(&state.pool, &handle).await {
        return Err(err(StatusCode::CONFLICT, "handle is reserved"));
    }
    if db::handle_exists(&state.pool, &handle).await {
        return Err(err(StatusCode::CONFLICT, "handle already taken"));
    }

    let tier = match req.tier.as_str() {
        "standard" | "premium" | "elite" => req.tier.clone(),
        _ => return Err(err(StatusCode::BAD_REQUEST, "invalid tier")),
    };

    let row = sqlx::query_as::<_, HandleRow>(
        "INSERT INTO handles (handle, pial_id, tier) VALUES ($1, $2, $3) RETURNING *",
    )
    .bind(&handle)
    .bind(req.pial_id)
    .bind(&tier)
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    db::log_event(
        &state.pool,
        row.id,
        "register",
        None,
        Some(req.pial_id),
        json!({ "tier": tier }),
    )
    .await;
    Ok(Json(to_handle_resp(row)))
}

// ── GET /v1/handles/:handle ───────────────────────────────────────────────────

async fn resolve_handle(
    State(state): State<Arc<AppState>>,
    Path(handle): Path<String>,
) -> Res<ResolveResp> {
    let handle = handle.to_lowercase();

    let row = sqlx::query_as::<_, HandleRow>("SELECT * FROM handles WHERE handle = $1")
        .bind(&handle)
        .fetch_optional(&state.pool)
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "handle not found"))?;

    Ok(Json(ResolveResp {
        handle: row.handle,
        pial_id: row.pial_id,
        status: row.status,
        tier: row.tier,
        cached: false,
    }))
}

// ── POST /v1/handles/:handle/bind ─────────────────────────────────────────────

async fn bind_handle(
    State(state): State<Arc<AppState>>,
    Path(handle): Path<String>,
    Json(req): Json<BindHandleReq>,
) -> Res<HandleResp> {
    let handle = handle.to_lowercase();

    let row = sqlx::query_as::<_, HandleRow>(
        "UPDATE handles
         SET pial_id = $1, status = 'active', last_bound_at = NOW(), updated_at = NOW()
         WHERE handle = $2 AND status IN ('inactive','quarantined')
         RETURNING *",
    )
    .bind(req.pial_id)
    .bind(&handle)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    .ok_or_else(|| err(StatusCode::NOT_FOUND, "handle not found or not bindable"))?;

    db::log_event(
        &state.pool,
        row.id,
        "bind",
        None,
        Some(req.pial_id),
        json!({ "requester": req.requester }),
    )
    .await;
    Ok(Json(to_handle_resp(row)))
}

// ── POST /v1/handles/:handle/unbind ──────────────────────────────────────────

async fn unbind_handle(
    State(state): State<Arc<AppState>>,
    Path(handle): Path<String>,
    Json(req): Json<UnbindHandleReq>,
) -> Res<HandleResp> {
    let handle = handle.to_lowercase();

    let row = sqlx::query_as::<_, HandleRow>(
        "UPDATE handles
         SET status = 'inactive', updated_at = NOW()
         WHERE handle = $1 AND pial_id = $2 AND status = 'active'
         RETURNING *",
    )
    .bind(&handle)
    .bind(req.requester)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    .ok_or_else(|| {
        err(
            StatusCode::NOT_FOUND,
            "handle not found or not owned by requester",
        )
    })?;

    db::log_event(
        &state.pool,
        row.id,
        "unbind",
        Some(req.requester),
        None,
        json!({}),
    )
    .await;
    Ok(Json(to_handle_resp(row)))
}

// ── POST /v1/handles/:handle/transfer ────────────────────────────────────────

async fn transfer_handle(
    State(state): State<Arc<AppState>>,
    Path(handle): Path<String>,
    Json(req): Json<TransferHandleReq>,
) -> Res<HandleResp> {
    let handle = handle.to_lowercase();

    let row = sqlx::query_as::<_, HandleRow>(
        "UPDATE handles
         SET pial_id = $1, last_bound_at = NOW(), updated_at = NOW()
         WHERE handle = $2 AND pial_id = $3 AND status = 'active'
         RETURNING *",
    )
    .bind(req.to_pial)
    .bind(&handle)
    .bind(req.from_pial)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    .ok_or_else(|| {
        err(
            StatusCode::NOT_FOUND,
            "handle not found or not owned by from_pial",
        )
    })?;

    db::log_event(
        &state.pool,
        row.id,
        "transfer",
        Some(req.from_pial),
        Some(req.to_pial),
        json!({ "note": "followers/reputation/wallet remain with from_pial" }),
    )
    .await;
    Ok(Json(to_handle_resp(row)))
}

// ── POST /v1/handles/:handle/admin ────────────────────────────────────────────

async fn admin_action(
    State(state): State<Arc<AppState>>,
    Path(handle): Path<String>,
    Json(req): Json<AdminActionReq>,
) -> Res<HandleResp> {
    let handle = handle.to_lowercase();

    let new_status = match req.action.as_str() {
        "freeze" => "frozen",
        "unfreeze" => "active",
        "quarantine" => "quarantined",
        "reclaim" => "reserved",
        _ => {
            return Err(err(
                StatusCode::BAD_REQUEST,
                "unknown action: freeze | unfreeze | quarantine | reclaim",
            ))
        }
    };

    let row = sqlx::query_as::<_, HandleRow>(
        "UPDATE handles SET status = $1, updated_at = NOW() WHERE handle = $2 RETURNING *",
    )
    .bind(new_status)
    .bind(&handle)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    .ok_or_else(|| err(StatusCode::NOT_FOUND, "handle not found"))?;

    db::log_event(
        &state.pool,
        row.id,
        &req.action,
        None,
        None,
        json!({ "admin": req.admin_id, "reason": req.reason }),
    )
    .await;
    Ok(Json(to_handle_resp(row)))
}

// ── POST /v1/handles/:handle/auction ─────────────────────────────────────────

async fn create_auction(
    State(state): State<Arc<AppState>>,
    Path(handle): Path<String>,
    Json(req): Json<CreateAuctionReq>,
) -> Res<AuctionResp> {
    let handle = handle.to_lowercase();
    let duration_hours = (req.duration_hours.max(1).min(168)) as i32;

    let hrow = sqlx::query_as::<_, HandleRow>(
        "UPDATE handles SET status = 'auction', updated_at = NOW()
         WHERE handle = $1 AND pial_id = $2 AND status = 'active'
         RETURNING *",
    )
    .bind(&handle)
    .bind(req.requester)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    .ok_or_else(|| {
        err(
            StatusCode::NOT_FOUND,
            "handle not found or not owned by requester",
        )
    })?;

    let arow = sqlx::query_as::<_, AuctionRow>(
        "INSERT INTO handle_auctions (handle_id, end_time, starting_price_aet, current_price_aet)
         VALUES ($1, NOW() + $2 * INTERVAL '1 hour', $3, $3)
         RETURNING *",
    )
    .bind(hrow.id)
    .bind(duration_hours)
    .bind(req.starting_price_aet)
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    db::log_event(
        &state.pool,
        hrow.id,
        "auction_start",
        Some(req.requester),
        None,
        json!({ "auction_id": arow.id, "duration_hours": duration_hours }),
    )
    .await;
    Ok(Json(to_auction_resp(arow, &handle, 0)))
}

// ── GET /v1/auctions/:id ─────────────────────────────────────────────────────

async fn get_auction(
    State(state): State<Arc<AppState>>,
    Path(auction_id): Path<Uuid>,
) -> Res<AuctionResp> {
    let (arow, handle, bid_count) = fetch_auction(&state.pool, auction_id).await?;
    Ok(Json(to_auction_resp(arow, &handle, bid_count)))
}

// ── POST /v1/auctions/:id/bid ─────────────────────────────────────────────────

async fn place_bid(
    State(state): State<Arc<AppState>>,
    Path(auction_id): Path<Uuid>,
    Json(req): Json<PlaceBidReq>,
) -> Res<AuctionResp> {
    let (arow, handle, _) = fetch_auction(&state.pool, auction_id).await?;

    if arow.status != "active" {
        return Err(err(StatusCode::CONFLICT, "auction is not active"));
    }
    if req.amount_aet <= arow.current_price_aet {
        return Err(err(
            StatusCode::BAD_REQUEST,
            "bid must exceed current price",
        ));
    }

    sqlx::query(
        "INSERT INTO auction_bids (auction_id, bidder_pial, amount_aet) VALUES ($1, $2, $3)",
    )
    .bind(auction_id)
    .bind(req.bidder_pial)
    .bind(req.amount_aet)
    .execute(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let updated = sqlx::query_as::<_, AuctionRow>(
        "UPDATE handle_auctions SET current_price_aet = $1, highest_bidder_pial = $2
         WHERE id = $3 RETURNING *",
    )
    .bind(req.amount_aet)
    .bind(req.bidder_pial)
    .bind(auction_id)
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let bid_count =
        sqlx::query_scalar::<_, i64>("SELECT COUNT(*) FROM auction_bids WHERE auction_id = $1")
            .bind(auction_id)
            .fetch_one(&state.pool)
            .await
            .unwrap_or(0);

    Ok(Json(to_auction_resp(updated, &handle, bid_count)))
}

// ── POST /v1/auctions/:id/settle ─────────────────────────────────────────────

async fn settle_auction(
    State(state): State<Arc<AppState>>,
    Path(auction_id): Path<Uuid>,
) -> Res<Value> {
    let (arow, handle, _) = fetch_auction(&state.pool, auction_id).await?;

    if arow.status != "active" {
        return Err(err(
            StatusCode::CONFLICT,
            "auction already settled or cancelled",
        ));
    }

    let new_status = if arow.highest_bidder_pial.is_some() {
        "settled"
    } else {
        "ended"
    };

    sqlx::query("UPDATE handle_auctions SET status = $1 WHERE id = $2")
        .bind(new_status)
        .bind(auction_id)
        .execute(&state.pool)
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    if let Some(winner) = arow.highest_bidder_pial {
        // Transfer handle to winner. Caller is responsible for deducting AET via Ain Soph.
        sqlx::query(
            "UPDATE handles SET pial_id = $1, status = 'active', last_bound_at = NOW(), updated_at = NOW()
             WHERE id = $2",
        )
        .bind(winner)
        .bind(arow.handle_id)
        .execute(&state.pool)
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

        db::log_event(
            &state.pool,
            arow.handle_id,
            "auction_settle",
            None,
            Some(winner),
            json!({ "auction_id": auction_id, "price_aet": arow.current_price_aet }),
        )
        .await;
    } else {
        // No bids — return handle to active.
        sqlx::query("UPDATE handles SET status = 'active', updated_at = NOW() WHERE id = $1")
            .bind(arow.handle_id)
            .execute(&state.pool)
            .await
            .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;
    }

    Ok(Json(json!({
        "status":    new_status,
        "handle":    handle,
        "winner":    arow.highest_bidder_pial,
        "price_aet": arow.current_price_aet,
    })))
}

// ── GET /v1/search?q= ────────────────────────────────────────────────────────

#[derive(Deserialize)]
struct SearchQuery {
    q: String,
}

async fn search_handles(
    State(state): State<Arc<AppState>>,
    Query(params): Query<SearchQuery>,
) -> Res<Vec<SearchResult>> {
    let q = params.q.to_lowercase();
    if q.is_empty() || q.len() > 30 {
        return Err(err(StatusCode::BAD_REQUEST, "q must be 1–30 characters"));
    }

    let pattern = format!("{q}%");
    let rows = sqlx::query_as::<_, HandleRow>(
        "SELECT * FROM handles WHERE handle LIKE $1 ORDER BY handle LIMIT 20",
    )
    .bind(&pattern)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let reserved = db::is_reserved(&state.pool, &q).await;
    let exact_taken = rows.iter().any(|r| r.handle == q);

    let mut results: Vec<SearchResult> = rows
        .iter()
        .map(|r| SearchResult {
            handle: r.handle.clone(),
            status: r.status.clone(),
            tier: r.tier.clone(),
            available: false,
        })
        .collect();

    if !exact_taken && !reserved {
        results.insert(
            0,
            SearchResult {
                handle: q.clone(),
                status: "available".into(),
                tier: "standard".into(),
                available: true,
            },
        );
    }

    Ok(Json(results))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

fn to_handle_resp(r: HandleRow) -> HandleResp {
    HandleResp {
        id: r.id,
        handle: r.handle,
        pial_id: r.pial_id,
        status: r.status,
        tier: r.tier,
        created_at: r.created_at,
        last_bound_at: r.last_bound_at,
    }
}

fn to_auction_resp(a: AuctionRow, handle: &str, bid_count: i64) -> AuctionResp {
    AuctionResp {
        id: a.id,
        handle: handle.to_string(),
        start_time: a.start_time,
        end_time: a.end_time,
        starting_price_aet: a.starting_price_aet,
        current_price_aet: a.current_price_aet,
        highest_bidder_pial: a.highest_bidder_pial,
        status: a.status,
        bid_count,
    }
}

async fn fetch_auction(
    pool: &PgPool,
    auction_id: Uuid,
) -> Result<(AuctionRow, String, i64), (StatusCode, Json<Value>)> {
    let arow = sqlx::query_as::<_, AuctionRow>("SELECT * FROM handle_auctions WHERE id = $1")
        .bind(auction_id)
        .fetch_optional(pool)
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "auction not found"))?;

    let handle = sqlx::query_scalar::<_, String>("SELECT handle FROM handles WHERE id = $1")
        .bind(arow.handle_id)
        .fetch_optional(pool)
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
        .unwrap_or_default();

    let bid_count =
        sqlx::query_scalar::<_, i64>("SELECT COUNT(*) FROM auction_bids WHERE auction_id = $1")
            .bind(auction_id)
            .fetch_one(pool)
            .await
            .unwrap_or(0);

    Ok((arow, handle, bid_count))
}
