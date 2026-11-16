//! Publishes identity_outbox to the `identity.events` topic.
//!
//! The same delivery contract as libs/notification-outbox, whose Pandaproxy
//! producer this is a sibling of: rows go out in id order through the Sitra
//! Achra HTTP proxy, a row is marked delivered only after the broker answers
//! with an offset, an outage holds the queue and loses nothing, and a row the
//! broker refuses repeatedly is quarantined so it cannot stop the rows behind
//! it. A quarantined row is an identity change the estate has not heard about,
//! so it is counted on /health and in a gauge until a person clears it:
//!
//!   UPDATE identity_outbox SET blocked_at = NULL, blocked_reason = NULL, attempts = 0
//!    WHERE id = $1;
//!
//! It is in-process rather than the shared crate because that crate is bound to
//! notification.events and Herald's reconcile; this brain's own pool and sqlx
//! do the work here.

use std::time::Duration;

use anyhow::{Context, Result};
use serde_json::{json, Value};
use sqlx::PgPool;
use tracing::{error, warn};

pub const TOPIC: &str = "identity.events";
/// Must match the partition count sitra-init creates the topic with.
const PARTITIONS: u32 = 12;
const BATCH: i64 = 100;
const TICK: Duration = Duration::from_millis(250);
const MAX_REFUSALS: i32 = 12;
/// f33d3r_security advisory keys: 7331_0004 manhattan drain, 7331_0006
/// notification drain, 7331_0010 migrator, 7331_0011 this relay.
const LOCK_KEY: i64 = 7331_0011;

/// FNV-1a over the identity, as notification-outbox partitions by PIAL.
fn partition_for(key: &str) -> u32 {
    key.bytes()
        .fold(2166136261u32, |h, b| (h ^ b as u32).wrapping_mul(16777619))
        % PARTITIONS
}

/// Starts the relay for the life of the process.
pub fn spawn(pool: PgPool, http: reqwest::Client) {
    let proxy = std::env::var("SITRA_PROXY_URL").unwrap_or_else(|_| "http://sitra-achra:8082".into());
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            match tick(&pool, &http, &proxy).await {
                Ok(()) => {
                    backoff = Duration::from_secs(1);
                    tokio::time::sleep(TICK).await;
                }
                Err(e) => {
                    error!(error = ?e, "identity.events relay failed; retrying");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
            }
        }
    });
}

async fn tick(pool: &PgPool, http: &reqwest::Client, proxy: &str) -> Result<()> {
    let mut conn = pool.acquire().await.context("relay: acquire")?;
    // One relay at a time, or two replicas would interleave an identity's events.
    let locked: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
        .bind(LOCK_KEY)
        .fetch_one(&mut *conn)
        .await?;
    if !locked {
        return Ok(());
    }
    let outcome = drain(&mut conn, http, proxy).await;
    let gauges = report_gauges(&mut conn).await;
    sqlx::query("SELECT pg_advisory_unlock($1)")
        .bind(LOCK_KEY)
        .execute(&mut *conn)
        .await?;
    outcome?;
    gauges
}

async fn report_gauges(conn: &mut sqlx::PgConnection) -> Result<()> {
    let (oldest, blocked): (f64, f64) = sqlx::query_as(
        "SELECT COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(created_at) FILTER (WHERE blocked_at IS NULL)), 0)::float8,
                COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)::float8
           FROM identity_outbox WHERE delivered_at IS NULL",
    )
    .fetch_one(conn)
    .await?;
    metrics::gauge!("identity_outbox_oldest_seconds").set(oldest);
    metrics::gauge!("identity_outbox_blocked").set(blocked);
    Ok(())
}

async fn drain(conn: &mut sqlx::PgConnection, http: &reqwest::Client, proxy: &str) -> Result<()> {
    let rows: Vec<(i64, String, String, Value)> = sqlx::query_as(
        "SELECT id, partition_key, record_key, event FROM identity_outbox
          WHERE delivered_at IS NULL AND blocked_at IS NULL ORDER BY id LIMIT $1",
    )
    .bind(BATCH)
    .fetch_all(&mut *conn)
    .await?;

    for (id, partition_key, record_key, mut event) in rows {
        event["seq"] = json!(id);
        let body = json!({ "records": [{
            "key": record_key,
            "value": event,
            "partition": partition_for(&partition_key),
        }]});

        // An outage says nothing about the row: keep it, stop, try next tick.
        let resp = match http
            .post(format!("{proxy}/topics/{TOPIC}"))
            .header("Content-Type", "application/vnd.kafka.json.v2+json")
            .json(&body)
            .send()
            .await
        {
            Ok(r) => r,
            Err(e) => {
                warn!(error = %e, "sitra proxy unreachable; holding identity events");
                return Ok(());
            }
        };
        let status = resp.status();
        let text = resp.text().await.unwrap_or_else(|e| format!("<unreadable body: {e}>"));
        let answer: Value = serde_json::from_str(&text).unwrap_or(Value::Null);
        let offset_ok = answer["offsets"][0]["offset"].as_i64().is_some()
            && answer["offsets"][0]["error_code"].as_i64().is_none();
        if status.is_success() && offset_ok {
            sqlx::query("UPDATE identity_outbox SET delivered_at = NOW() WHERE id = $1")
                .bind(id)
                .execute(&mut *conn)
                .await?;
            continue;
        }
        if status.is_server_error() {
            warn!(status = status.as_u16(), "sitra proxy failing; holding identity events");
            return Ok(());
        }
        // The broker answered and refused this row.
        let reason = format!("HTTP {}: {text}", status.as_u16());
        let attempts: i32 = sqlx::query_scalar(
            "UPDATE identity_outbox SET attempts = attempts + 1, last_error = $2
              WHERE id = $1 RETURNING attempts",
        )
        .bind(id)
        .bind(&reason)
        .fetch_one(&mut *conn)
        .await?;
        if attempts >= MAX_REFUSALS {
            error!(id, reason = %reason, "identity event quarantined — the estate has not heard about this change");
            sqlx::query(
                "UPDATE identity_outbox SET blocked_at = NOW(), blocked_reason = $2 WHERE id = $1",
            )
            .bind(id)
            .bind(&reason)
            .execute(&mut *conn)
            .await?;
            continue;
        }
        warn!(id, attempts, reason = %reason, "broker refused identity event; will retry");
        return Ok(());
    }
    Ok(())
}

/// What /health reports about this outbox.
pub struct Backlog {
    pub pending: i64,
    pub oldest_age_seconds: Option<f64>,
    pub quarantined: i64,
}

pub async fn backlog(pool: &PgPool) -> Result<Backlog> {
    let (pending, oldest, quarantined): (i64, Option<f64>, i64) = sqlx::query_as(
        "SELECT COUNT(*)::BIGINT,
                EXTRACT(EPOCH FROM (NOW() - MIN(created_at)))::DOUBLE PRECISION,
                COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)::BIGINT
           FROM identity_outbox WHERE delivered_at IS NULL",
    )
    .fetch_one(pool)
    .await?;
    Ok(Backlog {
        pending,
        oldest_age_seconds: oldest,
        quarantined,
    })
}
