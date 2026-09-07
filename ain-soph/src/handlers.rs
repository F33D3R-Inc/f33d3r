/// HTTP handlers for Ain Soph — ETHRA/AET crypto network.
///
/// All write endpoints require an idempotency key. If the same key is submitted
/// twice, the original response is returned without re-executing the operation.
///
/// Storage unit: 1 AET = 100 micro-AET.
/// Fee schedule: deposit 2.5%, transfer 3.0%, tip 1.0%, withdrawal 2.0%.
/// Fees credited to platform float account "ain_soph_fees".
use axum::{
    extract::{FromRef, Path, Query, State},
    http::StatusCode,
    Json,
};
use sqlx::PgPool;
use tracing::{info, instrument, warn};
use uuid::Uuid;

use crate::auth::{Caller, InternalApiKey};
use crate::db;
use crate::error::WalletError;
use crate::identity::{self, FEE_ACCOUNT};
use crate::manhattan_outbox;
use crate::models::*;
use manhattan_client::Manhattan;

/// Shared state: the ledger, and the naming plane that says whose ledger row is
/// whose.
///
/// Both are reachable as their own `State` extractor through `FromRef`, so the
/// money handlers keep the `State<PgPool>` signature they already had and take
/// the resolver as a second extractor. The settlement path is untouched; only
/// the identity that path is attributed to changed.
#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    pub manhattan: Manhattan,
    /// Shared secret for the service-to-service lane. Held in state so the
    /// `Caller` extractor never reads the environment per request.
    pub internal_api_key: InternalApiKey,
}

impl FromRef<AppState> for PgPool {
    fn from_ref(state: &AppState) -> PgPool {
        state.pool.clone()
    }
}

impl FromRef<AppState> for Manhattan {
    fn from_ref(state: &AppState) -> Manhattan {
        state.manhattan.clone()
    }
}

impl FromRef<AppState> for InternalApiKey {
    fn from_ref(state: &AppState) -> InternalApiKey {
        state.internal_api_key.clone()
    }
}

// ── GET /health ───────────────────────────────────────────────────────────────

/// Liveness plus the two numbers that say whether identity resolution is
/// actually working: how deep the registration backlog is, and how many account
/// keys predate the identity constraint and still need reconciling.
///
/// A database this cannot read is a genuinely unhealthy wallet, so that answers
/// 503 rather than reporting a zero it did not measure. A backlog is reported,
/// not failed on — the outbox is durable and catches up on its own.
pub async fn health(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
) -> (StatusCode, Json<serde_json::Value>) {
    let backlog = match manhattan_outbox::backlog(&pool).await {
        Ok(b) => b,
        Err(e) => return health_degraded(&mh, "manhattan_outbox", &e),
    };
    let unresolved = match db::unresolved_account_keys(&pool).await {
        Ok(n) => n,
        Err(e) => return health_degraded(&mh, "accounts", &e),
    };

    (
        StatusCode::OK,
        Json(serde_json::json!({
            "status":                     "ok",
            "brain":                      "ain-soph",
            "manhattan":                  mh.configured(),
            "manhattan_outbox_pending":   backlog.pending,
            "manhattan_outbox_failed":    backlog.failed,
            "unresolved_account_keys":    unresolved,
        })),
    )
}

fn health_degraded(
    mh: &Manhattan,
    table: &str,
    cause: &sqlx::Error,
) -> (StatusCode, Json<serde_json::Value>) {
    warn!(table, error = %cause, "health check could not read the ledger");
    (
        StatusCode::SERVICE_UNAVAILABLE,
        Json(serde_json::json!({
            "status":    "degraded",
            "brain":     "ain-soph",
            "manhattan": mh.configured(),
            "error":     format!("reading {table}: {cause}"),
        })),
    )
}

// ── GET /v1/supply ────────────────────────────────────────────────────────────

pub async fn get_supply(State(pool): State<PgPool>) -> Result<Json<SupplyResponse>, WalletError> {
    let total: (i64,) = sqlx::query_as(
        "SELECT COALESCE(SUM(balance)::bigint, 0) FROM accounts WHERE user_id != $1",
    )
    .bind(FEE_ACCOUNT)
    .fetch_one(&pool)
    .await?;

    let fee_pool: (i64,) =
        sqlx::query_as("SELECT COALESCE(balance, 0) FROM accounts WHERE user_id = $1")
            .bind(FEE_ACCOUNT)
            .fetch_optional(&pool)
            .await?
            .unwrap_or((0,));

    let count: (i64,) = sqlx::query_as("SELECT COUNT(*) FROM accounts WHERE user_id != $1")
        .bind(FEE_ACCOUNT)
        .fetch_one(&pool)
        .await?;

    Ok(Json(SupplyResponse {
        token: TOKEN_NAME,
        ticker: TOKEN_TICKER,
        total_balance_units: total.0,
        total_balance_aet: format_aet(total.0),
        fee_pool_units: fee_pool.0,
        fee_pool_aet: format_aet(fee_pool.0),
        account_count: count.0,
        decimals: AET_DECIMALS,
    }))
}

