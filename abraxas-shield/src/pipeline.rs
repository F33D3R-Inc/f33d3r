//! The async pipeline's front door: content.events → durable intake.
//!
//! The consumer does exactly one thing per record: write it into Abraxas's
//! own database (the subject, its media, and a subject_scan job) in one
//! transaction, then commit the offset. All judging happens afterwards from
//! the job queue, so a worker outage never holds the topic and a slow scan
//! never costs group membership.
//!
//! The offset only ever moves past a record that is safely recorded:
//!   * intake succeeded              → commit.
//!   * the record can never be read  → it is written to intake_deadletter
//!     (unparseable, unroutable)       with the reason, THEN committed. It is
//!                                     kept, never skipped.
//!   * intake failed (database down) → the consumer SEEKS back to this
//!                                     record's offset and backs off; the next
//!                                     poll redelivers the same record. If the
//!                                     seek itself fails, the record is retried
//!                                     in place. Nothing behind it is committed.
//!
//! Intake is idempotent on (kind, id), so a redelivery after a crash between
//! intake and commit changes nothing.

use std::time::Duration;

use rdkafka::config::ClientConfig;
use rdkafka::consumer::{CommitMode, Consumer, StreamConsumer};
use rdkafka::message::{BorrowedMessage, Message};
use rdkafka::Offset;
use serde::Deserialize;
use serde_json::json;
use sha2::{Digest, Sha256};
use sqlx::PgPool;
use uuid::Uuid;

use crate::api::AppState;
use crate::media::asset_id_from_served_url;

pub const TOPIC_CONTENT: &str = "content.events";
const CONSUMER_GROUP: &str = "abraxas-shield";

/// One record on content.events. feed-engine publishes `post.created` (works,
/// tracks included) and `vision.created`.
#[derive(Debug, Deserialize)]
struct ContentEvent {
    #[serde(default)]
    event: String,
    #[serde(default)]
    event_id: Option<String>,
    #[serde(default)]
    work_id: String,
    /// A Vision's canonical `<author_pial>.<seq>` address.
    #[serde(default)]
    vision_id: String,
    #[serde(default)]
    pial_id: String,
    #[serde(default)]
    body: String,
    #[serde(default)]
    media_url: Option<String>,
    #[serde(default)]
    media_urls: Vec<String>,
    /// caeor asset ids of the media, when the publisher names them.
    #[serde(default)]
    asset_ids: Vec<String>,
    #[serde(default)]
    is_adult_creator: bool,
    #[serde(default)]
    account_created_at: Option<chrono::DateTime<chrono::Utc>>,
}

enum Intake {
    Taken,
    NotOurs,
    Dead(String),
}

fn is_vision_ref(id: &str) -> bool {
    match id.rsplit_once('.') {
        Some((pial, seq)) => Uuid::parse_str(pial).is_ok() && seq.parse::<i64>().map(|n| n > 0).unwrap_or(false),
        None => false,
    }
}

async fn intake(pool: &PgPool, payload: &str) -> sqlx::Result<Intake> {
    let ev: ContentEvent = match serde_json::from_str(payload) {
        Ok(e) => e,
        Err(e) => return Ok(Intake::Dead(format!("does not parse: {e}"))),
    };
    let (kind, id) = match ev.event.as_str() {
        "post.created" => ("work", ev.work_id.trim().to_string()),
        "vision.created" => ("vision", ev.vision_id.trim().to_string()),
        _ => return Ok(Intake::NotOurs),
    };
    match kind {
        "work" if Uuid::parse_str(&id).is_err() => return Ok(Intake::Dead(format!("work_id {id:?} is not a UUID"))),
        "vision" if !is_vision_ref(&id) => return Ok(Intake::Dead(format!("vision_id {id:?} is not <pial>.<seq>"))),
        _ => {}
    }
    let Ok(pial) = Uuid::parse_str(ev.pial_id.trim()) else {
        return Ok(Intake::Dead(format!("pial_id {:?} is not a UUID", ev.pial_id)));
    };

    let mut assets: Vec<Uuid> = Vec::new();
    let mut unresolved: Vec<String> = Vec::new();
    for a in &ev.asset_ids {
        match Uuid::parse_str(a.trim()) {
            Ok(u) => assets.push(u),
            Err(_) => unresolved.push(a.clone()),
        }
    }
    for url in ev.media_urls.iter().chain(ev.media_url.iter()).filter(|u| !u.trim().is_empty()) {
        match asset_id_from_served_url(url) {
            Some(u) => assets.push(u),
            None => unresolved.push(url.clone()),
        }
    }
    assets.sort();
    assets.dedup();
    unresolved.sort();
    unresolved.dedup();
    let body = ev.body.trim();
    let body_sha = (!body.is_empty()).then(|| hex::encode(Sha256::digest(body.as_bytes())));

    let mut tx = pool.begin().await?;
    let inserted = sqlx::query(
        "INSERT INTO subjects (kind, id, pial, body, body_sha256, is_adult_creator, account_created_at, unresolved_media, event_id)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
         ON CONFLICT (kind, id) DO NOTHING",
    )
    .bind(kind)
    .bind(&id)
    .bind(pial)
    .bind((!body.is_empty()).then_some(body))
    .bind(&body_sha)
    .bind(ev.is_adult_creator)
    .bind(ev.account_created_at)
    .bind(&unresolved)
    .bind(&ev.event_id)
    .execute(&mut *tx)
    .await?
    .rows_affected();
    if inserted == 1 {
        for a in &assets {
            sqlx::query("INSERT INTO subject_assets (subject_kind, subject_id, asset_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING")
                .bind(kind)
                .bind(&id)
                .bind(a)
                .execute(&mut *tx)
                .await?;
        }
        crate::jobs::enqueue(&mut tx, "subject_scan", &format!("{kind}:{id}"), json!({})).await?;
    }
    tx.commit().await?;
    Ok(Intake::Taken)
}

