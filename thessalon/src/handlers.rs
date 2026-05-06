use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    response::Json,
};
use chrono::Utc;
use serde_json::{json, Value};
use sqlx::PgPool;
use uuid::Uuid;

use crate::clients;
use crate::models::*;

#[derive(Clone)]
pub struct AppState {
    pub pool:         PgPool,
    pub http:         reqwest::Client,
    pub ain_soph_url: String,
    pub verity_url:   String,
}

type Res<T> = Result<Json<T>, (StatusCode, Json<Value>)>;

fn err(status: StatusCode, msg: &str) -> (StatusCode, Json<Value>) {
    (status, Json(json!({"error": msg})))
}

fn dberr(e: sqlx::Error) -> (StatusCode, Json<Value>) {
    err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string())
}

// ── Health ────────────────────────────────────────────────────────────────────

pub async fn health() -> Json<Value> {
    Json(json!({"status": "ok", "brain": "thessalon", "version": "1.0"}))
}

pub async fn stats(State(s): State<AppState>) -> Json<Value> {
    let creators: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM creator_eligibility WHERE monetization_enabled"
    ).fetch_one(&s.pool).await.unwrap_or((0,));

    let active_subs: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM subscriptions WHERE status = 'active'"
    ).fetch_one(&s.pool).await.unwrap_or((0,));

    let tx_count: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM transactions"
    ).fetch_one(&s.pool).await.unwrap_or((0,));

    let volume: (Option<i64>,) = sqlx::query_as(
        "SELECT SUM(gross_aet) FROM transactions WHERE status = 'completed'"
    ).fetch_one(&s.pool).await.unwrap_or((None,));

    Json(json!({
        "active_creators": creators.0,
        "active_subscriptions": active_subs.0,
        "total_transactions": tx_count.0,
        "total_volume_aet": volume.0.unwrap_or(0),
    }))
}

// ── Creator onboarding ────────────────────────────────────────────────────────

pub async fn creator_enable(
    State(s): State<AppState>,
    Json(req): Json<EnableCreatorReq>,
) -> Res<Value> {
    let tier = clients::kyc_tier(&s.http, &s.verity_url, &req.pial_id).await;

    sqlx::query(
        "INSERT INTO creator_eligibility (pial_id, kyc_tier, monetization_enabled, enabled_at) \
         VALUES ($1, $2, true, NOW()) \
         ON CONFLICT (pial_id) DO UPDATE \
         SET kyc_tier = $2, monetization_enabled = true, enabled_at = NOW(), \
             disabled_at = NULL, disabled_reason = NULL"
    )
    .bind(&req.pial_id)
    .bind(tier)
    .execute(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(json!({
        "pial_id": req.pial_id,
        "monetization_enabled": true,
        "kyc_tier": tier,
    })))
}

