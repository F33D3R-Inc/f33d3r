//! Value the platform moves on purpose, and the one-time subscription import.
//!
//! Every route here is a peer call: it requires `X-Internal-Key` (feed-engine
//! has already checked that the person asking is the founder or an admin) and
//! names that person in `X-Pial-Identity` for the record. Each move is written
//! to `admin_value_moves` before the ledger is asked, and settled after, keyed
//! on an idempotency key the ledger also dedupes on — so a retry is the same
//! move, never a second one.

use std::sync::Arc;

use axum::{extract::State, http::HeaderMap, http::StatusCode, Json};
use serde::Deserialize;
use serde_json::{json, Value};
use uuid::Uuid;

use crate::clients::{format_aet, AinSoph, Charge};
use crate::commerce_events::{self, SubscriptionEvent};
use crate::handlers::{cerr, internal_key_ok, pial_from_headers};
use crate::pial_ref::PialRef;
use crate::AppState;

type Res = Result<Json<Value>, (StatusCode, Json<Value>)>;

const FEE_ACCOUNT: &str = "ain_soph_fees";

/// The person a peer call is made for. Required, so every move has an author.
fn admin_actor(headers: &HeaderMap, state: &AppState) -> Result<PialRef, (StatusCode, Json<Value>)> {
    if !internal_key_ok(headers, &state.cfg.internal_api_key) {
        return Err(cerr(StatusCode::FORBIDDEN, "this route is for the platform only"));
    }
    pial_from_headers(headers).ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "X-Pial-Identity names who is acting"))
}

fn ain(state: &AppState) -> AinSoph<'_> {
    AinSoph { http: &state.http, url: &state.cfg.ain_soph_url, key: &state.cfg.internal_api_key }
}

async fn record_move(
    state: &AppState,
    kind: &str,
    actor: &PialRef,
    target: &str,
    units: i64,
    reason: &str,
    key: &str,
) -> Result<(), (StatusCode, Json<Value>)> {
    sqlx::query(
        "INSERT INTO admin_value_moves (kind, actor_pial, target, amount_units, reason, idempotency_key)
         VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (idempotency_key) DO NOTHING",
    )
    .bind(kind)
    .bind(actor.text())
    .bind(target)
    .bind(units)
    .bind(reason)
    .bind(key)
    .execute(&state.pool)
    .await
    .map(|_| ())
    .map_err(|e| cerr(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))
}

