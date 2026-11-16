//! commerce.events — every commerce decision feed-engine projects, announced
//! exactly once, in order.
//!
//! Themis is the only writer of plans and subscriptions. feed-engine keeps a
//! read projection of them (its `creator_plans` and `subscriptions` tables)
//! because its feed, fanout and gating queries join on them. That projection is
//! fed only by this topic.
//!
//! A decision is written to `commerce_outbox` in the SAME transaction as the
//! decision itself. The relay publishes rows in seq order and marks a row only
//! after the broker has acknowledged it. A broker outage holds the queue and
//! loses nothing; a crash between publish and mark re-publishes the same
//! event_id, which the consumer dedupes on.
//!
//! The record on the wire:
//!
//! ```json
//! { "event_id": "uuid", "seq": 42,
//!   "type": "subscription.created" | "subscription.renewed" | "subscription.resumed"
//!         | "subscription.cancelled" | "subscription.expired"
//!         | "plan.changed" | "purchase.delivered",
//!   "subject": {"kind": "subscription"|"plan"|"purchase", "id": "..."},
//!   "subscription": { "id", "subscriber_account", "creator_account", "plan_id",
//!                     "status", "period_end", "cancelled_at", "price_units" }?,
//!   "plan": { "id", "creator_account", "name", "description", "price_units",
//!             "is_active" }?,
//!   "purchase": { "id", "buyer_pial", "listing_id" }?,
//!   "occurred_at": "RFC3339" }
//! ```
//!
//! Subscription events carry the full state of the subscription, so applying
//! one is an upsert and a replay converges on the same rows. The subject of a
//! subscription is the (subscriber account, creator account) pair, because
//! that is the row the projection keeps. Records are keyed by subject, so every
//! event about one subject lands on one partition in decision order; `seq` is
//! global and strictly increasing, so it also increases per subject.

use std::time::Duration;

use rdkafka::config::ClientConfig;
use rdkafka::error::{KafkaError, RDKafkaErrorCode};
use rdkafka::producer::{FutureProducer, FutureRecord};
use rdkafka::util::Timeout;
use serde_json::{json, Value};
use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

pub const TOPIC: &str = "commerce.events";
const LOCK_KEY: i64 = 7331_0101;
const BATCH: i64 = 200;
const IDLE: Duration = Duration::from_millis(500);
/// Refusals of one record, for an error the broker will never stop giving,
/// before it is quarantined so the records behind it move again.
const MAX_REFUSALS: i32 = 12;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SubscriptionEvent {
    Created,
    Renewed,
    Resumed,
    Cancelled,
    Expired,
}

impl SubscriptionEvent {
    fn as_str(self) -> &'static str {
        match self {
            SubscriptionEvent::Created => "subscription.created",
            SubscriptionEvent::Renewed => "subscription.renewed",
            SubscriptionEvent::Resumed => "subscription.resumed",
            SubscriptionEvent::Cancelled => "subscription.cancelled",
            SubscriptionEvent::Expired => "subscription.expired",
        }
    }
}

async fn insert(
    tx: &mut Transaction<'_, Postgres>,
    key: String,
    payload: Value,
) -> sqlx::Result<()> {
    sqlx::query("INSERT INTO commerce_outbox (partition_key, payload) VALUES ($1, $2)")
        .bind(key)
        .bind(payload)
        .execute(&mut **tx)
        .await?;
    Ok(())
}