// ── GET /v1/balance/:user_id ──────────────────────────────────────────────────

#[instrument(skip(pool, mh))]
pub async fn get_balance(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
    Path(user_id): Path<String>,
) -> Result<Json<BalanceResponse>, WalletError> {
    // Resolve before the auto-create below. This lookup opens an account on
    // first sight, so an unresolved path segment here is how a wallet keyed on
    // a handle or on another brain's row id gets minted in the first place.
    let user_id = identity::resolve_owner(&mh, &user_id).await?;

    // A read does not write. This used to auto-create the account on first
    // lookup, which is how wallets keyed on the wrong identifier came into
    // existence: anything that merely LOOKED at a balance minted a wallet for
    // whatever string it was handed. feed-engine's wallet page fell back to a
    // feed row id when a PIAL was missing, and the shadow-balance sweeper probed
    // every row id on boot — so the sweeper manufactured the very accounts it
    // then charged a transfer fee to clean up. Live proof before this change:
    // four wallet accounts for two people, half of them keyed on another brain's
    // primary key.
    //
    // An unknown identity now reads as a zero balance and stays unknown. The
    // account is created by the first real credit — deposit, tip, transfer —
    // where a genuine decision to give someone money is being made.
    let balance = db::get_balance(&pool, &user_id).await?.unwrap_or(0);

    let updated_at = db::get_account_updated_at(&pool, &user_id)
        .await?
        .unwrap_or_else(chrono::Utc::now);

    Ok(Json(BalanceResponse {
        user_id,
        balance_units: balance,
        balance_aet: format_aet(balance),
        token: TOKEN_TICKER,
        updated_at,
    }))
}

// ── GET /v1/transactions/:user_id ─────────────────────────────────────────────

#[instrument(skip(pool, mh))]
pub async fn list_transactions(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
    Path(user_id): Path<String>,
    Query(q): Query<TxHistoryQuery>,
) -> Result<Json<TxHistoryResponse>, WalletError> {
    // History is keyed the same way the ledger is, so it has to be asked for
    // the same way — otherwise a lookup by handle silently reports no history.
    let user_id = identity::resolve_owner(&mh, &user_id).await?;

    let limit = q.limit.clamp(1, 100);

    let rows: Vec<TxRow> = if let Some(before) = q.before {
        sqlx::query_as(
            r#"SELECT id, tx_type, from_user_id, to_user_id, amount, fee, net_amount, status, created_at
               FROM transactions
               WHERE (from_user_id = $1 OR to_user_id = $1) AND created_at < $2
               ORDER BY created_at DESC LIMIT $3"#,
        )
        .bind(&user_id).bind(before).bind(limit)
        .fetch_all(&pool).await?
    } else {
        sqlx::query_as(
            r#"SELECT id, tx_type, from_user_id, to_user_id, amount, fee, net_amount, status, created_at
               FROM transactions
               WHERE from_user_id = $1 OR to_user_id = $1
               ORDER BY created_at DESC LIMIT $2"#,
        )
        .bind(&user_id).bind(limit)
        .fetch_all(&pool).await?
    };

    let entries = rows
        .into_iter()
        .map(|r| {
            let (direction, counterparty) = if r.from_user_id.as_deref() == Some(&user_id) {
                ("out".into(), r.to_user_id.clone())
            } else if r.to_user_id.as_deref() == Some(&user_id) {
                ("in".into(), r.from_user_id.clone())
            } else {
                ("self".into(), None)
            };
            TxHistoryEntry {
                id: r.id,
                tx_type: r.tx_type,
                direction,
                counterparty,
                amount_units: r.amount,
                amount_aet: format_aet(r.amount),
                net_units: r.net_amount,
                net_aet: format_aet(r.net_amount),
                status: r.status,
                created_at: r.created_at,
            }
        })
        .collect::<Vec<_>>();

    let count = entries.len();
    Ok(Json(TxHistoryResponse {
        user_id,
        entries,
        count,
    }))
}

