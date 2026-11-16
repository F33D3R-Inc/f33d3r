//! The ranking.events consumer: the one way the bandit learns.
//!
//! ## Exactly once
//!
//! Offsets are not committed to Kafka. They are stored in f33d3r_rank, in the
//! same transaction as everything the batch applied (models, standing
//! reactions, applied event ids). Boot restores the models and assigns every
//! partition at its stored offset, so the database is always "every event
//! before these offsets, applied once" — a crash anywhere replays exactly the
//! events whose effects were not committed.
//!
//! ## One owner
//!
//! One consumer instance owns the topic: it holds pg advisory lock 7331_0041
//! on a dedicated connection while it consumes, and assigns every partition
//! itself (no consumer-group balancing). A second instance waits for the lock
//! and serves /rank meanwhile. The lock is not what keeps the state exact: each
//! batch writes its offsets compare-and-set against the offsets it started
//! from, so an instance that lost ownership without noticing fails its next
//! commit, releases everything it held, and goes back to waiting.
//!
//! ## Failures
//!
//! A message that can never be applied (not JSON, an unknown action, a missing
//! field) goes to ranking.events.dead with the reason, and its offset moves on.
//! Any other failure — the database, the dead topic — retries the whole batch
//! with backoff and never skips it: a held partition is visible, a skipped
//! signal is not.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{anyhow, Context, Result};
use rdkafka::config::ClientConfig;
use rdkafka::consumer::{Consumer, StreamConsumer};
use rdkafka::message::{Message, OwnedMessage};
use rdkafka::producer::{FutureProducer, FutureRecord};
use rdkafka::{Offset, TopicPartitionList};
use sqlx::{Connection, PgPool};
use tracing::{error, info, warn};

use crate::learning::apply::{apply, Outcome, Working};
use crate::learning::features::FeatureCache;
use crate::learning::signal::{RankingSignal, DEAD_TOPIC, TOPIC};
use crate::model_store::ModelStore;
use crate::persist;

/// Ownership of ranking.events. Distinct from every other advisory key in the estate.
const OWNER_LOCK_KEY: i64 = 7331_0041;
const BATCH_MAX: usize = 256;
const BATCH_WINDOW: Duration = Duration::from_millis(50);
const MAX_BACKOFF: Duration = Duration::from_secs(30);
const PRODUCE_TIMEOUT: Duration = Duration::from_secs(10);
const OWNER_RETRY: Duration = Duration::from_secs(15);
const PRUNE_EVERY: Duration = Duration::from_secs(3600);

#[derive(Clone)]
pub struct Intake {
    pub pool: PgPool,
    pub brokers: String,
    pub store: Arc<ModelStore>,
    pub cache: Arc<FeatureCache>,
}

enum BatchError {
    /// Another instance moved the offsets: this one no longer owns the topic.
    Fenced(String),
    Retry(anyhow::Error),
}

impl From<anyhow::Error> for BatchError {
    fn from(e: anyhow::Error) -> Self {
        BatchError::Retry(e)
    }
}

/// Start the consumer. It runs for the life of the process.
pub fn spawn(intake: Intake) {
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            match own_and_consume(&intake).await {
                Ok(()) => backoff = Duration::from_secs(1),
                Err(e) => {
                    error!(error = %format!("{e:#}"), "ranking.events consumer stopped; restarting");
                    metrics::counter!("aethyrrank_consumer_restarts_total").increment(1);
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(MAX_BACKOFF);
                }
            }
        }
    });
}

async fn own_and_consume(intake: &Intake) -> Result<()> {
    // The lock lives as long as this connection, which is never returned to the pool.
    let mut owner = intake
        .pool
        .acquire()
        .await
        .context("acquiring the ownership connection")?
        .detach();
    loop {
        let owned: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
            .bind(OWNER_LOCK_KEY)
            .fetch_one(&mut owner)
            .await
            .context("taking the ranking.events ownership lock")?;
        if owned {
            break;
        }
        info!("another AethyrRank instance owns ranking.events; serving /rank and waiting");
        tokio::time::sleep(OWNER_RETRY).await;
    }
    metrics::gauge!("aethyrrank_consumer_owner").set(1.0);

    let outcome = consume(intake).await;

    metrics::gauge!("aethyrrank_consumer_owner").set(0.0);
    if let Err(e) = owner.close().await {
        warn!(error = %e, "closing the ownership connection");
    }
    outcome
}

