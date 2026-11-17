//! Commerce: plans, subscriptions, pay-per-view, tips.
//!
//! Themis is the only door through which AET moves, and the only writer of
//! plans and subscriptions. Every amount is an integer of AET units (Ain Soph's
//! unit). Every charge is recorded before the money moves and settled after —
//! see charges.rs — and every change feed-engine projects is queued on
//! commerce.events in the same transaction (commerce_events.rs).
//!
//! The person acting is the X-Pial-Identity feed-engine sets from the session.
//! A request body never names the payer, the subscriber or the plan's owner.

use std::sync::Arc;

use axum::{
    extract::{Path, Query, State},
    http::{HeaderMap, StatusCode},
    Json,
};
use manhattan_client::KIND_WORK;
use serde_json::{json, Value};
use uuid::Uuid;

use crate::charges::{self, Settled};
use crate::clients::{self, format_aet};
use crate::commerce_events::{self, SubscriptionEvent};
use crate::handlers::{cerr, identity_refused, internal_key_ok, pial_from_headers, sale_notice};
use crate::models::*;
use crate::pial_ref::{register_content, require_identity, ContentRef, PialRef};
use crate::AppState;

type CommerceRes<T> = Result<Json<T>, (StatusCode, Json<Value>)>;

fn cdberr(e: sqlx::Error) -> (StatusCode, Json<Value>) {
    tracing::error!("commerce db error: {e}");
    cerr(StatusCode::INTERNAL_SERVER_ERROR, "database error")
}

/// The person acting: the session identity feed-engine names. Required.
fn acting_identity(headers: &HeaderMap) -> Result<PialRef, (StatusCode, Json<Value>)> {
    pial_from_headers(headers).ok_or_else(|| {
        cerr(StatusCode::UNAUTHORIZED, "X-Pial-Identity is required: this endpoint acts for an identity")
    })
}

/// Establishes that the caller may act as `acting_as`: it IS that identity, or
/// it is a peer holding `X-Internal-Key` (a service reading for a person who is
/// not making the call). Deny is the default: no header, no action.
fn require_caller(
    headers: &HeaderMap,
    cfg_key: &str,
    acting_as: &PialRef,
) -> Result<(), (StatusCode, Json<Value>)> {
    if internal_key_ok(headers, cfg_key) {
        return Ok(());
    }
    match pial_from_headers(headers) {
        None => Err(cerr(
            StatusCode::UNAUTHORIZED,
            "X-Pial-Identity is required: this endpoint acts for an identity",
        )),
        Some(caller) if caller == *acting_as => Ok(()),
        Some(caller) => {
            tracing::warn!(
                caller = %caller.text(),
                claimed = %acting_as.text(),
                "refused: caller tried to act as a different identity"
            );
            Err(cerr(StatusCode::FORBIDDEN, "you may only act as your own identity"))
        }
    }
}

async fn units_or_unavailable(state: &AppState) -> Result<i64, (StatusCode, Json<Value>)> {
    state.units_per_aet().await.map_err(|e| {
        tracing::error!("AET unit unavailable from ain-soph: {e}");
        cerr(StatusCode::SERVICE_UNAVAILABLE, "The wallet is unavailable right now. Nothing was charged.")
    })
}

/// The answer to a charge, as the HTTP caller sees it.
fn charge_answer(settled: Settled) -> Result<String, (StatusCode, Json<Value>)> {
    match settled {
        Settled::Completed(tx) => Ok(tx),
        Settled::Failed(why) => Err(cerr(StatusCode::PAYMENT_REQUIRED, &why)),
        Settled::Pending(why) => {
            tracing::warn!(reason = %why, "charge answered as still settling");
            Err(cerr(
            StatusCode::ACCEPTED,
            "The payment is still settling. It will complete or be released shortly; do not pay again.",
            ))
        }
    }
}

