//! Charges: every AET value move Themis makes, and the loop that finishes them.
//!
//! A charge is a `commerce_transactions` row. It is written 'pending' in the
//! same transaction as the thing being paid for (a subscription, a period's
//! renewal, a pay-per-view purchase, a tip), carrying the idempotency key Ain
//! Soph dedupes on. Then the money moves, then the row settles:
//!
//!   Done      → 'completed', the domain row takes effect, its events and
//!               notification are queued in the same transaction.
//!   Declined  → 'failed', the domain row fails (a declined renewal expires the
//!               subscription), and nothing moved.
//!   Unknown   → stays 'pending'. The reconciler re-drives it with the same key,
//!               so the ledger either reports the move it already made or makes
//!               it now — never twice.
//!
//! The maintenance loop also renews subscriptions whose paid period has ended,
//! fills in the accounts feed-engine's projection is keyed on, and withdraws the
//! Caeor asset of a deleted listing that never sold.

use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use serde_json::Value;
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

use crate::clients::{AinSoph, Charge};
use crate::commerce_events::{self, SubscriptionEvent};
use crate::AppState;

/// A subscription's paid period.
pub const PERIOD_DAYS: i64 = 30;

const TICK: Duration = Duration::from_secs(30);
/// A pending charge is left to the request that made it for this long before
/// the reconciler takes it over.
const RECONCILE_AFTER_SECS: i64 = 30;

/// Where a charge stands after an attempt.
#[derive(Debug)]
pub enum Settled {
    Completed(String),
    Failed(String),
    Pending(String),
}

/// The idempotency key of one subscription period's charge.
pub fn period_key(subscription_id: Uuid, period: i32) -> String {
    format!("themis:subscription:{subscription_id}:period:{period}")
}

/// Writes a pending charge inside the caller's transaction.
#[allow(clippy::too_many_arguments)]
pub async fn record(
    tx: &mut Transaction<'_, Postgres>,
    tx_type: &str,
    reference_id: Uuid,
    payer: &str,
    payee: &str,
    units: i64,
    idempotency_key: &str,
    period_index: Option<i32>,
    notice: Option<&crate::notifications::Event>,
) -> Result<Uuid, sqlx::Error> {
    let notice = notice.map(crate::notifications::to_stored);
    sqlx::query_scalar(
        "INSERT INTO commerce_transactions
           (tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet,
            idempotency_key, period_index, notice, status)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending')
         RETURNING id",
    )
    .bind(tx_type)
    .bind(reference_id)
    .bind(payer)
    .bind(payee)
    .bind(units)
    .bind(idempotency_key)
    .bind(period_index)
    .bind(notice)
    .fetch_one(&mut **tx)
    .await
}

#[derive(sqlx::FromRow)]
struct ChargeRow {
    tx_type: String,
    reference_id: Uuid,
    payer_pial_id: String,
    creator_pial_id: String,
    gross_aet: i64,
    idempotency_key: String,
    period_index: Option<i32>,
    notice: Option<Value>,
    status: String,
    failure_reason: Option<String>,
    ain_soph_tx_id: Option<String>,
}

/// Attempts one charge and settles it by what the ledger said.
pub async fn run(state: &AppState, charge_id: Uuid) -> Result<Settled> {
    let row: ChargeRow = sqlx::query_as(
        "SELECT tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet,
                idempotency_key, period_index, notice, status, failure_reason, ain_soph_tx_id
           FROM commerce_transactions WHERE id = $1",
    )
    .bind(charge_id)
    .fetch_one(&state.pool)
    .await?;
    match row.status.as_str() {
        "completed" => return Ok(Settled::Completed(row.ain_soph_tx_id.unwrap_or_default())),
        "failed" => return Ok(Settled::Failed(row.failure_reason.unwrap_or_default())),
        _ => {}
    }
    let ain = AinSoph { http: &state.http, url: &state.cfg.ain_soph_url, key: &state.cfg.internal_api_key };
    let outcome = match row.tx_type.as_str() {
        "tip" => ain.tip(&row.payer_pial_id, &row.creator_pial_id, row.gross_aet, &row.idempotency_key).await,
        meta => {
            ain.transfer(&row.payer_pial_id, &row.creator_pial_id, row.gross_aet, &row.idempotency_key, meta)
                .await
        }
    };
    match outcome {
        Charge::Done(tx_id) => {
            complete(state, charge_id, &row, &tx_id).await?;
            Ok(Settled::Completed(tx_id))
        }
        Charge::Declined(why) => {
            decline(state, charge_id, &row, &why).await?;
            Ok(Settled::Failed(why))
        }
        Charge::Unknown(why) => {
            tracing::warn!(%charge_id, reason = %why, "charge outcome unknown; the reconciler will re-drive it");
            Ok(Settled::Pending(why))
        }
    }
}

