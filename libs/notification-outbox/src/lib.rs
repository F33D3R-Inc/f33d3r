//! Durable notification delivery from a Rust brain to Herald.
//!
//! A brain writes one row with [`INSERT_SQL`], in the same transaction as the action
//! when it has one. A background drain publishes rows to `notification.events` in
//! order through the Sitra Achra HTTP proxy, and marks a row only after the broker
//! answers with an offset. An outage holds the queue and loses nothing. A row the
//! broker refuses repeatedly is quarantined, so it cannot stop the rows behind it.
//!
//! The drain has its own Postgres client, because brains use different sqlx versions.
//!
//! The drain also reports its backlog as metrics, and asks Herald whether every event
//! it delivered was stored, publishing any that were not again a limited number of times.

use std::time::{Duration, Instant};

use serde_json::{json, Value};
use tokio_postgres::{Client, NoTls};
use tracing::{error, info, warn};

/// Binds: event_id, recipient_pial, event JSON text.
pub const INSERT_SQL: &str = "INSERT INTO notification_outbox (event_id, recipient_pial, event) \
     VALUES ($1, $2, $3::jsonb) ON CONFLICT (event_id) DO NOTHING";

const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS notification_outbox (
    id             BIGSERIAL   PRIMARY KEY,
    event_id       TEXT        NOT NULL UNIQUE,
    recipient_pial TEXT        NOT NULL,
    event          JSONB       NOT NULL,
    attempts       INT         NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at   TIMESTAMPTZ,
    blocked_at     TIMESTAMPTZ,
    blocked_reason TEXT
);
CREATE INDEX IF NOT EXISTS idx_notification_outbox_pending
    ON notification_outbox(id) WHERE delivered_at IS NULL AND blocked_at IS NULL;
ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS reconciled_at TIMESTAMPTZ;
ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS reconcile_attempts INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_notification_outbox_reconcile
    ON notification_outbox(delivered_at) WHERE delivered_at IS NOT NULL AND reconciled_at IS NULL;
";

const TOPIC: &str = "notification.events";
/// Must match the partition count `sitra-init` creates the topic with.
const PARTITIONS: u32 = 12;
const BATCH: i64 = 100;
const TICK: Duration = Duration::from_millis(500);
const MAX_REFUSALS: i32 = 12;
/// Distinct from every other advisory key in the estate.
const LOCK_KEY: i64 = 7331_0006;
const GAUGE_INTERVAL: Duration = Duration::from_secs(15);
const RECONCILE_INTERVAL: Duration = Duration::from_secs(60);
const RECONCILE_BATCH: i64 = 1000;
const MAX_REPUBLISH: i32 = 3;

/// Who gets a notification and what it says.
pub struct Event {
    /// Derived from the action, so a retried action cannot notify twice.
    pub event_id: String,
    /// The persona account, when the brain knows it. Otherwise Herald uses the
    /// person's primary account.
    pub recipient_account: Option<String>,
    pub recipient_pial: String,
    pub actor_account: Option<String>,
    pub kind: String,
    pub target_id: String,
    pub target_type: String,
    pub payload: Value,
    pub push_title: String,
    pub push_body: String,
    pub push_url: String,
}

impl Event {
    /// The JSON Herald consumes. An event with no push title is inbox only.
    pub fn to_json(&self) -> String {
        let mut event = json!({
            "event_id": self.event_id,
            "recipient_pial": self.recipient_pial,
            "kind": self.kind,
            "target_id": self.target_id,
            "target_type": self.target_type,
            "payload": self.payload,
            "channel": if self.push_title.is_empty() { "inbox" } else { "push_inbox" },
        });
        if !self.push_title.is_empty() {
            event["push"] = json!({
                "title": self.push_title,
                "body": self.push_body,
                "action_url": self.push_url,
            });
        }
        if let Some(account) = &self.recipient_account {
            event["recipient_account"] = json!(account);
        }
        if let Some(actor) = &self.actor_account {
            event["actor_account"] = json!(actor);
        }
        event.to_string()
    }
}