pub async fn creator_status(
    State(s): State<AppState>,
    Path(pial_id): Path<String>,
) -> Res<Value> {
    let row: Option<(bool, i32)> = sqlx::query_as(
        "SELECT monetization_enabled, kyc_tier FROM creator_eligibility WHERE pial_id = $1"
    )
    .bind(&pial_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    let (enabled, tier) = row.unwrap_or((false, 0));
    Ok(Json(json!({"pial_id": pial_id, "monetization_enabled": enabled, "kyc_tier": tier})))
}

// ── Subscription plans ────────────────────────────────────────────────────────

pub async fn list_plans(
    State(s): State<AppState>,
    Path(pial_id): Path<String>,
) -> Res<Vec<PlanRow>> {
    let rows = sqlx::query_as::<_, PlanRow>(
        "SELECT id, creator_pial_id, name, price_aet, description, is_active, created_at \
         FROM subscription_plans WHERE creator_pial_id = $1 ORDER BY price_aet ASC"
    )
    .bind(&pial_id)
    .fetch_all(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(rows))
}

pub async fn create_plan(
    State(s): State<AppState>,
    Json(req): Json<CreatePlanReq>,
) -> Res<PlanRow> {
    let enabled: Option<(bool,)> = sqlx::query_as(
        "SELECT monetization_enabled FROM creator_eligibility WHERE pial_id = $1"
    )
    .bind(&req.creator_pial_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    if !enabled.map(|r| r.0).unwrap_or(false) {
        return Err(err(StatusCode::FORBIDDEN, "creator monetization not enabled"));
    }

    let price_units = aet_to_units(req.price_aet);
    if price_units <= 0 {
        return Err(err(StatusCode::BAD_REQUEST, "price must be greater than zero"));
    }

    let row = sqlx::query_as::<_, PlanRow>(
        "INSERT INTO subscription_plans (creator_pial_id, name, price_aet, description) \
         VALUES ($1, $2, $3, $4) \
         RETURNING id, creator_pial_id, name, price_aet, description, is_active, created_at"
    )
    .bind(&req.creator_pial_id)
    .bind(&req.name)
    .bind(price_units)
    .bind(&req.description)
    .fetch_one(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(row))
}

pub async fn update_plan(
    State(s): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<UpdatePlanReq>,
) -> Res<PlanRow> {
    let price = req.price_aet.map(aet_to_units);

    let row = sqlx::query_as::<_, PlanRow>(
        "UPDATE subscription_plans SET \
           name        = COALESCE($2, name), \
           price_aet   = COALESCE($3, price_aet), \
           description = COALESCE($4, description), \
           is_active   = COALESCE($5, is_active) \
         WHERE id = $1 \
         RETURNING id, creator_pial_id, name, price_aet, description, is_active, created_at"
    )
    .bind(id)
    .bind(req.name)
    .bind(price)
    .bind(req.description)
    .bind(req.is_active)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?
    .ok_or_else(|| err(StatusCode::NOT_FOUND, "plan not found"))?;

    Ok(Json(row))
}

pub async fn delete_plan(
    State(s): State<AppState>,
    Path(id): Path<Uuid>,
) -> Res<Value> {
    sqlx::query("UPDATE subscription_plans SET is_active = false WHERE id = $1")
        .bind(id)
        .execute(&s.pool)
        .await
        .map_err(dberr)?;

    Ok(Json(json!({"id": id, "is_active": false})))
}

// ── Subscriptions ─────────────────────────────────────────────────────────────

pub async fn subscribe(
    State(s): State<AppState>,
    Json(req): Json<SubscribeReq>,
) -> Res<Value> {
    let existing: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM subscriptions \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 AND status = 'active'"
    )
    .bind(&req.subscriber_pial_id)
    .bind(&req.creator_pial_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    if existing.is_some() {
        return Err(err(StatusCode::CONFLICT, "already subscribed"));
    }

    let plan: Option<(i64,)> = sqlx::query_as(
        "SELECT price_aet FROM subscription_plans WHERE id = $1 AND is_active"
    )
    .bind(req.plan_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    let price_units = plan
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "plan not found or inactive"))?.0;

    let tx_id = clients::transfer(
        &s.http, &s.ain_soph_url,
        &req.subscriber_pial_id, &req.creator_pial_id,
        price_units, "subscription",
    )
    .await
    .map_err(|e| err(StatusCode::PAYMENT_REQUIRED, &e.to_string()))?;

    let period_end = Utc::now() + chrono::Duration::days(30);

    let sub_id: (Uuid,) = sqlx::query_as(
        "INSERT INTO subscriptions \
           (subscriber_pial_id, creator_pial_id, plan_id, current_period_end) \
         VALUES ($1, $2, $3, $4) RETURNING id"
    )
    .bind(&req.subscriber_pial_id)
    .bind(&req.creator_pial_id)
    .bind(req.plan_id)
    .bind(period_end)
    .fetch_one(&s.pool)
    .await
    .map_err(dberr)?;

    sqlx::query(
        "INSERT INTO transactions \
           (tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet, ain_soph_tx_id) \
         VALUES ('subscription', $1, $2, $3, $4, $5)"
    )
    .bind(sub_id.0)
    .bind(&req.subscriber_pial_id)
    .bind(&req.creator_pial_id)
    .bind(price_units)
    .bind(&tx_id)
    .execute(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(json!({
        "subscription_id": sub_id.0,
        "subscriber_pial_id": req.subscriber_pial_id,
        "creator_pial_id":    req.creator_pial_id,
        "plan_id":            req.plan_id,
        "amount_aet":         units_to_aet(price_units),
        "period_end":         period_end,
        "ain_soph_tx_id":     tx_id,
        "status":             "active",
    })))
}

pub async fn unsubscribe(
    State(s): State<AppState>,
    Json(req): Json<UnsubscribeReq>,
) -> Res<Value> {
    let result = sqlx::query(
        "UPDATE subscriptions SET status = 'cancelled', cancelled_at = NOW() \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 AND status = 'active'"
    )
    .bind(&req.subscriber_pial_id)
    .bind(&req.creator_pial_id)
    .execute(&s.pool)
    .await
    .map_err(dberr)?;

    if result.rows_affected() == 0 {
        return Err(err(StatusCode::NOT_FOUND, "no active subscription found"));
    }

    Ok(Json(json!({"status": "cancelled"})))
}

pub async fn check_subscription(
    State(s): State<AppState>,
    Query(q): Query<AccessQuery>,
) -> Res<Value> {
    let subscriber = q.subscriber
        .ok_or_else(|| err(StatusCode::BAD_REQUEST, "subscriber required"))?;
    let creator = q.creator
        .ok_or_else(|| err(StatusCode::BAD_REQUEST, "creator required"))?;

    let has: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM subscriptions \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 \
           AND status = 'active' AND current_period_end > NOW()"
    )
    .bind(&subscriber)
    .bind(&creator)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(json!({"has_access": has.is_some()})))
}