/// Queues the state of one subscription, as it stands inside the caller's
/// transaction. A subscription without the accounts feed-engine keys it on
/// cannot be projected yet; it is announced once the accounts are known (see
/// `charges::backfill_accounts`), so this returns without queueing.
pub async fn subscription(
    tx: &mut Transaction<'_, Postgres>,
    kind: SubscriptionEvent,
    subscription_id: Uuid,
) -> sqlx::Result<()> {
    let row: Option<(Option<Uuid>, Option<Uuid>, Option<Uuid>, String, chrono::DateTime<chrono::Utc>, Option<chrono::DateTime<chrono::Utc>>, Option<i64>)> =
        sqlx::query_as(
            "SELECT s.subscriber_account, s.creator_account, s.plan_id, s.status,
                    s.current_period_end, s.cancelled_at, p.price_aet
               FROM subscriptions s
               LEFT JOIN subscription_plans p ON p.id = s.plan_id
              WHERE s.id = $1",
        )
        .bind(subscription_id)
        .fetch_optional(&mut **tx)
        .await?;
    let Some((Some(subscriber), Some(creator), plan_id, status, period_end, cancelled_at, price)) = row else {
        tracing::warn!(%subscription_id, "subscription not announced yet: its accounts are not known");
        return Ok(());
    };
    let payload = json!({
        "type": kind.as_str(),
        "subject": { "kind": "subscription", "id": format!("{subscriber}:{creator}") },
        "subscription": {
            "id": subscription_id,
            "subscriber_account": subscriber,
            "creator_account": creator,
            "plan_id": plan_id,
            "status": status,
            "period_end": period_end,
            "cancelled_at": cancelled_at,
            "price_units": price.unwrap_or(0),
        },
        "occurred_at": chrono::Utc::now().to_rfc3339(),
    });
    insert(tx, format!("subscription:{subscriber}:{creator}"), payload).await
}

/// Queues the state of one plan. A plan without its creator's account is
/// announced once the account is known.
pub async fn plan(tx: &mut Transaction<'_, Postgres>, plan_id: Uuid) -> sqlx::Result<()> {
    let row: Option<(Option<Uuid>, String, String, i64, bool)> = sqlx::query_as(
        "SELECT creator_account, name, description, price_aet, is_active
           FROM subscription_plans WHERE id = $1",
    )
    .bind(plan_id)
    .fetch_optional(&mut **tx)
    .await?;
    let Some((Some(creator), name, description, price, active)) = row else {
        tracing::warn!(%plan_id, "plan not announced yet: its creator's account is not known");
        return Ok(());
    };
    let payload = json!({
        "type": "plan.changed",
        "subject": { "kind": "plan", "id": plan_id },
        "plan": {
            "id": plan_id,
            "creator_account": creator,
            "name": name,
            "description": description,
            "price_units": price,
            "is_active": active,
        },
        "occurred_at": chrono::Utc::now().to_rfc3339(),
    });
    insert(tx, format!("plan:{plan_id}"), payload).await
}

/// Queues a delivered purchase: the buyer's library has a new item.
pub async fn purchase_delivered(
    tx: &mut Transaction<'_, Postgres>,
    purchase_id: Uuid,
    buyer_pial: &str,
    listing_id: Uuid,
) -> sqlx::Result<()> {
    let payload = json!({
        "type": "purchase.delivered",
        "subject": { "kind": "purchase", "id": purchase_id },
        "purchase": { "id": purchase_id, "buyer_pial": buyer_pial, "listing_id": listing_id },
        "occurred_at": chrono::Utc::now().to_rfc3339(),
    });
    insert(tx, format!("purchase:{purchase_id}"), payload).await
}

pub async fn backlog(pool: &PgPool) -> sqlx::Result<(i64, i64)> {
    sqlx::query_as(
        "SELECT COUNT(*) FILTER (WHERE blocked_at IS NULL), COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)
           FROM commerce_outbox WHERE published_at IS NULL",
    )
    .fetch_one(pool)
    .await
}

/// Errors the broker will give for this record however often it is retried.
fn permanent(e: &KafkaError) -> bool {
    matches!(
        e,
        KafkaError::MessageProduction(
            RDKafkaErrorCode::MessageSizeTooLarge
                | RDKafkaErrorCode::InvalidMessage
                | RDKafkaErrorCode::InvalidMessageSize
                | RDKafkaErrorCode::InvalidRecord
        )
    )
}