/// Settles a move by what the ledger said, and answers for it.
async fn settle_move(state: &AppState, key: &str, outcome: Charge, units: i64) -> Res {
    let (status, tx_id, why) = match &outcome {
        Charge::Done(tx) => ("completed", Some(tx.clone()), None),
        Charge::Declined(w) => ("failed", None, Some(w.clone())),
        Charge::Unknown(w) => {
            tracing::warn!(key, reason = %w, "admin value move outcome unknown; it stays pending");
            return Err(cerr(
                StatusCode::SERVICE_UNAVAILABLE,
                "the ledger did not answer — retry the same action; it will not be applied twice",
            ));
        }
    };
    sqlx::query(
        "UPDATE admin_value_moves SET status = $2, ain_soph_tx_id = $3, failure_reason = $4,
                amount_units = CASE WHEN $5 > 0 THEN $5 ELSE amount_units END
          WHERE idempotency_key = $1",
    )
    .bind(key)
    .bind(status)
    .bind(&tx_id)
    .bind(&why)
    .bind(units)
    .execute(&state.pool)
    .await
    .map_err(|e| cerr(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;
    match outcome {
        Charge::Done(tx) => {
            let units_per_aet = state.units_per_aet().await.map_err(|e| cerr(StatusCode::SERVICE_UNAVAILABLE, &e.to_string()))?;
            Ok(Json(json!({
                "ok": true,
                "ain_soph_tx_id": tx,
                "amount_units": units,
                "amount_display": format_aet(units, units_per_aet),
            })))
        }
        Charge::Declined(w) => Err(cerr(StatusCode::UNPROCESSABLE_ENTITY, &w)),
        Charge::Unknown(_) => unreachable!("answered above"),
    }
}

#[derive(Deserialize)]
pub struct CreditReq {
    pub target_pial: PialRef,
    pub amount_units: i64,
    #[serde(default)]
    pub reason: String,
    pub idempotency_key: String,
}

/// POST /v1/admin/aet/credit — mints platform credit for one identity.
pub async fn credit(State(state): State<Arc<AppState>>, headers: HeaderMap, Json(req): Json<CreditReq>) -> Res {
    let actor = admin_actor(&headers, &state)?;
    if req.amount_units <= 0 {
        return Err(cerr(StatusCode::BAD_REQUEST, "amount must be greater than zero"));
    }
    if req.idempotency_key.trim().is_empty() {
        return Err(cerr(StatusCode::BAD_REQUEST, "idempotency_key is required"));
    }
    let key = format!("themis:admin:credit:{}", req.idempotency_key.trim());
    let target = req.target_pial.text();
    record_move(&state, "credit", &actor, &target, req.amount_units, req.reason.trim(), &key).await?;
    let outcome = ain(&state).deposit(&target, req.amount_units, &key).await;
    tracing::info!(actor = %actor, units = req.amount_units, "admin AET credit requested");
    settle_move(&state, &key, outcome, req.amount_units).await
}

#[derive(Deserialize)]
pub struct SweepReq {
    pub to_pial: PialRef,
    pub idempotency_key: String,
}

/// POST /v1/admin/fees/sweep — moves the whole fee float to one identity.
pub async fn sweep_fees(State(state): State<Arc<AppState>>, headers: HeaderMap, Json(req): Json<SweepReq>) -> Res {
    let actor = admin_actor(&headers, &state)?;
    if req.idempotency_key.trim().is_empty() {
        return Err(cerr(StatusCode::BAD_REQUEST, "idempotency_key is required"));
    }
    let key = format!("themis:admin:sweep:{}", req.idempotency_key.trim());
    // A retried sweep re-sends the amount it recorded the first time, so the
    // ledger sees the same request and dedupes it.
    let recorded: Option<i64> =
        sqlx::query_scalar("SELECT amount_units FROM admin_value_moves WHERE idempotency_key = $1")
            .bind(&key)
            .fetch_optional(&state.pool)
            .await
            .map_err(|e| cerr(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;
    let units = match recorded {
        Some(u) => u,
        None => ain(&state)
            .balance(FEE_ACCOUNT)
            .await
            .map_err(|e| cerr(StatusCode::SERVICE_UNAVAILABLE, &e.to_string()))?,
    };
    if units <= 0 {
        return Ok(Json(json!({ "ok": true, "amount_units": 0, "amount_display": "" })));
    }
    let to = req.to_pial.text();
    record_move(&state, "fee_sweep", &actor, &to, units, "fee float sweep", &key).await?;
    let outcome = ain(&state).transfer(FEE_ACCOUNT, &to, units, &key, "fee_sweep").await;
    settle_move(&state, &key, outcome, units).await
}

#[derive(Deserialize)]
pub struct ReattributeReq {
    pub shadow_account: Uuid,
    pub owner_pial: PialRef,
}

/// POST /v1/admin/accounts/reattribute — returns a shadow balance to its owner.
pub async fn reattribute(State(state): State<Arc<AppState>>, headers: HeaderMap, Json(req): Json<ReattributeReq>) -> Res {
    let actor = admin_actor(&headers, &state)?;
    let shadow = req.shadow_account.hyphenated().to_string();
    let key = format!("themis:admin:reattribute:{shadow}");
    let owner = req.owner_pial.text();
    record_move(&state, "reattribution", &actor, &owner, 0, &format!("shadow account {shadow}"), &key).await?;
    let (outcome, units) = ain(&state).reattribute(&shadow, &owner, &key).await;
    settle_move(&state, &key, outcome, units).await
}

// ── The one-time import of feed-engine's subscriptions ──────────────────────

#[derive(Deserialize)]
pub struct ImportPlan {
    pub id: Uuid,
    pub creator_account: Uuid,
    pub creator_pial: PialRef,
    pub name: String,
    #[serde(default)]
    pub description: String,
    pub price_units: i64,
    pub is_active: bool,
    pub created_at: chrono::DateTime<chrono::Utc>,
}

#[derive(Deserialize)]
pub struct ImportSubscription {
    pub id: Uuid,
    pub subscriber_account: Uuid,
    pub subscriber_pial: PialRef,
    pub creator_account: Uuid,
    pub creator_pial: PialRef,
    pub plan_id: Option<Uuid>,
    /// active | cancelled | expired
    pub status: String,
    pub period_end: chrono::DateTime<chrono::Utc>,
    pub cancelled_at: Option<chrono::DateTime<chrono::Utc>>,
    pub created_at: chrono::DateTime<chrono::Utc>,
}

#[derive(Deserialize)]
pub struct ImportReq {
    #[serde(default)]
    pub plans: Vec<ImportPlan>,
    #[serde(default)]
    pub subscriptions: Vec<ImportSubscription>,
}

/// POST /v1/admin/import/subscriptions — takes feed-engine's plans and
/// subscriptions into Themis, keeping their ids, so Themis holds the truth and
/// its events re-project the same rows. Idempotent: a row already here is left
/// as it is. Answers how many of the ids sent are now present, which is what
/// the importer verifies.
pub async fn import(State(state): State<Arc<AppState>>, headers: HeaderMap, Json(req): Json<ImportReq>) -> Res {
    let actor = admin_actor(&headers, &state)?;
    let db = |e: sqlx::Error| cerr(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string());
    let mut tx = state.pool.begin().await.map_err(db)?;
    let mut plans_inserted = 0u64;
    for p in &req.plans {
        let n = sqlx::query(
            "INSERT INTO subscription_plans
               (id, creator_pial_id, creator_account, name, price_aet, description, is_active, created_at, imported_from)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'feed-engine')
             ON CONFLICT (id) DO NOTHING",
        )
        .bind(p.id)
        .bind(p.creator_pial.text())
        .bind(p.creator_account)
        .bind(&p.name)
        .bind(p.price_units)
        .bind(&p.description)
        .bind(p.is_active && p.price_units > 0)
        .bind(p.created_at)
        .execute(&mut *tx)
        .await
        .map_err(db)?
        .rows_affected();
        if n > 0 {
            plans_inserted += 1;
            commerce_events::plan(&mut tx, p.id).await.map_err(db)?;
        }
    }
    let mut subs_inserted = 0u64;
    for s in &req.subscriptions {
        let status = match s.status.as_str() {
            "active" if s.period_end > chrono::Utc::now() => "active",
            "active" | "expired" => "expired",
            _ => "cancelled",
        };
        let n = sqlx::query(
            "INSERT INTO subscriptions
               (id, subscriber_pial_id, creator_pial_id, subscriber_account, creator_account, plan_id,
                status, current_period_start, current_period_end, cancelled_at, created_at, imported_from)
             VALUES ($1, $2, $3, $4, $5,
                     (SELECT id FROM subscription_plans WHERE id = $6),
                     $7, $10, $8, $9, $10, 'feed-engine')
             ON CONFLICT (id) DO NOTHING",
        )
        .bind(s.id)
        .bind(s.subscriber_pial.text())
        .bind(s.creator_pial.text())
        .bind(s.subscriber_account)
        .bind(s.creator_account)
        .bind(s.plan_id)
        .bind(status)
        .bind(s.period_end)
        .bind(s.cancelled_at)
        .bind(s.created_at)
        .execute(&mut *tx)
        .await
        .map_err(db)?
        .rows_affected();
        if n > 0 {
            subs_inserted += 1;
            let kind = if status == "active" { SubscriptionEvent::Created } else { SubscriptionEvent::Expired };
            commerce_events::subscription(&mut tx, kind, s.id).await.map_err(db)?;
        }
    }
    tx.commit().await.map_err(db)?;

    let plan_ids: Vec<Uuid> = req.plans.iter().map(|p| p.id).collect();
    let sub_ids: Vec<Uuid> = req.subscriptions.iter().map(|s| s.id).collect();
    let plans_present: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM subscription_plans WHERE id = ANY($1)")
        .bind(&plan_ids)
        .fetch_one(&state.pool)
        .await
        .map_err(db)?;
    let subs_present: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM subscriptions WHERE id = ANY($1)")
        .bind(&sub_ids)
        .fetch_one(&state.pool)
        .await
        .map_err(db)?;
    tracing::info!(actor = %actor, plans_inserted, subs_inserted, "feed-engine subscriptions imported");
    Ok(Json(json!({
        "plans_sent": plan_ids.len(),
        "plans_inserted": plans_inserted,
        "plans_present": plans_present,
        "subscriptions_sent": sub_ids.len(),
        "subscriptions_inserted": subs_inserted,
        "subscriptions_present": subs_present,
    })))
}