pub async fn list_subscribers(
    State(s): State<AppState>,
    Path(pial_id): Path<String>,
) -> Res<Vec<SubscriptionRow>> {
    let rows = sqlx::query_as::<_, SubscriptionRow>(
        "SELECT id, subscriber_pial_id, creator_pial_id, plan_id, status, \
                current_period_start, current_period_end, cancelled_at, created_at \
         FROM subscriptions WHERE creator_pial_id = $1 ORDER BY created_at DESC"
    )
    .bind(&pial_id)
    .fetch_all(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(rows))
}

pub async fn list_subscriptions(
    State(s): State<AppState>,
    Path(pial_id): Path<String>,
) -> Res<Vec<SubscriptionRow>> {
    let rows = sqlx::query_as::<_, SubscriptionRow>(
        "SELECT id, subscriber_pial_id, creator_pial_id, plan_id, status, \
                current_period_start, current_period_end, cancelled_at, created_at \
         FROM subscriptions WHERE subscriber_pial_id = $1 ORDER BY created_at DESC"
    )
    .bind(&pial_id)
    .fetch_all(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(rows))
}

// ── PPV ───────────────────────────────────────────────────────────────────────

pub async fn create_ppv(
    State(s): State<AppState>,
    Json(req): Json<CreatePpvReq>,
) -> Res<PpvItemRow> {
    let enabled: Option<(bool,)> = sqlx::query_as(
        "SELECT monetization_enabled FROM creator_eligibility WHERE pial_id = $1"
    )
    .bind(&req.creator_pial_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    if !enabled.map(|r| r.0).unwrap_or(false) {
        return Err(err(StatusCode::FORBIDDEN, "creator monetization not enabled"));
    }

    let price_units = aet_to_units(req.price_aet);
    if price_units <= 0 {
        return Err(err(StatusCode::BAD_REQUEST, "price must be greater than zero"));
    }

    let row = sqlx::query_as::<_, PpvItemRow>(
        "INSERT INTO ppv_items (creator_pial_id, content_id, price_aet, title) \
         VALUES ($1, $2, $3, $4) \
         RETURNING id, creator_pial_id, content_id, price_aet, title, is_active, created_at"
    )
    .bind(&req.creator_pial_id)
    .bind(&req.content_id)
    .bind(price_units)
    .bind(&req.title)
    .fetch_one(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(row))
}

pub async fn purchase_ppv(
    State(s): State<AppState>,
    Path(item_id): Path<Uuid>,
    Json(req): Json<PurchasePpvReq>,
) -> Res<Value> {
    let item: Option<(String, i64)> = sqlx::query_as(
        "SELECT creator_pial_id, price_aet FROM ppv_items WHERE id = $1 AND is_active"
    )
    .bind(item_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    let (creator_pial, price_units) =
        item.ok_or_else(|| err(StatusCode::NOT_FOUND, "PPV item not found"))?;

    let existing: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM ppv_purchases WHERE buyer_pial_id = $1 AND ppv_item_id = $2"
    )
    .bind(&req.buyer_pial_id)
    .bind(item_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    if existing.is_some() {
        return Err(err(StatusCode::CONFLICT, "already purchased"));
    }

    let tx_id = clients::transfer(
        &s.http, &s.ain_soph_url,
        &req.buyer_pial_id, &creator_pial,
        price_units, "ppv",
    )
    .await
    .map_err(|e| err(StatusCode::PAYMENT_REQUIRED, &e.to_string()))?;

    let purchase_id: (Uuid,) = sqlx::query_as(
        "INSERT INTO ppv_purchases (buyer_pial_id, ppv_item_id, amount_aet, ain_soph_tx_id) \
         VALUES ($1, $2, $3, $4) RETURNING id"
    )
    .bind(&req.buyer_pial_id)
    .bind(item_id)
    .bind(price_units)
    .bind(&tx_id)
    .fetch_one(&s.pool)
    .await
    .map_err(dberr)?;

    sqlx::query(
        "INSERT INTO transactions \
           (tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet, ain_soph_tx_id) \
         VALUES ('ppv', $1, $2, $3, $4, $5)"
    )
    .bind(purchase_id.0)
    .bind(&req.buyer_pial_id)
    .bind(&creator_pial)
    .bind(price_units)
    .bind(&tx_id)
    .execute(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(json!({
        "purchase_id":    purchase_id.0,
        "item_id":        item_id,
        "amount_aet":     units_to_aet(price_units),
        "ain_soph_tx_id": tx_id,
        "has_access":     true,
    })))
}

pub async fn check_ppv_access(
    State(s): State<AppState>,
    Query(q): Query<AccessQuery>,
) -> Res<Value> {
    let buyer = q.buyer
        .ok_or_else(|| err(StatusCode::BAD_REQUEST, "buyer required"))?;
    let content_id = q.content_id
        .ok_or_else(|| err(StatusCode::BAD_REQUEST, "content_id required"))?;

    let has: Option<(Uuid,)> = sqlx::query_as(
        "SELECT p.id FROM ppv_purchases p \
         JOIN ppv_items i ON i.id = p.ppv_item_id \
         WHERE p.buyer_pial_id = $1 AND i.content_id = $2"
    )
    .bind(&buyer)
    .bind(&content_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(json!({"has_access": has.is_some()})))
}

pub async fn list_ppv_items(
    State(s): State<AppState>,
    Path(pial_id): Path<String>,
) -> Res<Vec<PpvItemRow>> {
    let rows = sqlx::query_as::<_, PpvItemRow>(
        "SELECT id, creator_pial_id, content_id, price_aet, title, is_active, created_at \
         FROM ppv_items WHERE creator_pial_id = $1 ORDER BY created_at DESC"
    )
    .bind(&pial_id)
    .fetch_all(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(rows))
}

// ── Tips ──────────────────────────────────────────────────────────────────────

pub async fn send_tip(
    State(s): State<AppState>,
    Json(req): Json<SendTipReq>,
) -> Res<Value> {
    if req.amount_aet <= 0.0 {
        return Err(err(StatusCode::BAD_REQUEST, "amount must be greater than zero"));
    }
    if req.sender_pial_id == req.recipient_pial_id {
        return Err(err(StatusCode::BAD_REQUEST, "cannot tip yourself"));
    }

    let tx_id = clients::tip(
        &s.http, &s.ain_soph_url,
        &req.sender_pial_id, &req.recipient_pial_id,
        req.amount_aet, &req.message,
    )
    .await
    .map_err(|e| err(StatusCode::PAYMENT_REQUIRED, &e.to_string()))?;

    let tip_id: (Uuid,) = sqlx::query_as(
        "INSERT INTO tips \
           (sender_pial_id, recipient_pial_id, amount_aet, message, content_id, ain_soph_tx_id) \
         VALUES ($1, $2, $3, $4, $5, $6) RETURNING id"
    )
    .bind(&req.sender_pial_id)
    .bind(&req.recipient_pial_id)
    .bind(aet_to_units(req.amount_aet))
    .bind(&req.message)
    .bind(req.content_id.as_deref())
    .bind(&tx_id)
    .fetch_one(&s.pool)
    .await
    .map_err(dberr)?;

    sqlx::query(
        "INSERT INTO transactions \
           (tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet, ain_soph_tx_id) \
         VALUES ('tip', $1, $2, $3, $4, $5)"
    )
    .bind(tip_id.0)
    .bind(&req.sender_pial_id)
    .bind(&req.recipient_pial_id)
    .bind(aet_to_units(req.amount_aet))
    .bind(&tx_id)
    .execute(&s.pool)
    .await
    .map_err(dberr)?;

    Ok(Json(json!({
        "tip_id":         tip_id.0,
        "amount_aet":     req.amount_aet,
        "ain_soph_tx_id": tx_id,
    })))
}

// ── Earnings ──────────────────────────────────────────────────────────────────

pub async fn earnings(
    State(s): State<AppState>,
    Path(pial_id): Path<String>,
) -> Res<Value> {
    let total: (i64,) = sqlx::query_as(
        "SELECT COALESCE(SUM(gross_aet), 0)::bigint FROM transactions \
         WHERE creator_pial_id = $1 AND status = 'completed'"
    )
    .bind(&pial_id)
    .fetch_one(&s.pool)
    .await
    .map_err(dberr)?;

    let by_type: Vec<(String, i64, i64)> = sqlx::query_as(
        "SELECT tx_type, COALESCE(SUM(gross_aet), 0)::bigint, COUNT(*)::bigint \
         FROM transactions \
         WHERE creator_pial_id = $1 AND status = 'completed' GROUP BY tx_type"
    )
    .bind(&pial_id)
    .fetch_all(&s.pool)
    .await
    .unwrap_or_default();

    let mut subs_aet = 0i64;
    let mut ppv_aet  = 0i64;
    let mut tips_aet = 0i64;
    let mut tx_count = 0i64;

    for (tx_type, amount, count) in &by_type {
        tx_count += count;
        match tx_type.as_str() {
            "subscription" => subs_aet += amount,
            "ppv"          => ppv_aet  += amount,
            "tip"          => tips_aet += amount,
            _              => {}
        }
    }

    Ok(Json(json!({
        "pial_id":              pial_id,
        "total_aet":            total.0,
        "total_aet_display":    units_to_aet(total.0),
        "subscriptions_aet":    subs_aet,
        "ppv_aet":              ppv_aet,
        "tips_aet":             tips_aet,
        "tx_count":             tx_count,
    })))
}
