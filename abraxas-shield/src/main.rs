/// Abraxas Shield — Kafka consumer / content safety pipeline.
/// Replaces the Python content-scan brain entirely.
/// No HTTP server — pure consumer/producer.
mod bot;
mod clickbait;
mod config;
mod db;
mod nsfw;
mod pipeline;
mod spam;
mod toxicity;

use std::time::Duration;

use deadpool_postgres::{Config as DbConfig, Runtime};
use rdkafka::config::ClientConfig;
use rdkafka::consumer::{CommitMode, Consumer, StreamConsumer};
use rdkafka::message::Message;
use rdkafka::producer::{FutureProducer, FutureRecord};
use serde_json::json;
use tokio_postgres::NoTls;
use tracing::{error, info, warn};

const TOPIC_CONTENT: &str = "content.events";
const TOPIC_SAFETY: &str = "safety.events";
const CONSUMER_GROUP: &str = "abraxas-shield";

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::from_env("RUST_LOG")
        )
        .init();

    info!("Abraxas Shield starting");

    let cfg = config::Config::from_env();
    let scan_version = cfg.scan_version.clone();

    // ── Postgres connection pool ──────────────────────────────────────────────
    let mut pg_cfg = DbConfig::new();
    pg_cfg.url = Some(cfg.database_url.clone());
    let pool = pg_cfg
        .create_pool(Some(Runtime::Tokio1), NoTls)
        .expect("Failed to create Postgres pool");

    info!("Postgres pool ready");

    // ── Kafka consumer ────────────────────────────────────────────────────────
    let consumer: StreamConsumer = ClientConfig::new()
        .set("bootstrap.servers", &cfg.kafka_brokers)
        .set("group.id", CONSUMER_GROUP)
        .set("auto.offset.reset", "earliest")
        .set("enable.auto.commit", "false")
        .set("session.timeout.ms", "30000")
        .create()
        .expect("Failed to create Kafka consumer");

    consumer
        .subscribe(&[TOPIC_CONTENT])
        .expect("Failed to subscribe to content.events");

    info!(topic = TOPIC_CONTENT, "Subscribed to Kafka topic");

    // ── Kafka producer ────────────────────────────────────────────────────────
    let producer: FutureProducer = ClientConfig::new()
        .set("bootstrap.servers", &cfg.kafka_brokers)
        .set("message.timeout.ms", "10000")
        .create()
        .expect("Failed to create Kafka producer");

    // ── ONNX visual detector (optional — None if model file absent) ──────────
    let detector = nsfw::load_detector();
    if detector.is_some() {
        info!("NSFW visual detector loaded — running in text+visual mode");
    } else {
        info!("NSFW visual detector not loaded — running in text-only mode");
    }

    info!("Abraxas Shield ready — consuming content.events");

    // ── Main loop ─────────────────────────────────────────────────────────────
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

                let event: pipeline::ContentEvent = match serde_json::from_str(payload) {
                    Ok(e) => e,
                    Err(e) => {
                        warn!(err = %e, "Failed to parse content event JSON, skipping");
                        let _ = consumer.commit_message(&msg, CommitMode::Async);
                        continue;
                    }
                };

                // Only process post.created events (future events may differ).
                // Safe to check via the parsed JSON directly in the raw payload.
                let event_type = serde_json::from_str::<serde_json::Value>(payload)
                    .ok()
                    .and_then(|v| v.get("event").and_then(|e| e.as_str()).map(String::from))
                    .unwrap_or_default();

                if event_type != "post.created" {
                    let _ = consumer.commit_message(&msg, CommitMode::Async);
                    continue;
                }

                let post_id = event.post_id.clone();
                let work_id = event.work_id.clone();
                info!(post_id = %post_id, work_id = %work_id, "Processing content event");

                // 1. Fetch content + author data from DB.
                // When work_id is present, queries the works table; otherwise falls back to posts.
                let (scan_record, content_source) = match db::get_post_for_scan(&pool, &post_id, &work_id).await {
                    Some(pair) => pair,
                    None => {
                        warn!(post_id = %post_id, work_id = %work_id, "Content not found or deleted, skipping");
                        let _ = consumer.commit_message(&msg, CommitMode::Async);
                        continue;
                    }
                };

                // 2. Skip if already past pending_scan (e.g. duplicate delivery).
                if !scan_record.pial_id.is_empty()
                    && event.scan_state != "pending_scan"
                    && !event.scan_state.is_empty()
                {
                    info!(
                        post_id = %post_id,
                        scan_state = %event.scan_state,
                        "Post already processed, skipping"
                    );
                    let _ = consumer.commit_message(&msg, CommitMode::Async);
                    continue;
                }

                // 3. Run detection pipeline.
                let decision = pipeline::run(
                    &pool,
                    &event,
                    scan_record.is_adult_creator,
                    &scan_version,
                    detector.as_ref(),
                )
                .await;

                info!(
                    post_id = %post_id,
                    recommendation = %decision.recommendation,
                    risk_level = %decision.risk_level,
                    "Shield decision"
                );

                // 4. Map recommendation → scan_state.
                let next_state = match decision.recommendation.as_str() {
                    "approve" => "clean",
                    "age_gate" => "age_gated",
                    "human_review" => "human_review",
                    "auto_block" => "blocked",
                    _ => "human_review",
                };

                // 5. Write scan result to DB.
                let scan_result = db::ScanResult {
                    post_id: post_id.clone(),
                    scan_version: scan_version.clone(),
                    nudity_score: decision.nudity_score,
                    gore_score: decision.gore_score,
                    clickbait_score: decision.clickbait_score,
                    hate_signals: decision.hate_signals.clone(),
                    violence_signals: decision.violence_signals.clone(),
                    self_harm_signals: decision.self_harm_signals.clone(),
                    risk_level: decision.risk_level.clone(),
                    recommendation: decision.recommendation.clone(),
                    signals: decision.signals.clone(),
                    is_duplicate: false,
                    duplicate_type: String::new(),
                    original_post_id: String::new(),
                };

                if let Err(e) = db::store_scan_result(&pool, &scan_result).await {
                    error!(post_id = %post_id, err = %e, "Failed to store scan result");
                    // Do not commit — retry on restart.
                    continue;
                }

                // 5b. When content came from the works table, also write per-signal scores
                //     to work_scores so the ranking pipeline can consume them.
                if content_source == db::ContentSource::Works {
                    if let Err(e) = db::store_work_scores(&pool, &post_id, &scan_result).await {
                        error!(post_id = %post_id, err = %e, "Failed to store work_scores");
                        continue;
                    }
                }

                // 6. Transition scan_state (works: sets is_blocked; posts: updates scan_state column).
                let reason = format!(
                    "abraxas-shield: {} (signals: {})",
                    decision.risk_level,
                    decision.signals.join(", ")
                );
                if let Err(e) =
                    db::set_post_scan_state(&pool, &post_id, next_state, &reason, &content_source).await
                {
                    error!(post_id = %post_id, err = %e, "Failed to set scan_state");
                    continue;
                }

                // 7. If age_gated, set is_nsfw = true on the appropriate table.
                if next_state == "age_gated" {
                    if let Err(e) = db::set_post_nsfw(&pool, &post_id, &content_source).await {
                        warn!(post_id = %post_id, err = %e, "Failed to set is_nsfw (non-fatal)");
                    }
                }

                // 8. If human_review, notify admin users.
                if next_state == "human_review" {
                    let admin_ids = db::get_admin_user_ids(&pool).await;
                    for admin_id in &admin_ids {
                        if let Err(e) =
                            db::notify_user(&pool, admin_id, "moderation", &post_id).await
                        {
                            warn!(
                                admin_id = %admin_id,
                                err = %e,
                                "Failed to notify admin (non-fatal)"
                            );
                        }
                    }
                }

                // 9. Publish safety.events to Sitra Achra.
                let safety_payload = json!({
                    "event": "post.safety_scored",
                    "post_id": post_id,
                    "pial_id": scan_record.pial_id,
                    "recommendation": decision.recommendation,
                    "risk_level": decision.risk_level,
                    "signals": decision.signals,
                    "nudity_score": decision.nudity_score,
                    "clickbait_score": decision.clickbait_score,
                    "hate_signals": decision.hate_signals,
                    "violence_signals": decision.violence_signals,
                    "self_harm_signals": decision.self_harm_signals,
                });
                let safety_bytes = serde_json::to_vec(&safety_payload)
                    .unwrap_or_default();

                let record = FutureRecord::to(TOPIC_SAFETY)
                    .key(post_id.as_bytes())
                    .payload(&safety_bytes);

                if let Err((e, _)) = producer
                    .send(record, Duration::from_secs(5))
                    .await
                {
                    warn!(post_id = %post_id, err = %e, "Failed to publish to safety.events (non-fatal)");
                }

                // 10. Commit Kafka offset only after all DB writes succeeded.
                if let Err(e) = consumer.commit_message(&msg, CommitMode::Async) {
                    error!(err = %e, "Kafka commit failed");
                }

                info!(post_id = %post_id, next_state, "Scan complete");
            }
        }
    }
}