fn charge_unrecorded(what: &str, id: Uuid, e: anyhow::Error) -> (StatusCode, Json<Value>) {
    tracing::error!(%id, "INCIDENT: {what} charge did not settle and stays pending for the reconciler: {e}");
    cerr(StatusCode::INTERNAL_SERVER_ERROR, "the payment could not be recorded; it has been logged — do not pay again")
}

pub async fn commerce_stats(State(state): State<Arc<AppState>>) -> CommerceRes<Value> {
    let pool = &state.pool;
    let creators: i64 =
        sqlx::query_scalar("SELECT COUNT(*) FROM creator_eligibility WHERE monetization_enabled")
            .fetch_one(pool)
            .await
            .map_err(cdberr)?;
    let active_subs: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM subscriptions WHERE status = 'active' AND current_period_end > NOW()",
    )
    .fetch_one(pool)
    .await
    .map_err(cdberr)?;
    let tx_count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM commerce_transactions")
        .fetch_one(pool)
        .await
        .map_err(cdberr)?;
    let volume: i64 = sqlx::query_scalar(
        "SELECT COALESCE(SUM(gross_aet), 0)::bigint FROM commerce_transactions WHERE status = 'completed'",
    )
    .fetch_one(pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({
        "active_creators": creators,
        "active_subscriptions": active_subs,
        "total_transactions": tx_count,
        "total_volume_units": volume,
    })))
}

pub async fn creator_enable(State(state): State<Arc<AppState>>, headers: HeaderMap) -> CommerceRes<Value> {
    let pial = acting_identity(&headers)?;
    // Enabling monetization is the moment a PIAL becomes payable by this brain.
    require_identity(&state.manhattan, &pial)
        .await
        .map_err(|e| identity_refused(&pial, e))?;
    // Monetization is for a verified identity. The level is elohim-veni's
    // fact, read now; this brain does not keep a copy of it.
    let tier = clients::identity_kyc_tier(
        &state.http,
        &state.cfg.elohim_veni_url,
        &state.cfg.internal_api_key,
        &pial.text(),
    )
    .await
    .map_err(|e| {
        tracing::warn!(pial = %pial, error = %e, "creator enable: identity authority could not be asked");
        cerr(StatusCode::SERVICE_UNAVAILABLE, "the identity authority could not be asked; nothing was changed")
    })?;
    if tier != "full" {
        return Err(cerr(
            StatusCode::FORBIDDEN,
            "monetization requires a verified identity (a document, a face match and a live check)",
        ));
    }
    sqlx::query(
        "INSERT INTO creator_eligibility (pial_id, monetization_enabled, enabled_at) \
         VALUES ($1, true, NOW()) \
         ON CONFLICT (pial_id) DO UPDATE \
         SET monetization_enabled = true, enabled_at = NOW(), \
             disabled_at = NULL, disabled_reason = NULL",
    )
    .bind(pial.text())
    .execute(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"monetization_enabled": true})))
}