// ── POST /deposit ─────────────────────────────────────────────────────────────

#[instrument(skip(pool, mh))]
pub async fn deposit(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
    caller: Caller,
    Json(req): Json<DepositRequest>,
) -> Result<Json<TxResponse>, WalletError> {
    if req.amount_cents <= 0 {
        return Err(WalletError::InvalidAmount);
    }

    // Whose balance grows is decided by the authenticated caller, not by the
    // body. A service caller crediting somebody else must hold the shared key.
    let user_id = caller.acting_as(&mh, req.user_id.as_deref()).await?;

    let fee = compute_fee(req.amount_cents, FEE_DEPOSIT_BPS);
    let net = req.amount_cents - fee;
    let tx_id = Uuid::new_v4();

    let mut tx = pool.begin().await?;

    let inserted: Option<(Uuid,)> = sqlx::query_as(
        r#"INSERT INTO transactions (id, idempotency_key, tx_type, to_user_id, amount, fee, net_amount)
           VALUES ($1, $2, 'deposit', $3, $4, $5, $6)
           ON CONFLICT (idempotency_key) DO NOTHING RETURNING id"#,
    )
    .bind(tx_id).bind(&req.idempotency_key)
    .bind(&user_id).bind(req.amount_cents).bind(fee).bind(net)
    .fetch_optional(&mut *tx).await?;

    if inserted.is_none() {
        tx.rollback().await?;
        return existing_tx(&pool, &req.idempotency_key, true).await;
    }

    db::ensure_account(&mut tx, &user_id).await?;
    db::ensure_account(&mut tx, FEE_ACCOUNT).await?;
    db::credit_account(&mut tx, &user_id, net).await?;
    db::credit_account(&mut tx, FEE_ACCOUNT, fee).await?;
    db::append_ledger(&mut tx, tx_id, &user_id, "credit", net).await?;
    if fee > 0 {
        db::append_ledger(&mut tx, tx_id, FEE_ACCOUNT, "credit", fee).await?;
    }

    tx.commit().await?;
    info!(user_id = %user_id, amount = req.amount_cents, fee, net, "AET deposit completed");

    Ok(Json(TxResponse {
        transaction_id: tx_id,
        tx_type: "deposit".into(),
        amount_units: req.amount_cents,
        amount_aet: format_aet(req.amount_cents),
        fee_units: fee,
        net_units: net,
        net_aet: format_aet(net),
        status: "completed".into(),
        created_at: chrono::Utc::now(),
        idempotent_hit: false,
    }))
}

// ── POST /v1/transfer ─────────────────────────────────────────────────────────

