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

/// States meaning "no verdict yet". Both spellings exist in the fleet: the works
/// column defaults to `pending`, older events carry `pending_scan`.
fn is_unscanned(scan_state: &str) -> bool {
    matches!(scan_state, "" | "pending" | "pending_scan")
}

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

                // Only creation events carry scannable content. Every lane that
                // publishes to content.events must be listed here — an unlisted
                // lane is silently unscanned, which is how Visions went dark.
                let event_type = serde_json::from_str::<serde_json::Value>(payload)
                    .ok()
                    .and_then(|v| v.get("event").and_then(|e| e.as_str()).map(String::from))
                    .unwrap_or_default();

                if !matches!(event_type.as_str(), "post.created" | "vision.created") {
                    let _ = consumer.commit_message(&msg, CommitMode::Async);
                    continue;
                }

                // Route to the lane the event names: vision_id → visions, work_id → works.
                let (content_id, content_source) = match event.content_ref() {
                    Some((id, source)) => (id.to_string(), source),
                    None => {
                        error!(
                            event_type = %event_type,
                            post_id = %event.post_id,
                            "Content event names no work_id or vision_id — unroutable, cannot scan"
                        );
                        let _ = consumer.commit_message(&msg, CommitMode::Async);
                        continue;
                    }
                };
                let lane = content_source.lane();
                info!(content_id = %content_id, lane, event_type = %event_type, "Processing content event");

                // 1. Fetch content + author data from the lane's table.
                //    Ok(None) means the row is deleted or — for a Vision — already
                //    past expires_at. Nothing to scan; commit and move on.
                //    Err means the database failed; do not commit, retry the offset.
                let scan_record = match db::get_content_for_scan(&pool, &content_id, content_source).await {
                    Ok(Some(record)) => record,
                    Ok(None) => {
                        info!(content_id = %content_id, lane, "Content gone (deleted or expired), skipping");
                        let _ = consumer.commit_message(&msg, CommitMode::Async);
                        continue;
                    }
                    Err(e) => {
                        error!(content_id = %content_id, lane, err = %e, "Failed to load content for scan");
                        continue;
                    }
                };

                // 2. Skip if already past an unscanned state (e.g. duplicate delivery).
                // feed-engine writes "pending"; "pending_scan" is the legacy spelling.
                if !scan_record.pial_id.is_empty() && !is_unscanned(&event.scan_state) {
                    info!(
                        content_id = %content_id,
                        lane,
                        scan_state = %event.scan_state,
                        "Content already processed, skipping"
                    );
                    let _ = consumer.commit_message(&msg, CommitMode::Async);
                    continue;
                }

                // 3. Run detection pipeline.
                let decision = pipeline::run(
                    &pool,
                    &event,
                    &content_id,
                    content_source,
                    scan_record.is_adult_creator,
                    &scan_version,
                    detector.as_ref(),
                )
                .await;

                info!(
                    content_id = %content_id,
                    lane,
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

                // 5. Write scan result to DB, tagged with the lane it came from.
                let scan_result = db::ScanResult {
                    content_id: content_id.clone(),
                    lane: lane.to_string(),
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
                    error!(content_id = %content_id, lane, err = %e, "Failed to store scan result");
                    // Do not commit — retry on restart.
                    continue;
                }

                // 5b. Works lane only: per-signal scores feed the ranking pipeline.
                //     The ephemeral Vision lane has no ranking surface.
                if content_source == db::ContentSource::Works {
                    if let Err(e) = db::store_work_scores(&pool, &content_id, &scan_result).await {
                        error!(content_id = %content_id, err = %e, "Failed to store work_scores");
                        continue;
                    }
                }

                // 6. Apply the verdict to the content row in its own lane and log
                //    the transition. A row that went away mid-scan — soft-deleted,
                //    or a Vision that hit its 24-hour expiry — is a normal skip, not
                //    an error to retry forever.
                let reason = format!(
                    "abraxas-shield: {} (signals: {})",
                    decision.risk_level,
                    decision.signals.join(", ")
                );
                match db::set_content_scan_state(
                    &pool, &content_id, next_state, &reason, content_source,
                )
                .await
                {
                    Ok(db::VerdictApplied::Updated { from_state }) => {
                        info!(
                            content_id = %content_id,
                            lane,
                            from_state = %from_state,
                            to_state = next_state,
                            "Verdict applied"
                        );
                    }
                    Ok(db::VerdictApplied::RowGone) => {
                        info!(
                            content_id = %content_id,
                            lane,
                            "Content gone before the verdict landed (deleted or expired), verdict discarded"
                        );
                        if let Err(e) = consumer.commit_message(&msg, CommitMode::Async) {
                            error!(err = %e, "Kafka commit failed");
                        }
                        continue;
                    }
                    Err(e) => {
                        error!(content_id = %content_id, lane, err = %e, "Failed to apply verdict");
                        continue;
                    }
                }

                // 7. If human_review, notify admin users. target_type names the lane
                //    so the admin surface resolves the id against the right table.
                if next_state == "human_review" {
                    let admin_ids = db::get_admin_user_ids(&pool).await;
                    for admin_id in &admin_ids {
                        if let Err(e) = db::notify_user(
                            &pool,
                            admin_id,
                            "moderation",
                            &content_id,
                            content_source.target_type(),
                        )
                        .await
                        {
                            warn!(
                                admin_id = %admin_id,
                                content_id = %content_id,
                                lane,
                                err = %e,
                                "Failed to notify admin (non-fatal)"
                            );
                        }
                    }
                }

                // 8. Publish safety.events to Sitra Achra, under the lane's own
                //    event name so downstream brains can tell the lanes apart.
                let (safety_event, id_field) = match content_source {
                    db::ContentSource::Works => ("post.safety_scored", "post_id"),
                    db::ContentSource::Visions => ("vision.safety_scored", "vision_id"),
                };
                let safety_payload = json!({
                    "event": safety_event,
                    "lane": lane,
                    id_field: content_id,
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
                    .key(content_id.as_bytes())
                    .payload(&safety_bytes);

                if let Err((e, _)) = producer
                    .send(record, Duration::from_secs(5))
                    .await
                {
                    warn!(content_id = %content_id, lane, err = %e, "Failed to publish to safety.events (non-fatal)");
                }

                // 9. Commit Kafka offset only after all DB writes succeeded.
                if let Err(e) = consumer.commit_message(&msg, CommitMode::Async) {
                    error!(err = %e, "Kafka commit failed");
                }

                info!(content_id = %content_id, lane, next_state, "Scan complete");
            }
        }
    }
}