pub async fn creator_status(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Value> {
    let enabled: Option<bool> =
        sqlx::query_scalar("SELECT monetization_enabled FROM creator_eligibility WHERE pial_id = $1")
            .bind(pial_id.text())
            .fetch_optional(&state.pool)
            .await
            .map_err(cdberr)?;
    Ok(Json(json!({"monetization_enabled": enabled.unwrap_or(false)})))
}

pub async fn list_plans(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Vec<PlanRow>> {
    let rows = sqlx::query_as::<_, PlanRow>(
        "SELECT id, creator_pial_id, name, price_aet, description, is_active, created_at \
         FROM subscription_plans WHERE creator_pial_id = $1 ORDER BY price_aet ASC",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

async fn require_monetized(state: &AppState, pial: &PialRef) -> Result<(), (StatusCode, Json<Value>)> {
    let enabled: Option<bool> =
        sqlx::query_scalar("SELECT monetization_enabled FROM creator_eligibility WHERE pial_id = $1")
            .bind(pial.text())
            .fetch_optional(&state.pool)
            .await
            .map_err(cdberr)?;
    if enabled != Some(true) {
        return Err(cerr(StatusCode::FORBIDDEN, "creator monetization not enabled"));
    }
    Ok(())
}

pub async fn create_plan(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<CreatePlanReq>,
) -> CommerceRes<PlanRow> {
    let creator = acting_identity(&headers)?;
    require_identity(&state.manhattan, &creator)
        .await
        .map_err(|e| identity_refused(&creator, e))?;
    require_monetized(&state, &creator).await?;
    if req.price_units <= 0 {
        return Err(cerr(StatusCode::BAD_REQUEST, "price must be greater than zero"));
    }
    if req.name.trim().is_empty() {
        return Err(cerr(StatusCode::BAD_REQUEST, "plan name is required"));
    }
    let mut tx = state.pool.begin().await.map_err(cdberr)?;
    let row = sqlx::query_as::<_, PlanRow>(
        "INSERT INTO subscription_plans (creator_pial_id, creator_account, name, price_aet, description) \
         VALUES ($1, $2, $3, $4, $5) \
         RETURNING id, creator_pial_id, name, price_aet, description, is_active, created_at",
    )
    .bind(creator.text())
    .bind(req.creator_account)
    .bind(req.name.trim())
    .bind(req.price_units)
    .bind(req.description.trim())
    .fetch_one(&mut *tx)
    .await
    .map_err(cdberr)?;
    commerce_events::plan(&mut tx, row.id).await.map_err(cdberr)?;
    tx.commit().await.map_err(cdberr)?;
    Ok(Json(row))
}

/// The plan's owner, so only its creator may change it.
async fn plan_owner(state: &AppState, id: Uuid) -> Result<PialRef, (StatusCode, Json<Value>)> {
    let owner: Option<PialRef> =
        sqlx::query_scalar("SELECT creator_pial_id FROM subscription_plans WHERE id = $1")
            .bind(id)
            .fetch_optional(&state.pool)
            .await
            .map_err(cdberr)?;
    owner.ok_or_else(|| cerr(StatusCode::NOT_FOUND, "plan not found"))
}

pub async fn update_plan(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(id): Path<Uuid>,
    Json(req): Json<UpdatePlanReq>,
) -> CommerceRes<PlanRow> {
    let caller = acting_identity(&headers)?;
    if plan_owner(&state, id).await? != caller {
        return Err(cerr(StatusCode::FORBIDDEN, "only the plan's creator may change it"));
    }
    if matches!(req.price_units, Some(p) if p <= 0) {
        return Err(cerr(StatusCode::BAD_REQUEST, "price must be greater than zero"));
    }
    let mut tx = state.pool.begin().await.map_err(cdberr)?;
    let row = sqlx::query_as::<_, PlanRow>(
        "UPDATE subscription_plans SET \
           name        = COALESCE($2, name), \
           price_aet   = COALESCE($3, price_aet), \
           description = COALESCE($4, description), \
           is_active   = COALESCE($5, is_active) \
         WHERE id = $1 \
         RETURNING id, creator_pial_id, name, price_aet, description, is_active, created_at",
    )
    .bind(id)
    .bind(req.name.as_deref().map(str::trim).filter(|n| !n.is_empty()))
    .bind(req.price_units)
    .bind(req.description.as_deref().map(str::trim))
    .bind(req.is_active)
    .fetch_one(&mut *tx)
    .await
    .map_err(cdberr)?;
    commerce_events::plan(&mut tx, id).await.map_err(cdberr)?;
    tx.commit().await.map_err(cdberr)?;
    Ok(Json(row))
}

pub async fn delete_plan(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(id): Path<Uuid>,
) -> CommerceRes<Value> {
    let caller = acting_identity(&headers)?;
    if plan_owner(&state, id).await? != caller {
        return Err(cerr(StatusCode::FORBIDDEN, "only the plan's creator may retire it"));
    }
    // A retired plan takes no new subscribers; its subscribers run to the end
    // of the period they paid for and are not renewed (charges::renew).
    let mut tx = state.pool.begin().await.map_err(cdberr)?;
    sqlx::query("UPDATE subscription_plans SET is_active = false WHERE id = $1")
        .bind(id)
        .execute(&mut *tx)
        .await
        .map_err(cdberr)?;
    commerce_events::plan(&mut tx, id).await.map_err(cdberr)?;
    tx.commit().await.map_err(cdberr)?;
    Ok(Json(json!({"id": id, "is_active": false})))
}

/// What a subscription answer says about the subscription now.
async fn subscription_view(state: &AppState, id: Uuid) -> CommerceRes<Value> {
    let row: (String, chrono::DateTime<chrono::Utc>, Option<chrono::DateTime<chrono::Utc>>, Option<Uuid>) =
        sqlx::query_as(
            "SELECT status, current_period_end, cancelled_at, plan_id FROM subscriptions WHERE id = $1",
        )
        .bind(id)
        .fetch_one(&state.pool)
        .await
        .map_err(cdberr)?;
    Ok(Json(json!({
        "subscription_id": id,
        "status": row.0,
        "period_end": row.1,
        "cancelled_at": row.2,
        "plan_id": row.3,
    })))
}

pub async fn subscribe(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<SubscribeReq>,
) -> CommerceRes<Value> {
    let subscriber = acting_identity(&headers)?;
    // The price, the payee and the payee's account come from the plan. Nothing
    // the request carries can change what is charged or who is paid.
    let plan: Option<(PialRef, Option<Uuid>, i64, String)> = sqlx::query_as(
        "SELECT creator_pial_id, creator_account, price_aet, name FROM subscription_plans \
          WHERE id = $1 AND is_active AND price_aet > 0",
    )
    .bind(req.plan_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    let (creator, creator_account, price_units, plan_name) =
        plan.ok_or_else(|| cerr(StatusCode::NOT_FOUND, "plan not found or retired"))?;
    if creator == subscriber {
        return Err(cerr(StatusCode::BAD_REQUEST, "you cannot subscribe to yourself"));
    }
    require_identity(&state.manhattan, &subscriber)
        .await
        .map_err(|e| identity_refused(&subscriber, e))?;
    require_identity(&state.manhattan, &creator)
        .await
        .map_err(|e| identity_refused(&creator, e))?;
    let units_per_aet = units_or_unavailable(&state).await?;

    // An active subscription that was cancelled but has not ended is resumed:
    // it is already paid to the end of its period, so nothing is charged.
    let existing: Option<(Uuid, String, bool)> = sqlx::query_as(
        "SELECT id, status, cancelled_at IS NOT NULL FROM subscriptions \
          WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 \
            AND (status = 'pending' OR (status = 'active' AND current_period_end > NOW()))",
    )
    .bind(subscriber.text())
    .bind(creator.text())
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    match existing {
        Some((id, status, true)) if status == "active" => {
            let mut tx = state.pool.begin().await.map_err(cdberr)?;
            sqlx::query("UPDATE subscriptions SET cancelled_at = NULL, plan_id = $2 WHERE id = $1")
                .bind(id)
                .bind(req.plan_id)
                .execute(&mut *tx)
                .await
                .map_err(cdberr)?;
            commerce_events::subscription(&mut tx, SubscriptionEvent::Resumed, id)
                .await
                .map_err(cdberr)?;
            tx.commit().await.map_err(cdberr)?;
            return subscription_view(&state, id).await;
        }
        Some((_, status, _)) if status == "pending" => {
            return Err(cerr(StatusCode::CONFLICT, "a subscription payment is already settling"));
        }
        Some(_) => return Err(cerr(StatusCode::CONFLICT, "already subscribed")),
        None => {}
    }

    let sub_id = Uuid::new_v4();
    let period_end = chrono::Utc::now() + chrono::Duration::days(charges::PERIOD_DAYS);
    let notice = sale_notice(
        format!("themis:subscribe:{sub_id}"),
        creator.text(),
        "subscribe",
        "New subscriber",
        sub_id.to_string(),
        format!("{} · {plan_name}", format_aet(price_units, units_per_aet)),
    );
    let mut tx = state.pool.begin().await.map_err(cdberr)?;
    sqlx::query(
        "INSERT INTO subscriptions \
           (id, subscriber_pial_id, creator_pial_id, subscriber_account, creator_account, plan_id, \
            current_period_end, period_index, status) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, 0, 'pending')",
    )
    .bind(sub_id)
    .bind(subscriber.text())
    .bind(creator.text())
    .bind(req.subscriber_account)
    .bind(creator_account)
    .bind(req.plan_id)
    .bind(period_end)
    .execute(&mut *tx)
    .await
    .map_err(cdberr)?;
    let charge_id = charges::record(
        &mut tx,
        "subscription",
        sub_id,
        &subscriber.text(),
        &creator.text(),
        price_units,
        &charges::period_key(sub_id, 0),
        Some(0),
        Some(&notice),
    )
    .await
    .map_err(cdberr)?;
    tx.commit().await.map_err(cdberr)?;

    let settled = charges::run(&state, charge_id)
        .await
        .map_err(|e| charge_unrecorded("subscription", sub_id, e))?;
    charge_answer(settled)?;
    subscription_view(&state, sub_id).await
}

pub async fn unsubscribe(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<UnsubscribeReq>,
) -> CommerceRes<Value> {
    let subscriber = acting_identity(&headers)?;
    // A cancelled subscription runs to the end of the period it paid for and is
    // then not renewed. A pending one is a payment in flight and cannot be
    // cancelled under it; the caller is told so and retries a moment later.
    let mut tx = state.pool.begin().await.map_err(cdberr)?;
    let id: Option<Uuid> = sqlx::query_scalar(
        "UPDATE subscriptions SET cancelled_at = NOW() \
          WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 \
            AND status = 'active' AND cancelled_at IS NULL AND current_period_end > NOW() \
          RETURNING id",
    )
    .bind(subscriber.text())
    .bind(req.creator_pial_id.text())
    .fetch_optional(&mut *tx)
    .await
    .map_err(cdberr)?;
    let Some(id) = id else {
        tx.rollback().await.map_err(cdberr)?;
        let pending: Option<Uuid> = sqlx::query_scalar(
            "SELECT id FROM subscriptions \
             WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 AND status = 'pending'",
        )
        .bind(subscriber.text())
        .bind(req.creator_pial_id.text())
        .fetch_optional(&state.pool)
        .await
        .map_err(cdberr)?;
        if pending.is_some() {
            return Err(cerr(
                StatusCode::CONFLICT,
                "this subscription's payment is still settling — try again in a moment",
            ));
        }
        return Err(cerr(StatusCode::NOT_FOUND, "no active subscription found"));
    };
    commerce_events::subscription(&mut tx, SubscriptionEvent::Cancelled, id)
        .await
        .map_err(cdberr)?;
    tx.commit().await.map_err(cdberr)?;
    subscription_view(&state, id).await
}

pub async fn check_subscription(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Query(q): Query<CommerceAccessQuery>,
) -> CommerceRes<Value> {
    let subscriber = q
        .subscriber
        .ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "subscriber required"))?;
    require_caller(&headers, &state.cfg.internal_api_key, &subscriber)?;
    let creator = q
        .creator
        .ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "creator required"))?;
    let has: Option<Uuid> = sqlx::query_scalar(
        "SELECT id FROM subscriptions \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 \
           AND status = 'active' AND current_period_end > NOW()",
    )
    .bind(subscriber.text())
    .bind(creator.text())
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"has_access": has.is_some()})))
}