#[instrument(skip(pool, mh))]
pub async fn transfer(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
    caller: Caller,
    Json(req): Json<TransferRequest>,
) -> Result<Json<TxResponse>, WalletError> {
    if req.amount_cents <= 0 {
        return Err(WalletError::InvalidAmount);
    }

    // Resolve both sides before comparing them. Two spellings of one person —
    // a handle on one side and its PIAL on the other — are a self-transfer, and
    // comparing the raw strings would not have noticed.
    // The payer is the authenticated caller. The payee is genuinely the
    // caller's to name — naming who you pay is the point; naming who pays is
    // the vulnerability.
    let from_user_id = caller.acting_as(&mh, req.from_user_id.as_deref()).await?;
    let to_user_id = identity::resolve_owner(&mh, &req.to_user_id).await?;
    if from_user_id == to_user_id {
        return Err(WalletError::SelfTransfer);
    }

    let fee = compute_fee(req.amount_cents, FEE_TRANSFER_BPS);
    let net = req.amount_cents - fee;
    let tx_id = Uuid::new_v4();

    let mut tx = pool.begin().await?;

    let inserted: Option<(Uuid,)> = sqlx::query_as(
        r#"INSERT INTO transactions (id, idempotency_key, tx_type, from_user_id, to_user_id, amount, fee, net_amount)
           VALUES ($1, $2, 'transfer', $3, $4, $5, $6, $7)
           ON CONFLICT (idempotency_key) DO NOTHING RETURNING id"#,
    )
    .bind(tx_id).bind(&req.idempotency_key)
    .bind(&from_user_id).bind(&to_user_id)
    .bind(req.amount_cents).bind(fee).bind(net)
    .fetch_optional(&mut *tx).await?;

    if inserted.is_none() {
        tx.rollback().await?;
        return existing_tx(&pool, &req.idempotency_key, true).await;
    }

    db::ensure_account(&mut tx, &from_user_id).await?;
    db::ensure_account(&mut tx, &to_user_id).await?;
    db::ensure_account(&mut tx, FEE_ACCOUNT).await?;

    let sender_balance = db::locked_balance(&mut tx, &from_user_id).await?;
    if sender_balance < req.amount_cents {
        tx.rollback().await?;
        return Err(WalletError::InsufficientBalance);
    }

    db::debit_account(&mut tx, &from_user_id, req.amount_cents).await?;
    db::credit_account(&mut tx, &to_user_id, net).await?;
    db::credit_account(&mut tx, FEE_ACCOUNT, fee).await?;
    db::append_ledger(&mut tx, tx_id, &from_user_id, "debit", req.amount_cents).await?;
    db::append_ledger(&mut tx, tx_id, &to_user_id, "credit", net).await?;
    if fee > 0 {
        db::append_ledger(&mut tx, tx_id, FEE_ACCOUNT, "credit", fee).await?;
    }

    tx.commit().await?;
    info!(from = %from_user_id, to = %to_user_id, amount = req.amount_cents, "AET transfer completed");

    emit_payment_signal_if_needed(&from_user_id, &to_user_id, &req.tx_meta);
    // FA Live: push updated balance to both parties immediately.
    notify_feed_engine_balance_spawn(pool.clone(), from_user_id.clone());
    notify_feed_engine_balance_spawn(pool.clone(), to_user_id.clone());

    Ok(Json(TxResponse {
        transaction_id: tx_id,
        tx_type: "transfer".into(),
        amount_units: req.amount_cents,
        amount_aet: format_aet(req.amount_cents),
        fee_units: fee,
        net_units: net,
        net_aet: format_aet(net),
        status: "completed".into(),
        created_at: chrono::Utc::now(),
        idempotent_hit: false,
    }))
}

// ── POST /v1/tip ──────────────────────────────────────────────────────────────
// Simplified tip endpoint — lower fee, AET units, optional message.
// Can be called from messaging UI (send AET alongside a message).

#[instrument(skip(pool, mh))]
pub async fn tip(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
    caller: Caller,
    Json(req): Json<TipRequest>,
) -> Result<Json<TxResponse>, WalletError> {
    if req.amount_aet <= 0.0 {
        return Err(WalletError::InvalidAmount);
    }

    // The field is named for a PIAL but was never checked to hold one. It is
    // now: whatever the caller sends, what reaches the ledger is a PIAL.
    let from_pial_id = caller.acting_as(&mh, req.from_pial_id.as_deref()).await?;
    let to_pial_id = identity::resolve_owner(&mh, &req.to_pial_id).await?;
    if from_pial_id == to_pial_id {
        return Err(WalletError::SelfTransfer);
    }

    // Convert AET → storage units
    let amount_units = (req.amount_aet * AET_DECIMALS as f64).round() as i64;
    if amount_units <= 0 {
        return Err(WalletError::InvalidAmount);
    }

    let fee = compute_fee(amount_units, FEE_TIP_BPS);
    let net = amount_units - fee;
    let tx_id = Uuid::new_v4();
    let idem = if req.idempotency_key.is_empty() {
        tx_id.to_string()
    } else {
        req.idempotency_key.clone()
    };

    let mut tx = pool.begin().await?;

    let inserted: Option<(Uuid,)> = sqlx::query_as(
        r#"INSERT INTO transactions (id, idempotency_key, tx_type, from_user_id, to_user_id, amount, fee, net_amount)
           VALUES ($1, $2, 'tip', $3, $4, $5, $6, $7)
           ON CONFLICT (idempotency_key) DO NOTHING RETURNING id"#,
    )
    .bind(tx_id).bind(&idem)
    .bind(&from_pial_id).bind(&to_pial_id)
    .bind(amount_units).bind(fee).bind(net)
    .fetch_optional(&mut *tx).await?;

    if inserted.is_none() {
        tx.rollback().await?;
        return existing_tx(&pool, &idem, true).await;
    }

    db::ensure_account(&mut tx, &from_pial_id).await?;
    db::ensure_account(&mut tx, &to_pial_id).await?;
    db::ensure_account(&mut tx, FEE_ACCOUNT).await?;

    let sender_balance = db::locked_balance(&mut tx, &from_pial_id).await?;
    if sender_balance < amount_units {
        tx.rollback().await?;
        return Err(WalletError::InsufficientBalance);
    }

    db::debit_account(&mut tx, &from_pial_id, amount_units).await?;
    db::credit_account(&mut tx, &to_pial_id, net).await?;
    db::credit_account(&mut tx, FEE_ACCOUNT, fee).await?;
    db::append_ledger(&mut tx, tx_id, &from_pial_id, "debit", amount_units).await?;
    db::append_ledger(&mut tx, tx_id, &to_pial_id, "credit", net).await?;
    if fee > 0 {
        db::append_ledger(&mut tx, tx_id, FEE_ACCOUNT, "credit", fee).await?;
    }

    tx.commit().await?;
    info!(from = %from_pial_id, to = %to_pial_id, aet = req.amount_aet, "AET tip sent");

    emit_payment_signal_if_needed(&from_pial_id, &to_pial_id, "tip");
    // FA Live: push updated balance to both parties immediately.
    notify_feed_engine_balance_spawn(pool.clone(), from_pial_id.clone());
    notify_feed_engine_balance_spawn(pool.clone(), to_pial_id.clone());

    Ok(Json(TxResponse {
        transaction_id: tx_id,
        tx_type: "tip".into(),
        amount_units,
        amount_aet: format_aet(amount_units),
        fee_units: fee,
        net_units: net,
        net_aet: format_aet(net),
        status: "completed".into(),
        created_at: chrono::Utc::now(),
        idempotent_hit: false,
    }))
}

