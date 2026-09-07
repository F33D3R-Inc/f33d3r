//! Publishes `frequency_events` rows to Sitra Achra, in order, and marks them.
//!
//! One drain runs at a time across every Auralis node (a session advisory
//! lock, the same device libs/manhattan-outbox uses), because two drains
//! would interleave one Frequency's events. Inside a tick, rows are taken in
//! `seq` order; the first row the broker refuses stops the batch, so nothing
//! behind it is published out of order. The next tick starts where this one
//! stopped.
//!
//! A row is marked published only after the broker acknowledged it
//! (`acks=all`). A crash between ack and mark republishes that row once —
//! at-least-once, which is the contract every consumer already honours by
//! deduplicating on event_id.

use std::time::Duration;

use metrics::{counter, gauge, histogram};
use rdkafka::config::ClientConfig;
use rdkafka::producer::{FutureProducer, FutureRecord};
use rdkafka::util::Timeout;
use sqlx::PgPool;
use tracing::{error, info, warn};

use super::schemas::Event;
use super::TOPIC;
use crate::telemetry::metrics as m;

/// Fixed and arbitrary; distinct from every other advisory key in the estate
/// (feed-engine 7331_0001, Manhattan 7331_0002, our migrator 7331_0003,
/// manhattan-outbox "mhoutbox").
const DRAIN_LOCK_KEY: i64 = 7331_0004;
const BATCH: i64 = 200;
const DELIVERY_TIMEOUT: Duration = Duration::from_secs(10);

pub struct Drain {
    pool: PgPool,
    producer: FutureProducer,
    interval: Duration,
}

impl Drain {
    pub fn new(pool: PgPool, brokers: &str, interval: Duration) -> anyhow::Result<Self> {
        let producer: FutureProducer = ClientConfig::new()
            .set("bootstrap.servers", brokers)
            .set("acks", "all")
            .set("enable.idempotence", "true")
            .set("message.timeout.ms", "10000")
            .set("request.timeout.ms", "5000")
            .set("retries", "3")
            .create()?;
        Ok(Self {
            pool,
            producer,
            interval,
        })
    }

    /// Runs for the life of the process. Errors are logged and retried with
    /// backoff; a broker outage shows up as a growing
    /// `frequency_events_unpublished` gauge, never as a silent drop.
    pub fn spawn(self) {
        tokio::spawn(async move {
            let mut backoff = self.interval;
            loop {
                match self.tick().await {
                    Ok(published) => {
                        backoff = self.interval;
                        if published == 0 {
                            tokio::time::sleep(self.interval).await;
                        }
                    }
                    Err(e) => {
                        counter!(m::EVENT_PUBLISH_FAILURES_TOTAL).increment(1);
                        error!(error = %e, "event drain tick failed; backing off");
                        tokio::time::sleep(backoff).await;
                        backoff = (backoff * 2).min(Duration::from_secs(30));
                    }
                }
            }
        });
    }

    /// One pass. Returns how many rows were published.
    pub async fn tick(&self) -> anyhow::Result<usize> {
        let mut conn = self.pool.acquire().await?;

        let (locked,): (bool,) = sqlx::query_as("SELECT pg_try_advisory_lock($1)")
            .bind(DRAIN_LOCK_KEY)
            .fetch_one(&mut *conn)
            .await?;
        if !locked {
            // Another node is draining. The backlog gauge is still ours to
            // report, so the dashboard is right whichever node is looking.
            self.report_backlog(&mut conn).await?;
            return Ok(0);
        }

        let outcome = self.drain_locked(&mut conn).await;

        if let Err(e) = sqlx::query("SELECT pg_advisory_unlock($1)")
            .bind(DRAIN_LOCK_KEY)
            .execute(&mut *conn)
            .await
        {
            warn!(error = %e, "releasing event drain lock");
        }
        outcome
    }

    async fn report_backlog(&self, conn: &mut sqlx::PgConnection) -> anyhow::Result<()> {
        let (n,): (i64,) =
            sqlx::query_as("SELECT COUNT(*) FROM frequency_events WHERE published_at IS NULL")
                .fetch_one(conn)
                .await?;
        gauge!(m::EVENTS_UNPUBLISHED).set(n as f64);
        Ok(())
    }

    async fn drain_locked(&self, conn: &mut sqlx::PgConnection) -> anyhow::Result<usize> {
        let rows: Vec<Event> = sqlx::query_as(
            "SELECT event_id, event_type, schema_version, pial_id,
                    created_at AS timestamp, frequency_id, correlation_id, payload
               FROM frequency_events
              WHERE published_at IS NULL
              ORDER BY seq
              LIMIT $1",
        )
        .bind(BATCH)
        .fetch_all(&mut *conn)
        .await?;

        let mut published = 0usize;
        for ev in &rows {
            let key = ev.frequency_id.to_string();
            let body = ev.wire();
            let record = FutureRecord::to(TOPIC).key(&key).payload(&body);
            match self
                .producer
                .send(record, Timeout::After(DELIVERY_TIMEOUT))
                .await
            {
                Ok(_) => {
                    sqlx::query(
                        "UPDATE frequency_events
                            SET published_at = NOW(), publish_attempts = publish_attempts + 1,
                                last_error = NULL
                          WHERE event_id = $1",
                    )
                    .bind(ev.event_id)
                    .execute(&mut *conn)
                    .await?;
                    counter!(m::EVENTS_PUBLISHED_TOTAL, "type" => ev.event_type.clone())
                        .increment(1);
                    histogram!(m::EVENT_PROPAGATION_SECONDS).record(
                        (chrono::Utc::now() - ev.timestamp)
                            .num_milliseconds()
                            .max(0) as f64
                            / 1000.0,
                    );
                    published += 1;
                }
                Err((e, _)) => {
                    sqlx::query(
                        "UPDATE frequency_events
                            SET publish_attempts = publish_attempts + 1, last_error = $2
                          WHERE event_id = $1",
                    )
                    .bind(ev.event_id)
                    .bind(e.to_string())
                    .execute(&mut *conn)
                    .await?;
                    counter!(m::EVENT_PUBLISH_FAILURES_TOTAL).increment(1);
                    self.report_backlog(conn).await?;
                    // Order is the contract. Stop here; the next tick resumes
                    // from this row.
                    anyhow::bail!(
                        "publishing {} ({}) for frequency {}: {e}",
                        ev.event_type,
                        ev.event_id,
                        ev.frequency_id
                    );
                }
            }
        }

        self.report_backlog(conn).await?;
        if published > 0 {
            info!(published, "frequency events published");
        }
        Ok(published)
    }
}