async fn complete(state: &AppState, charge_id: Uuid, row: &ChargeRow, tx_id: &str) -> Result<()> {
    let mut tx = state.pool.begin().await?;
    let moved = sqlx::query(
        "UPDATE commerce_transactions SET status = 'completed', ain_soph_tx_id = $2, settled_at = NOW()
          WHERE id = $1 AND status = 'pending'",
    )
    .bind(charge_id)
    .bind(tx_id)
    .execute(&mut *tx)
    .await?
    .rows_affected();
    if moved == 0 {
        tx.rollback().await?;
        return Ok(());
    }
    let r = row.reference_id;
    match row.tx_type.as_str() {
        "subscription" => {
            sqlx::query(
                "UPDATE subscriptions SET status = 'active', ain_soph_tx_id = $2
                  WHERE id = $1 AND status = 'pending'",
            )
            .bind(r)
            .bind(tx_id)
            .execute(&mut *tx)
            .await?;
            commerce_events::subscription(&mut tx, SubscriptionEvent::Created, r).await?;
        }
        "subscription_renewal" => {
            let period = row.period_index.unwrap_or(0);
            sqlx::query(
                "UPDATE subscriptions
                    SET period_index = $2,
                        current_period_start = current_period_end,
                        current_period_end = current_period_end + make_interval(days => $3),
                        ain_soph_tx_id = $4
                  WHERE id = $1 AND status = 'active' AND period_index = $2 - 1",
            )
            .bind(r)
            .bind(period)
            .bind(PERIOD_DAYS as i32)
            .bind(tx_id)
            .execute(&mut *tx)
            .await?;
            commerce_events::subscription(&mut tx, SubscriptionEvent::Renewed, r).await?;
        }
        "ppv" => {
            sqlx::query(
                "UPDATE ppv_purchases SET status = 'completed', ain_soph_tx_id = $2
                  WHERE id = $1 AND status = 'pending'",
            )
            .bind(r)
            .bind(tx_id)
            .execute(&mut *tx)
            .await?;
        }
        "tip" => {
            sqlx::query(
                "UPDATE tips SET status = 'completed', ain_soph_tx_id = $2
                  WHERE id = $1 AND status = 'pending'",
            )
            .bind(r)
            .bind(tx_id)
            .execute(&mut *tx)
            .await?;
        }
        other => anyhow::bail!("charge {charge_id} has unknown type {other}"),
    }
    if let Some(notice) = &row.notice {
        crate::notifications::enqueue_stored(&mut *tx, notice).await?;
    }
    tx.commit().await?;
    Ok(())
}

async fn decline(state: &AppState, charge_id: Uuid, row: &ChargeRow, why: &str) -> Result<()> {
    let mut tx = state.pool.begin().await?;
    let moved = sqlx::query(
        "UPDATE commerce_transactions SET status = 'failed', failure_reason = $2, settled_at = NOW()
          WHERE id = $1 AND status = 'pending'",
    )
    .bind(charge_id)
    .bind(why)
    .execute(&mut *tx)
    .await?
    .rows_affected();
    if moved == 0 {
        tx.rollback().await?;
        return Ok(());
    }
    let r = row.reference_id;
    match row.tx_type.as_str() {
        "subscription" => {
            sqlx::query(
                "UPDATE subscriptions SET status = 'failed', failure_reason = $2
                  WHERE id = $1 AND status = 'pending'",
            )
            .bind(r)
            .bind(why)
            .execute(&mut *tx)
            .await?;
        }
        "subscription_renewal" => {
            // A declined renewal ends the subscription: it is never carried on
            // unpaid.
            let period = row.period_index.unwrap_or(0);
            let ended = sqlx::query(
                "UPDATE subscriptions SET status = 'expired', failure_reason = $3
                  WHERE id = $1 AND status = 'active' AND period_index = $2 - 1",
            )
            .bind(r)
            .bind(period)
            .bind(format!("renewal declined: {why}"))
            .execute(&mut *tx)
            .await?
            .rows_affected();
            if ended > 0 {
                commerce_events::subscription(&mut tx, SubscriptionEvent::Expired, r).await?;
            }
        }
        "ppv" => {
            sqlx::query(
                "UPDATE ppv_purchases SET status = 'failed', failure_reason = $2
                  WHERE id = $1 AND status = 'pending'",
            )
            .bind(r)
            .bind(why)
            .execute(&mut *tx)
            .await?;
        }
        "tip" => {
            sqlx::query(
                "UPDATE tips SET status = 'failed', failure_reason = $2
                  WHERE id = $1 AND status = 'pending'",
            )
            .bind(r)
            .bind(why)
            .execute(&mut *tx)
            .await?;
        }
        other => anyhow::bail!("charge {charge_id} has unknown type {other}"),
    }
    tx.commit().await?;
    Ok(())
}