// ── POST /withdraw ────────────────────────────────────────────────────────────

#[instrument(skip(pool, mh))]
pub async fn withdraw(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
    caller: Caller,
    Json(req): Json<WithdrawRequest>,
) -> Result<Json<TxResponse>, WalletError> {
    if req.amount_cents <= 0 {
        return Err(WalletError::InvalidAmount);
    }

    // The account that is debited and paid out. This is the route the defect was
    // reported against: it took `req.user_id` and cashed out whoever was named.
    let user_id = caller.acting_as(&mh, req.user_id.as_deref()).await?;

    let fee = compute_fee(req.amount_cents, FEE_WITHDRAWAL_BPS);
    let net = req.amount_cents - fee;
    let tx_id = Uuid::new_v4();

    let mut tx = pool.begin().await?;

    let inserted: Option<(Uuid,)> = sqlx::query_as(
        r#"INSERT INTO transactions (id, idempotency_key, tx_type, from_user_id, amount, fee, net_amount)
           VALUES ($1, $2, 'withdrawal', $3, $4, $5, $6)
           ON CONFLICT (idempotency_key) DO NOTHING RETURNING id"#,
    )
    .bind(tx_id).bind(&req.idempotency_key)
    .bind(&user_id).bind(req.amount_cents).bind(fee).bind(net)
    .fetch_optional(&mut *tx).await?;

    if inserted.is_none() {
        tx.rollback().await?;
        return existing_tx(&pool, &req.idempotency_key, true).await;
    }

    db::ensure_account(&mut tx, &user_id).await?;
    db::ensure_account(&mut tx, FEE_ACCOUNT).await?;

    let balance = db::locked_balance(&mut tx, &user_id).await?;
    if balance < req.amount_cents {
        tx.rollback().await?;
        return Err(WalletError::InsufficientBalance);
    }

    db::debit_account(&mut tx, &user_id, req.amount_cents).await?;
    db::credit_account(&mut tx, FEE_ACCOUNT, fee).await?;
    db::append_ledger(&mut tx, tx_id, &user_id, "debit", req.amount_cents).await?;
    if fee > 0 {
        db::append_ledger(&mut tx, tx_id, FEE_ACCOUNT, "credit", fee).await?;
    }

    tx.commit().await?;
    info!(user_id = %user_id, amount = req.amount_cents, fee, net, "AET withdrawal completed");

    Ok(Json(TxResponse {
        transaction_id: tx_id,
        tx_type: "withdrawal".into(),
        amount_units: req.amount_cents,
        amount_aet: format_aet(req.amount_cents),
        fee_units: fee,
        net_units: net,
        net_aet: format_aet(net),
        status: "completed".into(),
        created_at: chrono::Utc::now(),
        idempotent_hit: false,
    }))
}

// ══ GOVERNANCE — Proposals & Voting ══════════════════════════════════════════

// ── POST /v1/proposals ────────────────────────────────────────────────────────