pub async fn list_subscribers(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Vec<SubscriptionRow>> {
    require_caller(&headers, &state.cfg.internal_api_key, &pial_id)?;
    let rows = sqlx::query_as::<_, SubscriptionRow>(
        "SELECT id, subscriber_pial_id, creator_pial_id, plan_id, status, \
                current_period_start, current_period_end, cancelled_at, created_at \
         FROM subscriptions \
          WHERE creator_pial_id = $1 AND status <> 'failed' \
          ORDER BY created_at DESC",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn list_subscriptions(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Vec<SubscriptionRow>> {
    require_caller(&headers, &state.cfg.internal_api_key, &pial_id)?;
    let rows = sqlx::query_as::<_, SubscriptionRow>(
        "SELECT id, subscriber_pial_id, creator_pial_id, plan_id, status, \
                current_period_start, current_period_end, cancelled_at, created_at \
         FROM subscriptions \
          WHERE subscriber_pial_id = $1 AND status <> 'failed' \
          ORDER BY created_at DESC",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn create_ppv(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<CreatePpvReq>,
) -> CommerceRes<PpvItemRow> {
    let creator = acting_identity(&headers)?;
    require_identity(&state.manhattan, &creator)
        .await
        .map_err(|e| identity_refused(&creator, e))?;
    // The item being sold is a work owned by feed-engine, referenced here by
    // name. Themis registers the name it must reference and resolves it; it
    // never assumes another brain's id scheme.
    if let Some(content) = ContentRef::parse(&req.content_id) {
        register_content(&state.manhattan, KIND_WORK, &content).await;
    }
    require_monetized(&state, &creator).await?;
    if req.price_units <= 0 {
        return Err(cerr(StatusCode::BAD_REQUEST, "price must be greater than zero"));
    }
    let row = sqlx::query_as::<_, PpvItemRow>(
        "INSERT INTO ppv_items (creator_pial_id, content_id, price_aet, title) \
         VALUES ($1, $2, $3, $4) \
         RETURNING id, creator_pial_id, content_id, price_aet, title, is_active, created_at",
    )
    .bind(creator.text())
    .bind(&req.content_id)
    .bind(req.price_units)
    .bind(&req.title)
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(row))
}

pub async fn purchase_ppv(
    State(state): State<Arc<AppState>>,
    Path(item_id): Path<Uuid>,
    headers: HeaderMap,
) -> CommerceRes<Value> {
    let buyer = acting_identity(&headers)?;
    require_identity(&state.manhattan, &buyer)
        .await
        .map_err(|e| identity_refused(&buyer, e))?;
    let item: Option<(PialRef, i64, String)> = sqlx::query_as(
        "SELECT creator_pial_id, price_aet, title FROM ppv_items WHERE id = $1 AND is_active AND price_aet > 0",
    )
    .bind(item_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    let (creator, price_units, title) =
        item.ok_or_else(|| cerr(StatusCode::NOT_FOUND, "PPV item not found"))?;
    if creator == buyer {
        return Err(cerr(StatusCode::BAD_REQUEST, "you cannot buy your own item"));
    }
    require_identity(&state.manhattan, &creator)
        .await
        .map_err(|e| identity_refused(&creator, e))?;
    let units_per_aet = units_or_unavailable(&state).await?;
    // A purchase already paid for, or one whose payment is in flight, blocks a
    // second charge. A FAILED one must not. This mirrors uq_ppv_purchase_live.
    let existing: Option<Uuid> = sqlx::query_scalar(
        "SELECT id FROM ppv_purchases \
          WHERE buyer_pial_id = $1 AND ppv_item_id = $2 AND status <> 'failed'",
    )
    .bind(buyer.text())
    .bind(item_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    if existing.is_some() {
        return Err(cerr(StatusCode::CONFLICT, "already purchased"));
    }
    let purchase_id = Uuid::new_v4();
    let notice = sale_notice(
        format!("themis:ppv:{purchase_id}"),
        creator.text(),
        "ppv_sale",
        "Pay-per-view sale",
        purchase_id.to_string(),
        format!("{} · {title}", format_aet(price_units, units_per_aet)),
    );
    let mut tx = state.pool.begin().await.map_err(cdberr)?;
    sqlx::query(
        "INSERT INTO ppv_purchases (id, buyer_pial_id, ppv_item_id, amount_aet, status) \
         VALUES ($1, $2, $3, $4, 'pending')",
    )
    .bind(purchase_id)
    .bind(buyer.text())
    .bind(item_id)
    .bind(price_units)
    .execute(&mut *tx)
    .await
    .map_err(cdberr)?;
    let charge_id = charges::record(
        &mut tx,
        "ppv",
        purchase_id,
        &buyer.text(),
        &creator.text(),
        price_units,
        &format!("themis:ppv:{purchase_id}"),
        None,
        Some(&notice),
    )
    .await
    .map_err(cdberr)?;
    tx.commit().await.map_err(cdberr)?;
    let settled = charges::run(&state, charge_id)
        .await
        .map_err(|e| charge_unrecorded("pay-per-view", purchase_id, e))?;
    let tx_id = charge_answer(settled)?;
    Ok(Json(json!({
        "purchase_id": purchase_id,
        "item_id": item_id,
        "amount_units": price_units,
        "ain_soph_tx_id": tx_id,
        "has_access": true,
    })))
}

pub async fn check_ppv_access(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Query(q): Query<CommerceAccessQuery>,
) -> CommerceRes<Value> {
    let buyer = q
        .buyer
        .ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "buyer required"))?;
    require_caller(&headers, &state.cfg.internal_api_key, &buyer)?;
    let content_id = q
        .content_id
        .ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "content_id required"))?;
    // Only a completed purchase unlocks content: the row is written before the
    // money moves, so a pending or failed one must not.
    let has: Option<Uuid> = sqlx::query_scalar(
        "SELECT p.id FROM ppv_purchases p \
         JOIN ppv_items i ON i.id = p.ppv_item_id \
         WHERE p.buyer_pial_id = $1 AND i.content_id = $2 \
           AND p.status = 'completed'",
    )
    .bind(buyer.text())
    .bind(&content_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"has_access": has.is_some()})))
}