/// Starts the drain for the life of the process.
pub fn spawn(database_url: String) {
    tokio::spawn(async move {
        let proxy = std::env::var("SITRA_PROXY_URL").unwrap_or_else(|_| "http://sitra-achra:8082".into());
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(10))
            .build()
            .expect("http client");
        let mut backoff = Duration::from_secs(1);
        loop {
            match run(&database_url, &proxy, &http).await {
                Ok(()) => backoff = Duration::from_secs(1),
                Err(e) => {
                    error!(error = %e, "notification outbox drain failed; retrying");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
            }
        }
    });
}

async fn run(database_url: &str, proxy: &str, http: &reqwest::Client) -> anyhow::Result<()> {
    let (client, connection) = tokio_postgres::connect(database_url, NoTls).await?;
    tokio::spawn(async move {
        if let Err(e) = connection.await {
            warn!(error = %e, "notification outbox connection ended");
        }
    });
    client.batch_execute(SCHEMA).await?;
    info!("notification outbox drain started");
    let herald = Herald::from_env(http);
    let mut last_gauges = Instant::now() - GAUGE_INTERVAL;
    let mut last_reconcile = Instant::now();
    loop {
        // One drain at a time, or two replicas would interleave a person's events.
        let locked: bool = client
            .query_one("SELECT pg_try_advisory_lock($1)", &[&LOCK_KEY])
            .await?
            .get(0);
        if locked {
            let mut outcome = drain_batch(&client, proxy, http).await;
            if outcome.is_ok() && last_reconcile.elapsed() >= RECONCILE_INTERVAL {
                last_reconcile = Instant::now();
                if let Err(e) = reconcile(&client, &herald).await {
                    warn!(error = %e, "notification reconcile failed; will retry");
                }
            }
            if outcome.is_ok() && last_gauges.elapsed() >= GAUGE_INTERVAL {
                last_gauges = Instant::now();
                outcome = report_gauges(&client).await;
            }
            client
                .execute("SELECT pg_advisory_unlock($1)", &[&LOCK_KEY])
                .await?;
            outcome?;
        }
        tokio::time::sleep(TICK).await;
    }
}

async fn report_gauges(client: &Client) -> anyhow::Result<()> {
    let row = client
        .query_one(
            "SELECT COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(created_at) FILTER (WHERE blocked_at IS NULL)), 0)::float8,
                    COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)::float8
               FROM notification_outbox WHERE delivered_at IS NULL",
            &[],
        )
        .await?;
    metrics::gauge!("notification_outbox_oldest_seconds").set(row.get::<_, f64>(0));
    metrics::gauge!("notification_outbox_blocked").set(row.get::<_, f64>(1));
    Ok(())
}

struct Herald<'a> {
    http: &'a reqwest::Client,
    url: String,
    key: String,
}

impl<'a> Herald<'a> {
    fn from_env(http: &'a reqwest::Client) -> Self {
        Self {
            http,
            url: std::env::var("HERALD_URL").unwrap_or_else(|_| "http://herald:8105".into()),
            key: std::env::var("INTERNAL_API_KEY").unwrap_or_default(),
        }
    }

    async fn missing(&self, event_ids: &[String]) -> anyhow::Result<Vec<String>> {
        if self.key.is_empty() {
            anyhow::bail!("INTERNAL_API_KEY is not set");
        }
        let answer: Value = self
            .http
            .post(format!("{}/v1/inbox/missing", self.url))
            .header("x-internal-key", &self.key)
            .json(&json!({ "event_ids": event_ids }))
            .send()
            .await?
            .error_for_status()?
            .json()
            .await?;
        Ok(serde_json::from_value(answer["missing"].clone())?)
    }
}

