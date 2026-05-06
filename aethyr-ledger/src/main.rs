mod db;
mod models;

use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::Json,
    routing::{get, post},
    Router,
};
use serde::Deserialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::PgPool;
use std::sync::Arc;
use tracing::info;
use tracing_subscriber::{layer::SubscriberExt, util::SubscriberInitExt, EnvFilter};
use uuid::Uuid;

use models::*;

#[derive(Clone)]
struct AppState {
    pool: PgPool,
}

type Res<T> = Result<Json<T>, (StatusCode, Json<Value>)>;

fn err(status: StatusCode, msg: &str) -> (StatusCode, Json<Value>) {
    (status, Json(json!({ "error": msg })))
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::registry()
        .with(EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()))
        .with(tracing_subscriber::fmt::layer())
        .init();

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8096".into());

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Aethyr Ledger schema up to date");

    // Background: seal pending events into blocks every 2 seconds
    let sealer_pool = pool.clone();
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(tokio::time::Duration::from_secs(2));
        loop {
            interval.tick().await;
            if let Err(e) = seal_block(&sealer_pool).await {
                tracing::warn!("block sealer: {e}");
            }
        }
    });

    let state = Arc::new(AppState { pool });

    let app = Router::new()
        .route("/health",                      get(health))
        // Account
        .route("/v1/accounts/:pial_id",       get(get_balance))
        .route("/v1/accounts/:pial_id/history", get(get_history))
        // Funding rails (fiat → credit → AET)
        .route("/v1/fund",                     post(fund_wallet))
        // Transfers (all internal AET movements)
        .route("/v1/transfer",                 post(transfer))
        // Relay node rewards (mint from reserves)
        .route("/v1/reward",                   post(relay_reward))
        // Burn (boost, penalty)
        .route("/v1/burn",                     post(burn))
        // Admin mint (grants, bootstrap)
        .route("/v1/admin/mint",               post(admin_mint))
        // Chain inspection
        .route("/v1/supply",                   get(get_supply))
        .route("/v1/blocks",                   get(get_blocks))
        .route("/v1/blocks/:number",           get(get_block))
        .with_state(state);

    let addr = format!("0.0.0.0:{port}");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    info!("Aethyr Ledger listening on {addr}");
    axum::serve(listener, app).await?;
    Ok(())
}

// ── Health ────────────────────────────────────────────────────────────────────

async fn health() -> Json<Value> {
    Json(json!({ "status": "ok", "service": "aethyr-ledger" }))
}

// ── GET /v1/accounts/:pial_id ─────────────────────────────────────────────────

