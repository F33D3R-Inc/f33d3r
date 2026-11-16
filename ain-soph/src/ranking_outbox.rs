//! ranking.events — the payment signals AethyrRank learns from, durable.
//!
//! A tip or a subscription is a ranking signal: the payer wants more of the
//! creator they paid. AethyrRank's one intake is Sitra Achra `ranking.events`
//! (aethyrrank-engine/src/learning/signal.rs); the reward is its taxonomy's,
//! not a number sent from here.
//!
//! The signal is a row in `ranking_outbox`, written in the SAME transaction as
//! the payment, so a payment and its signal exist together or not at all. The
//! relay below publishes rows in order through the Sitra Achra HTTP proxy and
//! marks a row only once the broker has answered with an offset. An outage or a
//! restart holds the queue and loses nothing; a crash between publish and mark
//! republishes the same event_id, which the consumer dedupes on. A record the
//! broker refuses outright, repeatedly, is quarantined so the rows behind it
//! move, and reported on every pass until a person looks.

use std::time::Duration;

use serde_json::{json, Value};
use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

pub const TOPIC: &str = "ranking.events";
/// Distinct from every other advisory key in the estate.
const LOCK_KEY: i64 = 7331_0102;
const BATCH: i64 = 100;
const IDLE: Duration = Duration::from_millis(500);
const MAX_REFUSALS: i32 = 12;

pub const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS ranking_outbox (
    seq            BIGSERIAL   PRIMARY KEY,
    event_id       UUID        NOT NULL UNIQUE,
    partition_key  TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts       INT         NOT NULL DEFAULT 0,
    last_error     TEXT,
    published_at   TIMESTAMPTZ,
    blocked_at     TIMESTAMPTZ,
    blocked_reason TEXT
);
CREATE INDEX IF NOT EXISTS idx_ranking_outbox_pending ON ranking_outbox (seq)
    WHERE published_at IS NULL AND blocked_at IS NULL;
"#;

/// The ranking action a payment's meta names, if it is one.
fn action_for(meta: &str) -> Option<&'static str> {
    match meta.to_lowercase().as_str() {
        "subscription" | "subscribe" => Some("subscription"),
        "tip" => Some("tip"),
        _ => None,
    }
}

/// Queues the payment's ranking signal inside the payment's own transaction.
/// A payment that is not a ranking signal queues nothing.
pub async fn enqueue(
    tx: &mut Transaction<'_, Postgres>,
    from_pial: &str,
    to_pial: &str,
    meta: &str,
) -> Result<(), sqlx::Error> {
    let Some(action) = action_for(meta) else {
        return Ok(());
    };
    let event_id = Uuid::new_v4();
    let now = chrono::Utc::now();
    let payload = json!({
        "event_id":     event_id,
        "source":       "ain-soph",
        "viewer_pial":  from_pial,
        "creator_pial": to_pial,
        "action":       action,
        "occurred_at":  now,
        "received_at":  now,
    });
    sqlx::query("INSERT INTO ranking_outbox (event_id, partition_key, payload) VALUES ($1, $2, $3)")
        .bind(event_id)
        .bind(from_pial)
        .bind(payload)
        .execute(&mut **tx)
        .await?;
    Ok(())
}

pub fn spawn_relay(pool: PgPool, proxy: String) {
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            match relay(&pool, &proxy).await {
                Ok(()) => backoff = Duration::from_secs(1),
                Err(e) => {
                    tracing::error!(error = %e, "ranking.events relay stopped; restarting");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
            }
        }
    });
}

enum Publish {
    Confirmed,
    /// Worth retrying: the broker or the proxy may answer next time.
    Retry(String),
    /// The proxy refused the record itself.
    Refused(String),
}