/// Asks Herald which delivered events it has not stored and publishes those again.
/// Herald stores by event ID, so a repeat is harmless.
async fn reconcile(client: &Client, herald: &Herald<'_>) -> anyhow::Result<()> {
    let rows = client
        .query(
            "SELECT event_id, reconcile_attempts FROM notification_outbox
              WHERE delivered_at IS NOT NULL AND reconciled_at IS NULL
                AND delivered_at < NOW() - INTERVAL '2 minutes'
              ORDER BY delivered_at LIMIT $1",
            &[&RECONCILE_BATCH],
        )
        .await?;
    if rows.is_empty() {
        metrics::gauge!("notification_reconcile_missing").set(0.0);
        return Ok(());
    }
    let ids: Vec<String> = rows.iter().map(|r| r.get(0)).collect();
    let missing = herald.missing(&ids).await?;
    metrics::gauge!("notification_reconcile_missing").set(missing.len() as f64);

    let (mut republish, mut settled) = (Vec::new(), Vec::new());
    for row in &rows {
        let id: String = row.get(0);
        let attempts: i32 = row.get(1);
        if !missing.contains(&id) {
            settled.push(id);
        } else if attempts < MAX_REPUBLISH {
            republish.push(id);
        } else {
            error!(event_id = %id, "Herald never stored this notification after republishing");
            metrics::counter!("notification_reconcile_lost_total").increment(1);
            settled.push(id);
        }
    }
    client
        .execute(
            "UPDATE notification_outbox SET reconciled_at = NOW() WHERE event_id = ANY($1)",
            &[&settled],
        )
        .await?;
    client
        .execute(
            "UPDATE notification_outbox
                SET delivered_at = NULL, reconcile_attempts = reconcile_attempts + 1
              WHERE event_id = ANY($1)",
            &[&republish],
        )
        .await?;
    Ok(())
}

fn partition_for(pial: &str) -> u32 {
    pial.bytes().fold(2166136261u32, |h, b| (h ^ b as u32).wrapping_mul(16777619)) % PARTITIONS
}

async fn drain_batch(client: &Client, proxy: &str, http: &reqwest::Client) -> anyhow::Result<()> {
    let rows = client
        .query(
            "SELECT id, recipient_pial, event::text FROM notification_outbox
              WHERE delivered_at IS NULL AND blocked_at IS NULL ORDER BY id LIMIT $1",
            &[&BATCH],
        )
        .await?;
    for row in rows {
        let id: i64 = row.get(0);
        let pial: String = row.get(1);
        let event: String = row.get(2);
        let value: Value = serde_json::from_str(&event)?;
        let body = json!({ "records": [{ "value": value, "partition": partition_for(&pial) }] });

        // An outage says nothing about the row: keep it, stop the batch, try later.
        let resp = match http
            .post(format!("{proxy}/topics/{TOPIC}"))
            .header("Content-Type", "application/vnd.kafka.json.v2+json")
            .json(&body)
            .send()
            .await
        {
            Ok(r) => r,
            Err(e) => {
                warn!(error = %e, "sitra proxy unreachable; holding notifications");
                return Ok(());
            }
        };
        let status = resp.status();
        let answer: Value = resp.json().await.unwrap_or(Value::Null);
        let offset_ok = answer["offsets"][0]["offset"].as_i64().is_some()
            && answer["offsets"][0]["error_code"].as_i64().is_none();
        if status.is_success() && offset_ok {
            client
                .execute("UPDATE notification_outbox SET delivered_at = NOW() WHERE id = $1", &[&id])
                .await?;
            continue;
        }
        if status.is_server_error() {
            warn!(status = status.as_u16(), "sitra proxy failing; holding notifications");
            return Ok(());
        }
        // The broker answered and refused this row.
        let reason = answer.to_string();
        let attempts: i32 = client
            .query_one(
                "UPDATE notification_outbox SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts",
                &[&id],
            )
            .await?
            .get(0);
        if attempts >= MAX_REFUSALS {
            error!(id, reason = %reason, "notification quarantined");
            client
                .execute(
                    "UPDATE notification_outbox SET blocked_at = NOW(), blocked_reason = $2 WHERE id = $1",
                    &[&id, &reason],
                )
                .await?;
            continue;
        }
        warn!(id, attempts, reason = %reason, "broker refused notification; will retry");
        return Ok(());
    }
    Ok(())
}