pub async fn list_ppv_items(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Vec<PpvItemRow>> {
    let rows = sqlx::query_as::<_, PpvItemRow>(
        "SELECT id, creator_pial_id, content_id, price_aet, title, is_active, created_at \
         FROM ppv_items WHERE creator_pial_id = $1 ORDER BY created_at DESC",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn send_tip(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<SendTipReq>,
) -> CommerceRes<Value> {
    let sender = acting_identity(&headers)?;
    if req.amount_units <= 0 {
        return Err(cerr(StatusCode::BAD_REQUEST, "amount must be greater than zero"));
    }
    if req.idempotency_key.trim().is_empty() {
        return Err(cerr(StatusCode::BAD_REQUEST, "idempotency_key is required"));
    }
    if sender == req.recipient_pial_id {
        return Err(cerr(StatusCode::BAD_REQUEST, "cannot tip yourself"));
    }
    require_identity(&state.manhattan, &sender)
        .await
        .map_err(|e| identity_refused(&sender, e))?;
    require_identity(&state.manhattan, &req.recipient_pial_id)
        .await
        .map_err(|e| identity_refused(&req.recipient_pial_id, e))?;
    if let Some(content) = req.content_id.as_deref().and_then(ContentRef::parse) {
        register_content(&state.manhattan, KIND_WORK, &content).await;
    }
    let units_per_aet = units_or_unavailable(&state).await?;
    let key = format!("themis:tip:{}:{}", sender.text(), req.idempotency_key.trim());
    // A retried tip is the same tip: answer with what became of it.
    let prior: Option<Uuid> =
        sqlx::query_scalar("SELECT id FROM commerce_transactions WHERE idempotency_key = $1")
            .bind(&key)
            .fetch_optional(&state.pool)
            .await
            .map_err(cdberr)?;
    let charge_id = match prior {
        Some(id) => id,
        None => {
            let tip_id = Uuid::new_v4();
            let display = format_aet(req.amount_units, units_per_aet);
            let notice = crate::notifications::Event {
                actor_account: req.from_account.clone(),
                recipient_account: req.to_account.clone(),
                event_id: format!("themis:tip:{tip_id}"),
                recipient_pial: req.recipient_pial_id.text(),
                kind: "tip".into(),
                target_id: req.target_id.clone(),
                target_type: req.target_type.clone(),
                // The native clients read micro-AET; the ratio comes from the
                // ledger's own unit.
                payload: json!({
                    "amount_units": req.amount_units,
                    "amount_aet": display,
                    "amount_uaet": req.amount_units * (1_000_000 / units_per_aet),
                }),
                push_title: if req.from_handle.is_empty() {
                    "You received a tip".into()
                } else {
                    format!("@{} sent you a tip", req.from_handle)
                },
                push_body: display.clone(),
                push_url: "/wallet".into(),
            };
            let mut tx = state.pool.begin().await.map_err(cdberr)?;
            sqlx::query(
                "INSERT INTO tips \
                   (id, sender_pial_id, recipient_pial_id, amount_aet, message, content_id, status) \
                 VALUES ($1, $2, $3, $4, $5, $6, 'pending')",
            )
            .bind(tip_id)
            .bind(sender.text())
            .bind(req.recipient_pial_id.text())
            .bind(req.amount_units)
            .bind(&req.message)
            .bind(req.content_id.as_deref())
            .execute(&mut *tx)
            .await
            .map_err(cdberr)?;
            let id = charges::record(
                &mut tx,
                "tip",
                tip_id,
                &sender.text(),
                &req.recipient_pial_id.text(),
                req.amount_units,
                &key,
                None,
                Some(&notice),
            )
            .await
            .map_err(cdberr)?;
            tx.commit().await.map_err(cdberr)?;
            id
        }
    };
    let settled = charges::run(&state, charge_id)
        .await
        .map_err(|e| charge_unrecorded("tip", charge_id, e))?;
    let tx_id = charge_answer(settled)?;
    Ok(Json(json!({
        "amount_units": req.amount_units,
        "amount_display": format_aet(req.amount_units, units_per_aet),
        "ain_soph_tx_id": tx_id,
    })))
}

pub async fn creator_earnings(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Value> {
    require_caller(&headers, &state.cfg.internal_api_key, &pial_id)?;
    let units_per_aet = units_or_unavailable(&state).await?;
    let by_type: Vec<(String, i64, i64)> = sqlx::query_as(
        "SELECT tx_type, COALESCE(SUM(gross_aet), 0)::bigint, COUNT(*)::bigint \
         FROM commerce_transactions \
         WHERE creator_pial_id = $1 AND status = 'completed' GROUP BY tx_type",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    let (mut total, mut subs, mut ppv, mut tips, mut tx_count) = (0i64, 0i64, 0i64, 0i64, 0i64);
    for (tx_type, amount, count) in &by_type {
        tx_count += count;
        total += amount;
        match tx_type.as_str() {
            "subscription" | "subscription_renewal" => subs += amount,
            "ppv" => ppv += amount,
            "tip" => tips += amount,
            _ => {}
        }
    }
    let active_subscribers: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM subscriptions \
          WHERE creator_pial_id = $1 AND status = 'active' AND current_period_end > NOW()",
    )
    .bind(pial_id.text())
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({
        "total_units":           total,
        "total_display":         format_aet(total, units_per_aet),
        "subscriptions_units":   subs,
        "subscriptions_display": format_aet(subs, units_per_aet),
        "ppv_units":             ppv,
        "tips_units":            tips,
        "tips_display":          format_aet(tips, units_per_aet),
        "tx_count":              tx_count,
        "active_subscribers":    active_subscribers,
    })))
}