async fn get_balance(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<Uuid>,
) -> Res<BalanceResp> {
    db::ensure_account(&state.pool, pial_id).await;

    let row = sqlx::query_as::<_, AccountRow>(
        "SELECT * FROM ledger_accounts WHERE pial_id = $1",
    )
    .bind(pial_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    .ok_or_else(|| err(StatusCode::NOT_FOUND, "account not found"))?;

    Ok(Json(BalanceResp {
        pial_id,
        balance_uaet:   row.balance_uaet,
        balance_aet:    uaet_to_aet(row.balance_uaet),
        credit_balance: row.credit_balance,
        total_earned:   row.total_earned,
        total_spent:    row.total_spent,
    }))
}

// ── GET /v1/accounts/:pial_id/history ────────────────────────────────────────

#[derive(Deserialize)]
struct HistoryQuery {
    limit:  Option<i64>,
    offset: Option<i64>,
}

async fn get_history(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<Uuid>,
    Query(q): Query<HistoryQuery>,
) -> Res<Vec<EventResp>> {
    let limit  = q.limit.unwrap_or(50).min(200);
    let offset = q.offset.unwrap_or(0);

    let rows = sqlx::query_as::<_, EventRow>(
        "SELECT * FROM ledger_events
         WHERE (from_pial = $1 OR to_pial = $1) AND status = 'confirmed'
         ORDER BY created_at DESC LIMIT $2 OFFSET $3",
    )
    .bind(pial_id)
    .bind(limit)
    .bind(offset)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    Ok(Json(rows.iter().map(to_event_resp).collect()))
}

// ── POST /v1/fund ─────────────────────────────────────────────────────────────
// Fiat funding rail: credit purchase → AET mint.

async fn fund_wallet(
    State(state): State<Arc<AppState>>,
    Json(req): Json<FundReq>,
) -> Res<FundResp> {
    let valid_rails = ["apple_iap","gift_card","card","google_play","crypto","internal"];
    if !valid_rails.contains(&req.rail.as_str()) {
        return Err(err(StatusCode::BAD_REQUEST, "invalid funding rail"));
    }
    if req.fiat_amount <= 0 {
        return Err(err(StatusCode::BAD_REQUEST, "fiat_amount must be positive"));
    }

    // Platform-controlled conversion: 1 cent fiat → 1 Aethyr Credit → 100 micro-AET
    // Platform fee: 2.5% of credit on deposit
    let gross_credit  = req.fiat_amount; // 1:1 cent → credit
    let fee_credit    = (gross_credit * FEE_DEPOSIT) / 10_000;
    let net_credit    = gross_credit - fee_credit;
    let aet_to_mint   = net_credit * AC_TO_UAET;

    db::ensure_account(&state.pool, req.pial_id).await;

    // Record funding event
    let funding_id: Uuid = sqlx::query_scalar(
        "INSERT INTO funding_events
            (pial_id, rail, fiat_amount, fiat_currency, credit_amount, status, external_ref, metadata, confirmed_at)
         VALUES ($1,$2,$3,$4,$5,'confirmed',$6,$7,NOW())
         RETURNING id",
    )
    .bind(req.pial_id)
    .bind(&req.rail)
    .bind(req.fiat_amount)
    .bind(req.fiat_currency.as_deref().unwrap_or("USD"))
    .bind(net_credit)
    .bind(req.external_ref.as_deref())
    .bind(req.metadata.clone().unwrap_or(json!({})))
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    // Mint AET to user wallet
    mint_internal(&state.pool, req.pial_id, aet_to_mint, "PURCHASE",
        json!({ "rail": req.rail, "funding_id": funding_id, "fiat_cents": req.fiat_amount }))
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e))?;

    let new_balance = db::get_balance(&state.pool, req.pial_id).await;

    Ok(Json(FundResp {
        funding_id,
        credit_amount: net_credit,
        aet_minted: aet_to_mint,
        new_balance,
    }))
}

// ── POST /v1/transfer ─────────────────────────────────────────────────────────