async fn dead_letter(pool: &PgPool, msg: &BorrowedMessage<'_>, payload: &str, reason: &str) -> sqlx::Result<()> {
    sqlx::query(
        "INSERT INTO intake_deadletter (topic, partition, \"offset\", payload, reason)
         VALUES ($1, $2, $3, $4, $5) ON CONFLICT (topic, partition, \"offset\") DO NOTHING",
    )
    .bind(msg.topic())
    .bind(msg.partition())
    .bind(msg.offset())
    .bind(payload)
    .bind(reason)
    .execute(pool)
    .await?;
    Ok(())
}

/// Handles one record. Ok(()) means it is safely recorded and may be committed.
async fn handle(st: &AppState, msg: &BorrowedMessage<'_>) -> sqlx::Result<()> {
    let payload = match msg.payload_view::<str>() {
        Some(Ok(s)) => s.to_string(),
        Some(Err(e)) => {
            let raw = msg.payload().map(|b| String::from_utf8_lossy(b).into_owned()).unwrap_or_default();
            return dead_letter(&st.pool, msg, &raw, &format!("payload is not UTF-8: {e}")).await;
        }
        None => return dead_letter(&st.pool, msg, "", "empty payload").await,
    };
    match intake(&st.pool, &payload).await? {
        Intake::Taken => {
            metrics::counter!("abraxas_intake_total", "outcome" => "taken").increment(1);
            crate::jobs::wake(&st.jobs_wake);
            Ok(())
        }
        Intake::NotOurs => Ok(()),
        Intake::Dead(reason) => {
            tracing::error!(partition = msg.partition(), offset = msg.offset(), reason = %reason,
                "content event cannot be taken in; dead-lettered");
            metrics::counter!("abraxas_intake_total", "outcome" => "deadletter").increment(1);
            dead_letter(&st.pool, msg, &payload, &reason).await
        }
    }
}

pub fn spawn(st: AppState) {
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            if let Err(e) = consume(&st).await {
                tracing::error!(error = %e, "content.events consumer stopped; restarting");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        }
    });
}

async fn consume(st: &AppState) -> anyhow::Result<()> {
    let consumer: StreamConsumer = ClientConfig::new()
        .set("bootstrap.servers", &st.cfg.kafka_brokers)
        .set("group.id", CONSUMER_GROUP)
        .set("auto.offset.reset", "earliest")
        .set("enable.auto.commit", "false")
        .set("enable.auto.offset.store", "false")
        .set("session.timeout.ms", "30000")
        .create()?;
    consumer.subscribe(&[TOPIC_CONTENT])?;
    tracing::info!(topic = TOPIC_CONTENT, "consuming");

    loop {
        let msg = consumer.recv().await?;
        let mut backoff = Duration::from_millis(500);
        loop {
            match handle(st, &msg).await {
                Ok(()) => {
                    if let Err(e) = consumer.commit_message(&msg, CommitMode::Async) {
                        // Not fatal: the record is recorded and intake is
                        // idempotent, so a redelivery changes nothing.
                        tracing::warn!(error = %e, "offset commit failed; the record will be redelivered and ignored");
                    }
                    break;
                }
                Err(e) => {
                    tracing::error!(partition = msg.partition(), offset = msg.offset(), error = %e,
                        "intake failed; seeking back to this record");
                    match consumer.seek(msg.topic(), msg.partition(), Offset::Offset(msg.offset()), Duration::from_secs(10)) {
                        Ok(()) => {
                            // The next poll returns this record again.
                            tokio::time::sleep(backoff).await;
                            break;
                        }
                        Err(se) => {
                            tracing::error!(error = %se, "seek failed; retrying the record in place");
                            tokio::time::sleep(backoff).await;
                            backoff = (backoff * 2).min(Duration::from_secs(30));
                        }
                    }
                }
            }
        }
    }
}