async fn consume(intake: &Intake) -> Result<()> {
    // Owned now: the stored state is the authority, and nothing else writes it.
    let restored = persist::restore_all(&intake.pool, &intake.store).await?;
    let mut positions = persist::stored_offsets(&intake.pool, TOPIC).await?;
    info!(models = restored, partitions_stored = positions.len(), "ranking state restored");

    let consumer: StreamConsumer = ClientConfig::new()
        .set("bootstrap.servers", &intake.brokers)
        // librdkafka requires a group id; it is never used to commit or balance.
        .set("group.id", "aethyrrank-ranking")
        .set("enable.auto.commit", "false")
        .set("enable.auto.offset.store", "false")
        .set("auto.offset.reset", "earliest")
        .create()
        .context("creating the ranking.events consumer")?;
    let dead: FutureProducer = ClientConfig::new()
        .set("bootstrap.servers", &intake.brokers)
        .set("acks", "all")
        .set("enable.idempotence", "true")
        .set("message.timeout.ms", "10000")
        .create()
        .context("creating the dead-letter producer")?;

    let partitions = tokio::task::block_in_place(|| {
        consumer
            .fetch_metadata(Some(TOPIC), Duration::from_secs(10))
            .context("reading ranking.events metadata")
    })?
    .topics()
    .iter()
    .find(|t| t.name() == TOPIC)
    .map(|t| t.partitions().iter().map(|p| p.id()).collect::<Vec<i32>>())
    .unwrap_or_default();
    if partitions.is_empty() {
        return Err(anyhow!("{TOPIC} has no partitions; sitra-init declares it"));
    }

    let mut tpl = TopicPartitionList::new();
    for p in &partitions {
        let at = match positions.get(p) {
            Some(n) => Offset::Offset(*n),
            None => Offset::Beginning,
        };
        tpl.add_partition_offset(TOPIC, *p, at)
            .with_context(|| format!("assigning {TOPIC}[{p}]"))?;
    }
    consumer.assign(&tpl).context("assigning ranking.events")?;
    info!(topic = TOPIC, partitions = partitions.len(), "ranking.events consumer started");

    let mut last_prune = Instant::now() - PRUNE_EVERY;
    loop {
        let batch = collect(&consumer).await?;
        let mut backoff = Duration::from_millis(500);
        loop {
            match settle(intake, &dead, &batch, &mut positions).await {
                Ok(()) => break,
                Err(BatchError::Fenced(why)) => {
                    return Err(anyhow!("lost ownership of ranking.events: {why}"));
                }
                Err(BatchError::Retry(e)) => {
                    error!(error = %format!("{e:#}"), size = batch.len(), "ranking batch not applied; holding it and retrying");
                    metrics::counter!("aethyrrank_ranking_batch_failures_total").increment(1);
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(MAX_BACKOFF);
                }
            }
        }
        if last_prune.elapsed() >= PRUNE_EVERY {
            last_prune = Instant::now();
            match sqlx::query(
                "DELETE FROM ranking_events_applied WHERE applied_at < NOW() - INTERVAL '30 days'",
            )
            .execute(&intake.pool)
            .await
            {
                Ok(r) => info!(pruned = r.rows_affected(), "applied-event ids past 30 days pruned"),
                Err(e) => error!(error = %e, "pruning applied-event ids failed; retrying in an hour"),
            }
        }
    }
}

/// Wait for one message, then take whatever else arrives within the window.
async fn collect(consumer: &StreamConsumer) -> Result<Vec<OwnedMessage>> {
    let first = consumer.recv().await.context("reading ranking.events")?.detach();
    let mut batch = vec![first];
    let deadline = tokio::time::Instant::now() + BATCH_WINDOW;
    while batch.len() < BATCH_MAX {
        match tokio::time::timeout_at(deadline, consumer.recv()).await {
            Ok(m) => batch.push(m.context("reading ranking.events")?.detach()),
            Err(_) => break,
        }
    }
    Ok(batch)
}