// ── The maintenance loop ─────────────────────────────────────────────────────

pub fn spawn_maintenance(state: Arc<AppState>) {
    tokio::spawn(async move {
        let mut tick = tokio::time::interval(TICK);
        loop {
            tick.tick().await;
            if let Err(e) = reconcile(&state).await {
                tracing::error!(error = %e, "charge reconciliation pass failed");
            }
            if let Err(e) = renew(&state).await {
                tracing::error!(error = %e, "subscription renewal pass failed");
            }
            if let Err(e) = backfill_accounts(&state).await {
                tracing::error!(error = %e, "account backfill pass failed");
            }
            if let Err(e) = crate::caeor::withdraw_delisted(&state).await {
                tracing::error!(error = %e, "delisted asset withdrawal pass failed");
            }
        }
    });
}

/// Re-drives charges whose outcome nobody recorded.
async fn reconcile(state: &AppState) -> Result<()> {
    let ids: Vec<Uuid> = sqlx::query_scalar(
        "SELECT id FROM commerce_transactions
          WHERE status = 'pending' AND created_at < NOW() - make_interval(secs => $1)
          ORDER BY created_at LIMIT 100",
    )
    .bind(RECONCILE_AFTER_SECS as f64)
    .fetch_all(&state.pool)
    .await?;
    let n = ids.len();
    for id in ids {
        if let Err(e) = run(state, id).await {
            tracing::error!(charge_id = %id, error = %e, "reconciling charge failed");
        }
    }
    if n > 0 {
        tracing::info!(charges = n, "re-drove pending charges");
    }
    Ok(())
}

#[derive(sqlx::FromRow)]
struct DueSubscription {
    id: Uuid,
    period_index: i32,
    cancelled: bool,
    subscriber_pial_id: String,
    creator_pial_id: String,
    price: Option<i64>,
    plan_active: Option<bool>,
}

/// Ends or renews every active subscription whose paid period is over.
async fn renew(state: &AppState) -> Result<()> {
    let due: Vec<DueSubscription> = sqlx::query_as(
        "SELECT s.id, s.period_index, (s.cancelled_at IS NOT NULL) AS cancelled,
                s.subscriber_pial_id, s.creator_pial_id,
                p.price_aet AS price, p.is_active AS plan_active
           FROM subscriptions s
           LEFT JOIN subscription_plans p ON p.id = s.plan_id
          WHERE s.status = 'active' AND s.current_period_end <= NOW()
          ORDER BY s.current_period_end LIMIT 200",
    )
    .fetch_all(&state.pool)
    .await?;
    if due.is_empty() {
        return Ok(());
    }
    let units_per_aet = state.units_per_aet().await?;
    for s in due {
        let ending = if s.cancelled {
            Some(("cancelled", "cancelled by the subscriber".to_string()))
        } else if s.plan_active != Some(true) || s.price.unwrap_or(0) <= 0 {
            Some(("expired", "the plan was retired".to_string()))
        } else {
            None
        };
        if let Some((status, reason)) = ending {
            let mut tx = state.pool.begin().await?;
            let ended = sqlx::query(
                "UPDATE subscriptions SET status = $2, failure_reason = $3
                  WHERE id = $1 AND status = 'active' AND current_period_end <= NOW()",
            )
            .bind(s.id)
            .bind(status)
            .bind(&reason)
            .execute(&mut *tx)
            .await?
            .rows_affected();
            if ended > 0 {
                commerce_events::subscription(&mut tx, SubscriptionEvent::Expired, s.id).await?;
            }
            tx.commit().await?;
            continue;
        }

        let period = s.period_index + 1;
        let key = period_key(s.id, period);
        let price = s.price.unwrap_or(0);
        let notice = crate::handlers::sale_notice(
            format!("themis:renewal:{}:{period}", s.id),
            s.creator_pial_id.clone(),
            "subscribe",
            "Subscription renewed",
            s.id.to_string(),
            crate::clients::format_aet(price, units_per_aet),
        );
        let mut tx = state.pool.begin().await?;
        let existing: Option<Uuid> =
            sqlx::query_scalar("SELECT id FROM commerce_transactions WHERE idempotency_key = $1")
                .bind(&key)
                .fetch_optional(&mut *tx)
                .await?;
        let charge_id = match existing {
            Some(id) => id,
            None => {
                record(
                    &mut tx,
                    "subscription_renewal",
                    s.id,
                    &s.subscriber_pial_id,
                    &s.creator_pial_id,
                    price,
                    &key,
                    Some(period),
                    Some(&notice),
                )
                .await?
            }
        };
        tx.commit().await?;
        match run(state, charge_id).await {
            Ok(Settled::Completed(_)) => tracing::info!(subscription = %s.id, period, "subscription renewed"),
            Ok(Settled::Failed(why)) => tracing::info!(subscription = %s.id, period, reason = %why, "renewal declined; subscription expired"),
            Ok(Settled::Pending(_)) => {}
            Err(e) => tracing::error!(subscription = %s.id, period, error = %e, "renewal charge failed to settle"),
        }
    }
    Ok(())
}