#[instrument(skip(pool, mh))]
pub async fn create_proposal(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
    caller: Caller,
    Json(req): Json<CreateProposalRequest>,
) -> Result<Json<ProposalSummary>, WalletError> {
    if req.title.trim().is_empty() {
        return Err(WalletError::BadRequest("title is required".into()));
    }
    if req.options.len() < 2 {
        return Err(WalletError::BadRequest(
            "at least 2 options required".into(),
        ));
    }
    if !["token", "identity", "equal"].contains(&req.weight_model.as_str()) {
        return Err(WalletError::BadRequest(
            "weight_model must be token|identity|equal".into(),
        ));
    }
    if req.ends_at <= chrono::Utc::now() {
        return Err(WalletError::BadRequest(
            "ends_at must be in the future".into(),
        ));
    }

    // A proposal is attributed to a person, so its author is resolved exactly
    // like a payer is.
    let creator_id = caller.acting_as(&mh, req.creator_id.as_deref()).await?;

    let id = Uuid::new_v4();
    sqlx::query(
        r#"INSERT INTO proposals (id, title, description, creator_id, weight_model, options, ends_at)
           VALUES ($1, $2, $3, $4, $5, $6, $7)"#,
    )
    .bind(id).bind(&req.title).bind(&req.description)
    .bind(&creator_id).bind(&req.weight_model)
    .bind(&req.options).bind(req.ends_at)
    .execute(&pool).await?;

    info!(id = %id, title = %req.title, creator = %creator_id, "proposal created");

    Ok(Json(ProposalSummary {
        id,
        title: req.title,
        description: req.description,
        creator_id,
        weight_model: req.weight_model,
        options: req.options,
        ends_at: req.ends_at,
        status: "active".into(),
        total_votes: 0,
        created_at: chrono::Utc::now(),
    }))
}

// ── GET /v1/proposals ─────────────────────────────────────────────────────────