async fn transfer(
    State(state): State<Arc<AppState>>,
    Json(req): Json<TransferReq>,
) -> Res<EventResp> {
    let valid_types = ["TRANSFER","TIP","SUBSCRIPTION","CREATOR_PAYOUT"];
    if !valid_types.contains(&req.event_type.as_str()) {
        return Err(err(StatusCode::BAD_REQUEST, "invalid event_type for transfer"));
    }
    if req.amount_uaet <= 0 {
        return Err(err(StatusCode::BAD_REQUEST, "amount must be positive"));
    }
    if req.from_pial == req.to_pial {
        return Err(err(StatusCode::BAD_REQUEST, "from and to PIAL must differ"));
    }

    let fee_bp = match req.event_type.as_str() {
        "TIP"          => FEE_TIP,
        "SUBSCRIPTION" => FEE_SUBSCRIPTION,
        _              => FEE_TRANSFER,
    };
    let fee_uaet  = fee_for(req.amount_uaet, fee_bp);
    let total_out = req.amount_uaet + fee_uaet;

    db::ensure_account(&state.pool, req.from_pial).await;
    db::ensure_account(&state.pool, req.to_pial).await;

    // Atomic balance check + debit + credit using a transaction
    let mut tx = state.pool.begin().await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    // Lock sender row
    let sender_balance: i64 = sqlx::query_scalar(
        "SELECT balance_uaet FROM ledger_accounts WHERE pial_id = $1 FOR UPDATE",
    )
    .bind(req.from_pial)
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    if sender_balance < total_out {
        return Err(err(StatusCode::PAYMENT_REQUIRED, "insufficient balance"));
    }

    const TREASURY: &str = "00000000-0000-0000-0000-000000000000";

    // Debit sender + increment sequence (XRP-style ordering)
    let seq_no: i64 = sqlx::query_scalar(
        "UPDATE ledger_accounts
         SET balance_uaet = balance_uaet - $1,
             total_spent   = total_spent   + $1,
             sequence_no   = sequence_no   + 1,
             updated_at    = NOW()
         WHERE pial_id = $2
         RETURNING sequence_no",
    )
    .bind(total_out)
    .bind(req.from_pial)
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    // Credit receiver (net of fee)
    sqlx::query(
        "UPDATE ledger_accounts
         SET balance_uaet = balance_uaet + $1,
             total_earned  = total_earned  + $1,
             updated_at    = NOW()
         WHERE pial_id = $2",
    )
    .bind(req.amount_uaet)
    .bind(req.to_pial)
    .execute(&mut *tx)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    // Route fee to treasury account
    if fee_uaet > 0 {
        sqlx::query(
            "INSERT INTO ledger_accounts (pial_id) VALUES ($1) ON CONFLICT DO NOTHING",
        )
        .bind(TREASURY)
        .execute(&mut *tx)
        .await
        .ok();

        sqlx::query(
            "UPDATE ledger_accounts
             SET balance_uaet = balance_uaet + $1, total_earned = total_earned + $1, updated_at = NOW()
             WHERE pial_id = $2::uuid",
        )
        .bind(fee_uaet)
        .bind(TREASURY)
        .execute(&mut *tx)
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;
    }

    // Record event with sequence number
    let row = sqlx::query_as::<_, EventRow>(
        "INSERT INTO ledger_events
            (event_type, from_pial, to_pial, amount_uaet, fee_uaet,
             seq_from, glyph_sig, idempotency_key, metadata)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
         RETURNING *",
    )
    .bind(&req.event_type)
    .bind(req.from_pial)
    .bind(req.to_pial)
    .bind(req.amount_uaet)
    .bind(fee_uaet)
    .bind(seq_no)
    .bind(req.glyph_sig.as_deref())
    .bind(req.idempotency_key.as_deref())
    .bind(req.metadata.clone().unwrap_or(json!({})))
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    tx.commit().await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    Ok(Json(to_event_resp(&row)))
}

// ── POST /v1/reward ───────────────────────────────────────────────────────────
// Mint relay node reward from platform reserves.

async fn relay_reward(
    State(state): State<Arc<AppState>>,
    Json(req): Json<RelayRewardReq>,
) -> Res<EventResp> {
    if req.amount_uaet <= 0 {
        return Err(err(StatusCode::BAD_REQUEST, "amount must be positive"));
    }

    db::ensure_account(&state.pool, req.node_pial).await;

    mint_internal(
        &state.pool,
        req.node_pial,
        req.amount_uaet,
        "RELAY_REWARD",
        req.metadata.clone().unwrap_or(json!({})),
    )
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e))?;

    let row = sqlx::query_as::<_, EventRow>(
        "SELECT * FROM ledger_events
         WHERE to_pial = $1 AND event_type = 'RELAY_REWARD'
         ORDER BY created_at DESC LIMIT 1",
    )
    .bind(req.node_pial)
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    Ok(Json(to_event_resp(&row)))
}

// ── POST /v1/burn ─────────────────────────────────────────────────────────────

async fn burn(
    State(state): State<Arc<AppState>>,
    Json(req): Json<BurnReq>,
) -> Res<EventResp> {
    if req.amount_uaet <= 0 {
        return Err(err(StatusCode::BAD_REQUEST, "amount must be positive"));
    }

    let balance = db::get_balance(&state.pool, req.from_pial).await;
    if balance < req.amount_uaet {
        return Err(err(StatusCode::PAYMENT_REQUIRED, "insufficient balance"));
    }

    let mut tx = state.pool.begin().await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    sqlx::query(
        "UPDATE ledger_accounts
         SET balance_uaet = balance_uaet - $1,
             total_spent = total_spent + $1,
             total_burned = total_burned + $1,
             updated_at = NOW()
         WHERE pial_id = $2",
    )
    .bind(req.amount_uaet)
    .bind(req.from_pial)
    .execute(&mut *tx)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let row = sqlx::query_as::<_, EventRow>(
        "INSERT INTO ledger_events
            (event_type, from_pial, to_pial, amount_uaet, idempotency_key, metadata)
         VALUES ('BURN', $1, NULL, $2, $3, $4)
         RETURNING *",
    )
    .bind(req.from_pial)
    .bind(req.amount_uaet)
    .bind(req.idempotency_key.as_deref())
    .bind(json!({ "reason": req.reason }))
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    sqlx::query(
        "UPDATE aet_supply
         SET total_burned = total_burned + $1,
             circulating  = GREATEST(0, circulating - $1),
             total_supply = GREATEST(0, total_supply - $1),
             updated_at   = NOW()",
    )
    .bind(req.amount_uaet)
    .execute(&mut *tx)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    tx.commit().await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    Ok(Json(to_event_resp(&row)))
}