async fn publish(client: &reqwest::Client, proxy: &str, key: &str, value: &Value) -> Publish {
    let body = json!({ "records": [{ "key": key, "value": value }] });
    let resp = match client
        .post(format!("{proxy}/topics/{TOPIC}"))
        .header("Content-Type", "application/vnd.kafka.json.v2+json")
        .json(&body)
        .send()
        .await
    {
        Ok(r) => r,
        Err(e) => return Publish::Retry(e.to_string()),
    };
    let status = resp.status();
    let answer: Value = match resp.json().await {
        Ok(v) => v,
        Err(e) => return Publish::Retry(format!("status {status}: unreadable answer: {e}")),
    };
    let confirmed = answer["offsets"][0]["offset"].as_i64().is_some()
        && answer["offsets"][0]["error_code"].as_i64().is_none();
    if status.is_success() && confirmed {
        return Publish::Confirmed;
    }
    let why = format!("status {status}: {answer}");
    if status.is_client_error()
        && status != reqwest::StatusCode::REQUEST_TIMEOUT
        && status != reqwest::StatusCode::TOO_MANY_REQUESTS
    {
        Publish::Refused(why)
    } else {
        Publish::Retry(why)
    }
}

async fn relay(pool: &PgPool, proxy: &str) -> anyhow::Result<()> {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()?;

    let mut lock_conn = pool.acquire().await?.detach();
    loop {
        let held: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
            .bind(LOCK_KEY)
            .fetch_one(&mut lock_conn)
            .await?;
        if held {
            break;
        }
        // Another Ain Soph holds the relay. Order needs exactly one.
        tokio::time::sleep(Duration::from_secs(5)).await;
    }
    tracing::info!("ranking.events relay holds its lock");

    let mut last_report = std::time::Instant::now() - Duration::from_secs(120);
    loop {
        // The lock lives on this connection: if it is gone, so is the lock.
        sqlx::query("SELECT 1").execute(&mut lock_conn).await?;

        if last_report.elapsed() >= Duration::from_secs(60) {
            last_report = std::time::Instant::now();
            let (pending, blocked): (i64, i64) = sqlx::query_as(
                "SELECT COUNT(*) FILTER (WHERE blocked_at IS NULL), COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)
                   FROM ranking_outbox WHERE published_at IS NULL",
            )
            .fetch_one(pool)
            .await?;
            metrics::gauge!("ain_soph_ranking_outbox_pending").set(pending as f64);
            metrics::gauge!("ain_soph_ranking_outbox_quarantined").set(blocked as f64);
            if blocked > 0 {
                tracing::error!(
                    quarantined = blocked,
                    "INCIDENT: ranking.events records quarantined — payment signals not delivered. \
                     SELECT seq, blocked_reason FROM ranking_outbox WHERE blocked_at IS NOT NULL AND published_at IS NULL"
                );
            }
        }

        let rows: Vec<(i64, String, Value, i32)> = sqlx::query_as(
            "SELECT seq, partition_key, payload, attempts FROM ranking_outbox
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

        for (seq, key, payload, attempts) in rows {
            let (why, quarantine) = match publish(&client, proxy, &key, &payload).await {
                Publish::Confirmed => {
                    sqlx::query("UPDATE ranking_outbox SET published_at = NOW() WHERE seq = $1")
                        .bind(seq)
                        .execute(pool)
                        .await?;
                    metrics::counter!("ain_soph_ranking_signals_sent_total").increment(1);
                    continue;
                }
                Publish::Refused(w) => (w, attempts + 1 >= MAX_REFUSALS),
                Publish::Retry(w) => (w, false),
            };
            sqlx::query(
                "UPDATE ranking_outbox
                    SET attempts = attempts + 1, last_error = $2,
                        blocked_at = CASE WHEN $3 THEN NOW() END,
                        blocked_reason = CASE WHEN $3 THEN $2 END
                  WHERE seq = $1",
            )
            .bind(seq)
            .bind(&why)
            .bind(quarantine)
            .execute(pool)
            .await?;
            tracing::warn!(seq, error = %why, quarantined = quarantine, "ranking.events publish not confirmed; holding the queue");
            // Stop at the first refusal so signals keep their order.
            tokio::time::sleep(Duration::from_secs(2)).await;
            break;
        }
    }
}