pub fn spawn_relay(pool: PgPool, brokers: String) {
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            match relay(&pool, &brokers).await {
                Ok(()) => backoff = Duration::from_secs(1),
                Err(e) => {
                    tracing::error!(error = %e, "commerce.events relay stopped; restarting");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
            }
        }
    });
}

/// Holds the relay lock for as long as its connection lives, and publishes.
async fn relay(pool: &PgPool, brokers: &str) -> anyhow::Result<()> {
    let producer: FutureProducer = ClientConfig::new()
        .set("bootstrap.servers", brokers)
        .set("enable.idempotence", "true")
        .set("acks", "all")
        .set("message.timeout.ms", "30000")
        .create()?;

    let mut lock_conn = pool.acquire().await?.detach();
    loop {
        let held: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
            .bind(LOCK_KEY)
            .fetch_one(&mut lock_conn)
            .await?;
        if held {
            break;
        }
        // Another Themis holds the relay. Order needs exactly one.
        tokio::time::sleep(Duration::from_secs(5)).await;
    }
    tracing::info!("commerce.events relay holds its lock");

    let mut last_report = std::time::Instant::now() - Duration::from_secs(120);
    loop {
        // The lock lives on this connection: if it is gone, so is the lock.
        sqlx::query("SELECT 1").execute(&mut lock_conn).await?;

        if last_report.elapsed() >= Duration::from_secs(60) {
            last_report = std::time::Instant::now();
            let (pending, blocked) = backlog(pool).await?;
            metrics::gauge!("themis_commerce_outbox_pending").set(pending as f64);
            metrics::gauge!("themis_commerce_outbox_quarantined").set(blocked as f64);
            if blocked > 0 {
                tracing::error!(
                    quarantined = blocked,
                    "INCIDENT: commerce.events records quarantined — decisions not announced. \
                     SELECT seq, blocked_reason FROM commerce_outbox WHERE blocked_at IS NOT NULL AND published_at IS NULL"
                );
            }
        }

        let rows: Vec<(i64, Uuid, String, Value, i32)> = sqlx::query_as(
            "SELECT seq, event_id, partition_key, payload, attempts
               FROM commerce_outbox
              WHERE published_at IS NULL AND blocked_at IS NULL
              ORDER BY seq LIMIT $1",
        )
        .bind(BATCH)
        .fetch_all(pool)
        .await?;
        if rows.is_empty() {
            tokio::time::sleep(IDLE).await;
            continue;
        }

        for (seq, event_id, key, mut payload, attempts) in rows {
            if let Some(obj) = payload.as_object_mut() {
                obj.insert("event_id".into(), json!(event_id));
                obj.insert("seq".into(), json!(seq));
            }
            let body = serde_json::to_vec(&payload)?;
            let record = FutureRecord::to(TOPIC).key(key.as_bytes()).payload(&body);
            match producer.send(record, Timeout::After(Duration::from_secs(30))).await {
                Ok(_) => {
                    sqlx::query("UPDATE commerce_outbox SET published_at = NOW() WHERE seq = $1")
                        .bind(seq)
                        .execute(pool)
                        .await?;
                    metrics::counter!("themis_commerce_events_published_total").increment(1);
                }
                Err((e, _)) => {
                    let quarantine = permanent(&e) && attempts + 1 >= MAX_REFUSALS;
                    sqlx::query(
                        "UPDATE commerce_outbox
                            SET attempts = attempts + 1, last_error = $2,
                                blocked_at = CASE WHEN $3 THEN NOW() END,
                                blocked_reason = CASE WHEN $3 THEN $2 END
                          WHERE seq = $1",
                    )
                    .bind(seq)
                    .bind(e.to_string())
                    .bind(quarantine)
                    .execute(pool)
                    .await?;
                    tracing::warn!(seq, error = %e, quarantined = quarantine, "commerce.events publish refused; holding the queue");
                    // Stop at the first refusal: running ahead would publish
                    // later decisions about a subject before earlier ones.
                    tokio::time::sleep(Duration::from_secs(2)).await;
                    break;
                }
            }
        }
    }
}