// ── POST /v1/admin/mint ───────────────────────────────────────────────────────

async fn admin_mint(
    State(state): State<Arc<AppState>>,
    Json(req): Json<MintReq>,
) -> Res<EventResp> {
    if req.amount_uaet <= 0 {
        return Err(err(StatusCode::BAD_REQUEST, "amount must be positive"));
    }

    db::ensure_account(&state.pool, req.to_pial).await;

    mint_internal(
        &state.pool,
        req.to_pial,
        req.amount_uaet,
        "MINT",
        json!({ "reason": req.reason, "admin": req.admin_id }),
    )
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e))?;

    let row = sqlx::query_as::<_, EventRow>(
        "SELECT * FROM ledger_events WHERE to_pial = $1 AND event_type = 'MINT'
         ORDER BY created_at DESC LIMIT 1",
    )
    .bind(req.to_pial)
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    Ok(Json(to_event_resp(&row)))
}

// ── GET /v1/supply ────────────────────────────────────────────────────────────

async fn get_supply(State(state): State<Arc<AppState>>) -> Res<SupplyResp> {
    let (total, circ, minted, burned): (i64, i64, i64, i64) = sqlx::query_as(
        "SELECT total_supply, circulating, total_minted, total_burned FROM aet_supply WHERE id = 1",
    )
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    Ok(Json(SupplyResp {
        total_supply_uaet: total,
        total_supply_aet:  uaet_to_aet(total),
        circulating_uaet:  circ,
        circulating_aet:   uaet_to_aet(circ),
        total_minted:      minted,
        total_burned:      burned,
    }))
}

// ── GET /v1/blocks ────────────────────────────────────────────────────────────

#[derive(Deserialize)]
struct BlocksQuery { limit: Option<i64> }

async fn get_blocks(
    State(state): State<Arc<AppState>>,
    Query(q): Query<BlocksQuery>,
) -> Res<Vec<BlockResp>> {
    let limit = q.limit.unwrap_or(20).min(100);
    let rows = sqlx::query_as::<_, BlockRow>(
        "SELECT * FROM ledger_blocks ORDER BY block_number DESC LIMIT $1",
    )
    .bind(limit)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    Ok(Json(rows.iter().map(to_block_resp).collect()))
}

// ── GET /v1/blocks/:number ────────────────────────────────────────────────────

async fn get_block(
    State(state): State<Arc<AppState>>,
    Path(number): Path<i64>,
) -> Res<BlockResp> {
    let row = sqlx::query_as::<_, BlockRow>(
        "SELECT * FROM ledger_blocks WHERE block_number = $1",
    )
    .bind(number)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    .ok_or_else(|| err(StatusCode::NOT_FOUND, "block not found"))?;

    Ok(Json(to_block_resp(&row)))
}

// ── Block sealer (background task) ────────────────────────────────────────────
// Groups unassigned events into a block every 2 seconds. Hash-chains them.

