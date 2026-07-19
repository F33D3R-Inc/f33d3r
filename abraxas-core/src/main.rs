/// Abraxas Core — Kafka consumer / ranking signal pipeline.
/// Consumes ranking.events from Sitra Achra (Redpanda) and writes
/// ranking signals to post_rank_signals in f33d3r_feed Postgres.
/// No HTTP server — pure Kafka consumer.
mod config;
mod db;
mod pipeline;

use std::time::Duration;

use deadpool_postgres::{Config as DbConfig, Runtime};
use rdkafka::config::ClientConfig;
use rdkafka::consumer::{CommitMode, Consumer, StreamConsumer};
use rdkafka::message::Message;
use tokio_postgres::NoTls;
use tracing::{error, info, warn};

const TOPIC_RANKING: &str = "ranking.events";

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(tracing_subscriber::EnvFilter::from_env("RUST_LOG"))
        .init();

    info!("Abraxas Core starting");

    let cfg = config::Config::from_env();

    // ── Postgres connection pool ──────────────────────────────────────────────
    let mut pg_cfg = DbConfig::new();
    pg_cfg.url = Some(cfg.database_url.clone());
    let pool = pg_cfg
        .create_pool(Some(Runtime::Tokio1), NoTls)
        .expect("Failed to create Postgres pool");

    info!("Postgres pool ready");

    // ── Ensure schema (idempotent) ────────────────────────────────────────────
    db::ensure_schema(&pool)
        .await
        .expect("Schema initialisation failed");

    // ── Kafka consumer ────────────────────────────────────────────────────────
    let consumer: StreamConsumer = ClientConfig::new()
        .set("bootstrap.servers", &cfg.kafka_brokers)
        .set("group.id", &cfg.group_id)
        .set("auto.offset.reset", "earliest")
        .set("enable.auto.commit", "false")
        .set("session.timeout.ms", "30000")
        .create()
        .expect("Failed to create Kafka consumer");

    consumer
        .subscribe(&[TOPIC_RANKING])
        .expect("Failed to subscribe to ranking.events");

    info!(topic = TOPIC_RANKING, "Subscribed to Kafka topic");
    info!("Abraxas Core ready — consuming ranking.events");

    // ── Main consumer loop ────────────────────────────────────────────────────
    loop {
        match consumer.recv().await {
            Err(e) => {
                error!(err = %e, "Kafka recv error");
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            Ok(msg) => {
                let payload = match msg.payload_view::<str>() {
                    Some(Ok(s)) => s,
                    Some(Err(e)) => {
                        warn!(err = %e, "Non-UTF8 message payload, skipping");
                        let _ = consumer.commit_message(&msg, CommitMode::Async);
                        continue;
                    }
                    None => {
                        warn!("Empty message payload, skipping");
                        let _ = consumer.commit_message(&msg, CommitMode::Async);
                        continue;
                    }
                };

                // Deserialisation errors are non-fatal: log + skip.
                let event: pipeline::RankingEvent = match serde_json::from_str(payload) {
                    Ok(e) => e,
                    Err(e) => {
                        warn!(err = %e, payload = %payload, "Failed to parse ranking event JSON, skipping");
                        let _ = consumer.commit_message(&msg, CommitMode::Async);
                        continue;
                    }
                };

                let post_id = event.post_id.clone();

                // DB errors are non-fatal: signal loss is acceptable;
                // rank corruption is not. Log + skip, do NOT crash.
                if let Err(e) = pipeline::process(&pool, event).await {
                    error!(
                        post_id = %post_id,
                        err     = %e,
                        "Pipeline error — skipping signal (non-fatal)"
                    );
                    let _ = consumer.commit_message(&msg, CommitMode::Async);
                    continue;
                }

                info!(post_id = %post_id, "Ranking signal recorded");

                // Commit only after successful DB write.
                if let Err(e) = consumer.commit_message(&msg, CommitMode::Async) {
                    error!(err = %e, "Kafka commit failed");
                }
            }
        }
    }
}