/// feed-engine's answer for which account a person's commerce belongs to when
/// a row predates Themis carrying it.
async fn primary_account(state: &AppState, pial: &str) -> Result<Option<Uuid>> {
    let resp = state
        .http
        .get(format!("{}/api/internal/pial/{pial}/primary-account", state.cfg.nantar_url))
        .header("X-Internal-Key", &state.cfg.internal_api_key)
        .send()
        .await?;
    if resp.status() == reqwest::StatusCode::NOT_FOUND {
        return Ok(None);
    }
    let v: Value = resp.error_for_status()?.json().await?;
    Ok(v.get("account").and_then(|a| a.as_str()).and_then(|a| Uuid::parse_str(a).ok()))
}

/// Fills in the accounts of rows made before Themis carried them, and announces
/// each row once it can be projected.
async fn backfill_accounts(state: &AppState) -> Result<()> {
    let plans: Vec<(Uuid, String)> = sqlx::query_as(
        "SELECT id, creator_pial_id FROM subscription_plans WHERE creator_account IS NULL LIMIT 100",
    )
    .fetch_all(&state.pool)
    .await?;
    for (id, pial) in plans {
        let Some(account) = primary_account(state, &pial).await? else {
            tracing::warn!(plan = %id, "plan's creator has no account in feed-engine; not projectable");
            continue;
        };
        let mut tx = state.pool.begin().await?;
        sqlx::query("UPDATE subscription_plans SET creator_account = $2 WHERE id = $1 AND creator_account IS NULL")
            .bind(id)
            .bind(account)
            .execute(&mut *tx)
            .await?;
        commerce_events::plan(&mut tx, id).await?;
        tx.commit().await?;
    }

    let subs: Vec<(Uuid, String, String, Option<Uuid>, Option<Uuid>, String)> = sqlx::query_as(
        "SELECT id, subscriber_pial_id, creator_pial_id, subscriber_account, creator_account, status
           FROM subscriptions
          WHERE (subscriber_account IS NULL OR creator_account IS NULL)
            AND status IN ('active', 'cancelled', 'expired')
          LIMIT 100",
    )
    .fetch_all(&state.pool)
    .await?;
    for (id, sub_pial, creator_pial, sub_acct, creator_acct, status) in subs {
        let sub_acct = match sub_acct {
            Some(a) => Some(a),
            None => primary_account(state, &sub_pial).await?,
        };
        let creator_acct = match creator_acct {
            Some(a) => Some(a),
            None => primary_account(state, &creator_pial).await?,
        };
        let (Some(sa), Some(ca)) = (sub_acct, creator_acct) else {
            tracing::warn!(subscription = %id, "subscription's people have no accounts in feed-engine; not projectable");
            continue;
        };
        let mut tx = state.pool.begin().await?;
        sqlx::query(
            "UPDATE subscriptions SET subscriber_account = $2, creator_account = $3 WHERE id = $1",
        )
        .bind(id)
        .bind(sa)
        .bind(ca)
        .execute(&mut *tx)
        .await?;
        let kind = if status == "active" { SubscriptionEvent::Created } else { SubscriptionEvent::Expired };
        commerce_events::subscription(&mut tx, kind, id).await?;
        tx.commit().await?;
    }
    Ok(())
}