async fn seal_block(pool: &PgPool) -> anyhow::Result<()> {
    let count: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM ledger_events WHERE block_id IS NULL AND status = 'confirmed'",
    )
    .fetch_one(pool)
    .await?;

    if count == 0 { return Ok(()); }

    let block_num   = db::next_block_number(pool).await;
    let prev_hash   = db::prev_block_hash(pool).await;
    let block_id    = Uuid::new_v4();

    // Assign block_id to pending events
    sqlx::query(
        "UPDATE ledger_events SET block_id = $1
         WHERE block_id IS NULL AND status = 'confirmed'",
    )
    .bind(block_id)
    .execute(pool)
    .await?;

    // Aggregate block stats
    let (volume, fees): (i64, i64) = sqlx::query_as(
        "SELECT COALESCE(SUM(amount_uaet)::bigint, 0), COALESCE(SUM(fee_uaet)::bigint, 0)
         FROM ledger_events WHERE block_id = $1",
    )
    .bind(block_id)
    .fetch_one(pool)
    .await?;

    // Events root: SHA-256(sorted event IDs concatenated) — lightweight commitment
    // Verifiable: any auditor can fetch the event list and reproduce this hash.
    let event_ids: Vec<String> = sqlx::query_scalar(
        "SELECT id::text FROM ledger_events WHERE block_id = $1 ORDER BY created_at ASC",
    )
    .bind(block_id)
    .fetch_all(pool)
    .await
    .unwrap_or_default();

    let mut root_hasher = Sha256::new();
    for eid in &event_ids { root_hasher.update(eid.as_bytes()); }
    let events_root = hex::encode(root_hasher.finalize());

    // Block hash: SHA-256(block_num | prev_hash | events_root | count | volume | timestamp)
    let ts   = chrono::Utc::now().timestamp_millis().to_string();
    let data = format!("{block_num}{prev_hash}{events_root}{count}{volume}{ts}");
    let mut hasher = Sha256::new();
    hasher.update(data.as_bytes());
    let block_hash = hex::encode(hasher.finalize());

    sqlx::query(
        "INSERT INTO ledger_blocks
            (id, block_number, prev_hash, block_hash, events_root, event_count, total_volume, fee_collected)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8)",
    )
    .bind(block_id)
    .bind(block_num)
    .bind(&prev_hash)
    .bind(&block_hash)
    .bind(&events_root)
    .bind(count as i32)
    .bind(volume)
    .bind(fees)
    .execute(pool)
    .await?;

    info!("sealed block #{block_num} — {count} events — hash {}", &block_hash[..16]);
    Ok(())
}

// ── Internal mint helper ──────────────────────────────────────────────────────

async fn mint_internal(
    pool: &PgPool,
    to_pial: Uuid,
    amount: i64,
    event_type: &str,
    metadata: serde_json::Value,
) -> Result<(), String> {
    let mut tx = pool.begin().await.map_err(|e| e.to_string())?;

    sqlx::query(
        "UPDATE ledger_accounts
         SET balance_uaet = balance_uaet + $1,
             total_earned  = total_earned  + $1,
             total_minted  = total_minted  + $1,
             updated_at    = NOW()
         WHERE pial_id = $2",
    )
    .bind(amount)
    .bind(to_pial)
    .execute(&mut *tx)
    .await
    .map_err(|e| e.to_string())?;

    sqlx::query(
        "INSERT INTO ledger_events (event_type, from_pial, to_pial, amount_uaet, metadata)
         VALUES ($1, NULL, $2, $3, $4)",
    )
    .bind(event_type)
    .bind(to_pial)
    .bind(amount)
    .bind(metadata)
    .execute(&mut *tx)
    .await
    .map_err(|e| e.to_string())?;

    sqlx::query(
        "UPDATE aet_supply
         SET total_supply  = total_supply  + $1,
             circulating   = circulating   + $1,
             total_minted  = total_minted  + $1,
             updated_at    = NOW()",
    )
    .bind(amount)
    .execute(&mut *tx)
    .await
    .map_err(|e| e.to_string())?;

    tx.commit().await.map_err(|e| e.to_string())?;
    Ok(())
}

// ── Response converters ───────────────────────────────────────────────────────

fn to_event_resp(r: &EventRow) -> EventResp {
    EventResp {
        event_id:    r.id,
        event_type:  r.event_type.clone(),
        from_pial:   r.from_pial,
        to_pial:     r.to_pial,
        amount_uaet: r.amount_uaet,
        amount_aet:  uaet_to_aet(r.amount_uaet),
        fee_uaet:    r.fee_uaet,
        status:      r.status.clone(),
        created_at:  r.created_at,
    }
}

fn to_block_resp(r: &BlockRow) -> BlockResp {
    BlockResp {
        block_number:  r.block_number,
        block_hash:    r.block_hash.clone(),
        prev_hash:     r.prev_hash.clone(),
        events_root:   r.events_root.clone(),
        event_count:   r.event_count,
        total_volume:  r.total_volume,
        fee_collected: r.fee_collected,
        sealed_at:     r.sealed_at,
    }
}