pub async fn list_proposals(
    State(pool): State<PgPool>,
) -> Result<Json<Vec<ProposalSummary>>, WalletError> {
    // Auto-close expired proposals
    sqlx::query(
        "UPDATE proposals SET status = 'ended' WHERE status = 'active' AND ends_at <= NOW()",
    )
    .execute(&pool)
    .await?;

    let rows: Vec<ProposalRow> = sqlx::query_as(
        r#"SELECT id, title, description, creator_id, weight_model, options, ends_at, status, created_at
           FROM proposals ORDER BY created_at DESC LIMIT 50"#,
    )
    .fetch_all(&pool).await?;

    let mut summaries = Vec::with_capacity(rows.len());
    for row in rows {
        let total_votes: (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM votes WHERE proposal_id = $1")
                .bind(row.id)
                .fetch_one(&pool)
                .await?;
        summaries.push(ProposalSummary {
            id: row.id,
            title: row.title,
            description: row.description,
            creator_id: row.creator_id,
            weight_model: row.weight_model,
            options: row.options,
            ends_at: row.ends_at,
            status: row.status,
            total_votes: total_votes.0,
            created_at: row.created_at,
        });
    }

    Ok(Json(summaries))
}

// ── GET /v1/proposals/:id ─────────────────────────────────────────────────────

pub async fn get_proposal(
    State(pool): State<PgPool>,
    Path(id): Path<Uuid>,
) -> Result<Json<ProposalDetailResponse>, WalletError> {
    let row: ProposalRow = sqlx::query_as(
        r#"SELECT id, title, description, creator_id, weight_model, options, ends_at, status, created_at
           FROM proposals WHERE id = $1"#,
    )
    .bind(id).fetch_optional(&pool).await?
    .ok_or(WalletError::ProposalNotFound)?;

    let vote_results: Vec<VoteResultRow> = sqlx::query_as(
        r#"SELECT option, COUNT(*) AS vote_count, COALESCE(SUM(weight)::bigint, 0) AS total_weight
           FROM votes WHERE proposal_id = $1 GROUP BY option"#,
    )
    .bind(id)
    .fetch_all(&pool)
    .await?;

    let total_weight: i64 = vote_results.iter().map(|r| r.total_weight).sum();
    let total_votes: i64 = vote_results.iter().map(|r| r.vote_count).sum();

    // Build result per option (including options with 0 votes)
    let results: Vec<OptionResult> = row
        .options
        .iter()
        .map(|opt| {
            let found = vote_results.iter().find(|r| &r.option == opt);
            let weight = found.map(|r| r.total_weight).unwrap_or(0);
            let count = found.map(|r| r.vote_count).unwrap_or(0);
            let pct = if total_weight > 0 {
                weight as f64 / total_weight as f64 * 100.0
            } else {
                0.0
            };
            OptionResult {
                option: opt.clone(),
                vote_count: count,
                total_weight: weight,
                pct,
            }
        })
        .collect();

    let leading = results
        .iter()
        .max_by(|a, b| a.total_weight.cmp(&b.total_weight))
        .filter(|r| r.total_weight > 0)
        .map(|r| r.option.clone());

    let summary = ProposalSummary {
        id: row.id,
        title: row.title,
        description: row.description,
        creator_id: row.creator_id,
        weight_model: row.weight_model,
        options: row.options,
        ends_at: row.ends_at,
        status: row.status,
        total_votes,
        created_at: row.created_at,
    };

    Ok(Json(ProposalDetailResponse {
        proposal: summary,
        results,
        total_weight,
        leading,
    }))
}

// ── POST /v1/proposals/:id/vote ───────────────────────────────────────────────

#[instrument(skip(pool, mh))]
pub async fn cast_vote(
    State(pool): State<PgPool>,
    State(mh): State<Manhattan>,
    caller: Caller,
    Path(proposal_id): Path<Uuid>,
    Json(req): Json<VoteRequest>,
) -> Result<Json<VoteResponse>, WalletError> {
    // One vote per identity is enforced by UNIQUE(proposal_id, voter_id), which
    // only means anything if the voter id is an identity. Two spellings of one
    // person would otherwise be two votes.
    // Token-weighted voting spends the voter's balance as weight, so naming the
    // voter is naming whose stake is cast.
    let voter_id = caller.acting_as(&mh, req.voter_id.as_deref()).await?;

    // Fetch and validate proposal
    let row: ProposalRow = sqlx::query_as(
        "SELECT id, title, description, creator_id, weight_model, options, ends_at, status, created_at
         FROM proposals WHERE id = $1"
    )
    .bind(proposal_id).fetch_optional(&pool).await?
    .ok_or(WalletError::ProposalNotFound)?;

    if row.status != "active" || row.ends_at <= chrono::Utc::now() {
        return Err(WalletError::ProposalNotActive);
    }
    if !row.options.contains(&req.option) {
        return Err(WalletError::InvalidOption);
    }

    // Compute vote weight based on weight model
    let weight: i64 = match row.weight_model.as_str() {
        "token" => {
            // Weight = voter's current AET balance (at time of vote)
            db::get_balance(&pool, &voter_id).await?.unwrap_or(0).max(1)
        }
        "identity" | _ => 1,
    };

    // Insert vote — UNIQUE(proposal_id, voter_id) prevents double-voting
    let result = sqlx::query(
        "INSERT INTO votes (proposal_id, voter_id, option, weight) VALUES ($1, $2, $3, $4)",
    )
    .bind(proposal_id)
    .bind(&voter_id)
    .bind(&req.option)
    .bind(weight)
    .execute(&pool)
    .await;

    match result {
        Ok(_) => {}
        Err(sqlx::Error::Database(e))
            if e.constraint() == Some("votes_proposal_id_voter_id_key") =>
        {
            return Err(WalletError::AlreadyVoted);
        }
        Err(e) => return Err(WalletError::Db(e)),
    }

    info!(
        proposal = %proposal_id, voter = %voter_id,
        option = %req.option, weight, "vote cast"
    );

    Ok(Json(VoteResponse {
        proposal_id,
        voter_id,
        option: req.option,
        weight,
        created_at: chrono::Utc::now(),
    }))
}

// ── POST /v1/ledger/checkpoint ────────────────────────────────────────────────
// Called by Aethyr Ledger after each block is sealed. Stores the Merkle root of
// that block so Ain Soph can cross-verify settlement without querying the ledger brain.

#[instrument(skip(pool))]
pub async fn ledger_checkpoint(
    State(pool): State<PgPool>,
    caller: Caller,
    Json(req): Json<LedgerCheckpointRequest>,
) -> Result<Json<serde_json::Value>, WalletError> {
    // A checkpoint is the platform attesting to its own sealed block. No end
    // user acts here, so an end-user session is not authority for it.
    caller.require_service()?;

    if req.events_root.is_empty() {
        return Err(WalletError::BadRequest("events_root is required".into()));
    }
    if req.tx_count < 0 {
        return Err(WalletError::BadRequest(
            "tx_count must be non-negative".into(),
        ));
    }

    sqlx::query(
        r#"INSERT INTO ledger_checkpoints (block_id, events_root, tx_count, sealed_at)
           VALUES ($1, $2, $3, $4)
           ON CONFLICT (block_id) DO UPDATE
             SET events_root = EXCLUDED.events_root,
                 tx_count    = EXCLUDED.tx_count,
                 sealed_at   = EXCLUDED.sealed_at,
                 received_at = NOW()"#,
    )
    .bind(req.block_id)
    .bind(&req.events_root)
    .bind(req.tx_count)
    .bind(req.sealed_at)
    .execute(&pool)
    .await?;

    info!(
        block_id = req.block_id,
        tx_count = req.tx_count,
        root = %&req.events_root[..16.min(req.events_root.len())],
        "ledger checkpoint stored"
    );

    Ok(Json(serde_json::json!({
        "ok": true,
        "block_id": req.block_id,
        "events_root": req.events_root,
    })))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

async fn existing_tx(pool: &PgPool, key: &str, hit: bool) -> Result<Json<TxResponse>, WalletError> {
    let row: TxRowSimple = sqlx::query_as(
        "SELECT id, tx_type, amount, fee, net_amount, status, created_at
         FROM transactions WHERE idempotency_key = $1",
    )
    .bind(key)
    .fetch_one(pool)
    .await?;

    Ok(Json(TxResponse {
        transaction_id: row.id,
        tx_type: row.tx_type,
        amount_units: row.amount,
        amount_aet: format_aet(row.amount),
        fee_units: row.fee,
        net_units: row.net_amount,
        net_aet: format_aet(row.net_amount),
        status: row.status,
        created_at: row.created_at,
        idempotent_hit: hit,
    }))
}

fn emit_payment_signal_if_needed(from_user: &str, to_user: &str, meta: &str) {
    let (event_type, reward) = match meta.to_lowercase().as_str() {
        "subscription" | "subscribe" => ("subscription", 2.0_f64),
        "tip" => ("tip", 0.8_f64),
        _ => return,
    };
    let from = from_user.to_string();
    let to = to_user.to_string();
    let aethyrrank_url = std::env::var("AETHYRRANK_URL")
        .unwrap_or_else(|_| "http://aethyrrank-engine:8080".to_string());
    tokio::spawn(async move {
        emit_payment_signal(&aethyrrank_url, &from, &to, event_type, reward).await;
    });
}

async fn emit_payment_signal(url: &str, from: &str, creator: &str, event_type: &str, reward: f64) {
    let payload = serde_json::json!({
        "user_id":    from,
        "session_id": uuid::Uuid::new_v4().to_string(),
        "surface":    "wallet",
        "events": [{
            "content_id":          creator,
            "event_type":          event_type,
            "position_at_display": 0,
            "timestamp":           chrono::Utc::now(),
            "exploration_slot":    false,
            "reward_override":     reward,
        }]
    });
    let endpoint = format!("{}/feedback", url);
    if let Ok(client) = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(3))
        .build()
    {
        if let Err(e) = client.post(&endpoint).json(&payload).send().await {
            warn!("AethyrRank payment signal failed: {e}");
        }
    }
}

// ── FA Live: push balance update to feed-engine after any transaction ─────────

/// Fetches the current balance for pial_id and pushes it to feed-engine's
/// balance-update webhook so the user's SSE session receives a live update.
/// Fire-and-forget — spawned as a tokio task, never blocks the response.
pub fn notify_feed_engine_balance_spawn(pool: PgPool, pial_id: String) {
    tokio::spawn(async move {
        notify_feed_engine_balance(&pool, &pial_id).await;
    });
}

async fn notify_feed_engine_balance(pool: &PgPool, pial_id: &str) {
    let balance_units = db::get_balance(pool, pial_id)
        .await
        .ok()
        .flatten()
        .unwrap_or(0);
    let balance_aet = format_aet(balance_units);

    let feed_engine_url =
        std::env::var("FEED_ENGINE_URL").unwrap_or_else(|_| "http://feed-engine:8081".to_string());
    let api_key = std::env::var("INTERNAL_API_KEY").unwrap_or_default();

    let Ok(client) = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(3))
        .build()
    else {
        return;
    };

    let _ = client
        .post(format!("{}/api/internal/balance-update", feed_engine_url))
        .header("X-Internal-Key", api_key)
        .json(&serde_json::json!({ "pial_id": pial_id, "balance_aet": balance_aet }))
        .send()
        .await;
}