async fn settle(
    intake: &Intake,
    dead: &FutureProducer,
    batch: &[OwnedMessage],
    positions: &mut HashMap<i32, i64>,
) -> Result<(), BatchError> {
    let mut tx = intake.pool.begin().await.context("opening the batch transaction")?;
    let mut working = Working::default();
    let mut next: HashMap<i32, i64> = HashMap::new();
    let mut dead_letters: Vec<(&OwnedMessage, String)> = Vec::new();
    let mut outcomes: Vec<(String, String, Outcome, f64, f64)> = Vec::new();

    for m in batch {
        let (p, o) = (m.partition(), m.offset());
        let from = next.get(&p).or(positions.get(&p)).copied();
        if from.is_some_and(|n| o < n) {
            continue; // before where this partition already stands
        }
        if let Some(n) = from.filter(|n| o > *n) {
            // Retention removed messages this consumer never read.
            error!(partition = p, expected = n, got = o, "ranking.events skipped ahead: signals expired before they were applied");
            metrics::counter!("aethyrrank_ranking_signals_expired_total").increment((o - n) as u64);
        }
        next.insert(p, o + 1);
        match RankingSignal::parse(m.payload().unwrap_or_default()) {
            Err(reason) => dead_letters.push((m, reason)),
            Ok(s) => {
                let out = apply(&mut tx, &intake.cache, &intake.store, &mut working, &s).await?;
                let now = chrono::Utc::now();
                let since = |t: chrono::DateTime<chrono::Utc>| (now - t).num_milliseconds().max(0) as f64 / 1000.0;
                outcomes.push((
                    format!("{:?}", s.action).to_lowercase(),
                    s.source.clone(),
                    out,
                    since(s.occurred_at),
                    since(s.received_at),
                ));
            }
        }
    }

    for (key, (model, updates)) in &working.models {
        persist::write_model(&mut tx, key, &model.a_matrix, &model.b_vector, *updates).await?;
    }

    // Compare-and-set: the offsets move from where this instance found them, or not at all.
    for (p, n) in &next {
        let moved = match positions.get(p) {
            Some(expected) => sqlx::query(
                "UPDATE ranking_consumer_offsets SET next_offset = $3, updated_at = NOW()
                  WHERE topic = $1 AND partition = $2 AND next_offset = $4",
            )
            .bind(TOPIC)
            .bind(*p)
            .bind(*n)
            .bind(*expected)
            .execute(&mut *tx)
            .await,
            None => sqlx::query(
                "INSERT INTO ranking_consumer_offsets (topic, partition, next_offset)
                 VALUES ($1, $2, $3) ON CONFLICT (topic, partition) DO NOTHING",
            )
            .bind(TOPIC)
            .bind(*p)
            .bind(*n)
            .execute(&mut *tx)
            .await,
        }
        .with_context(|| format!("moving the offset of {TOPIC}[{p}]"))?;
        if moved.rows_affected() != 1 {
            return Err(BatchError::Fenced(format!(
                "{TOPIC}[{p}] was moved by someone else (expected {:?})",
                positions.get(p)
            )));
        }
    }

    for (m, reason) in &dead_letters {
        let body = serde_json::json!({
            "reason": reason,
            "topic": TOPIC,
            "partition": m.partition(),
            "offset": m.offset(),
            "payload": String::from_utf8_lossy(m.payload().unwrap_or_default()),
        })
        .to_string();
        let key = m.key().unwrap_or_default();
        dead.send(FutureRecord::to(DEAD_TOPIC).key(key).payload(&body), PRODUCE_TIMEOUT)
            .await
            .map_err(|(e, _)| anyhow!("dead-lettering {TOPIC}[{}]@{}: {e}", m.partition(), m.offset()))?;
        error!(partition = m.partition(), offset = m.offset(), reason = %reason, "ranking signal dead-lettered");
    }

    tx.commit().await.context("committing the ranking batch")?;

    // Durable: now /rank may see it.
    for (key, (model, _)) in working.models {
        intake.store.install(&key, model.a_matrix, model.b_vector);
    }
    for work in &working.forgotten {
        intake.cache.forget_work(work);
    }
    positions.extend(next);
    for (action, source, out, since_action, since_recorded) in outcomes {
        metrics::counter!(
            "aethyrrank_ranking_signals_total",
            "action" => action, "source" => source.clone(), "outcome" => out.label()
        )
        .increment(1);
        // From the person acting, and from the producer recording it, to learned.
        metrics::histogram!("aethyrrank_ranking_signal_lag_seconds", "from" => "action", "source" => source.clone())
            .record(since_action);
        metrics::histogram!("aethyrrank_ranking_signal_lag_seconds", "from" => "recorded", "source" => source)
            .record(since_recorded);
    }
    metrics::counter!("aethyrrank_ranking_signals_dead_total").increment(dead_letters.len() as u64);
    Ok(())
}
