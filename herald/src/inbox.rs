//! The notification inbox: every notification on the platform lands here.
//!
//! Brains publish to `notification.events`. Herald consumes in one shared group
//! from the earliest offset, stores the row under its unique event ID, and only
//! then commits. A crash redelivers and the unique ID makes that a no-op, so a
//! notification is stored exactly once. Storage failures hold the partition
//! rather than skip it. Events that can never be stored go to the dead topic,
//! where an alert can see them, instead of disappearing.
//!
//! After storing, the new unread count goes to `notification.updates`, which every
//! feed-engine replica consumes to render the badge for connected devices. Push
//! is sent after that, tracked per row, and retried by the sweep.

use std::collections::HashMap;
use std::time::Duration;

use axum::{
    extract::{Path, Query, State},
    http::{HeaderMap, StatusCode},
    response::IntoResponse,
    Json,
};
use chrono::{DateTime, Utc};
use rdkafka::config::ClientConfig;
use rdkafka::consumer::{CommitMode, Consumer, StreamConsumer};
use rdkafka::message::Message;
use rdkafka::producer::{FutureProducer, FutureRecord};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tracing::{error, info, warn};

use crate::db;
use crate::handlers::{self, AppState};
use crate::identity::{self, TargetError};
use crate::models::NotifyRequest;

pub const TOPIC_EVENTS: &str = "notification.events";
pub const TOPIC_DEAD: &str = "notification.events.dead";
pub const TOPIC_UPDATES: &str = "notification.updates";

const GROUP: &str = "herald-inbox";
const PRODUCE_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_BACKOFF: Duration = Duration::from_secs(30);
const SWEEP_INTERVAL: Duration = Duration::from_secs(30);
const MAX_LIST: i64 = 500;

/// What a brain publishes.
#[derive(Debug, Deserialize)]
pub struct NotificationEvent {
    pub event_id: String,
    #[serde(default)]
    pub recipient_account: String,
    pub recipient_pial: String,
    pub kind: String,
    #[serde(default)]
    pub actor_account: Option<String>,
    #[serde(default)]
    pub target_id: String,
    #[serde(default)]
    pub target_type: String,
    #[serde(default)]
    pub payload: Option<Value>,
    /// push_inbox | inbox | push
    #[serde(default = "default_channel")]
    pub channel: String,
    #[serde(default)]
    pub push: Option<PushContent>,
    #[serde(default)]
    pub created_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct PushContent {
    pub title: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub icon_url: Option<String>,
    #[serde(default)]
    pub action_url: Option<String>,
}

fn default_channel() -> String {
    "push_inbox".into()
}

/// Whether a failure can be retried or will never succeed.
enum Failure {
    Retry(String),
    Poison(String),
}

/// Publishes to the updates and dead topics.
#[derive(Clone)]
pub struct InboxBus {
    producer: FutureProducer,
}

impl InboxBus {
    pub fn new(brokers: &str) -> anyhow::Result<Self> {
        let producer = ClientConfig::new()
            .set("bootstrap.servers", brokers)
            .set("acks", "all")
            .set("enable.idempotence", "true")
            .set("message.timeout.ms", "10000")
            .create()?;
        Ok(Self { producer })
    }

    async fn send(&self, topic: &str, key: &str, value: &str) -> Result<(), String> {
        self.producer
            .send(
                FutureRecord::to(topic).key(key).payload(value),
                PRODUCE_TIMEOUT,
            )
            .await
            .map(|_| ())
            .map_err(|(e, _)| e.to_string())
    }

    /// Announces an account's unread count, which is read from the rows each time.
    pub async fn announce_unread(&self, state: &AppState, account: &str) -> Result<(), String> {
        let unread = db::inbox_unread(&state.pool, account)
            .await
            .map_err(|e| e.to_string())?;
        let pial = db::inbox_recipient_pial(&state.pool, account)
            .await
            .map_err(|e| e.to_string())?
            .unwrap_or_default();
        let body = json!({
            "account": account,
            "pial": pial,
            "unread": unread,
            "at_ms": Utc::now().timestamp_millis(),
        })
        .to_string();
        self.send(TOPIC_UPDATES, account, &body).await
    }
}

/// Starts the consumer and the push sweep.
pub fn spawn(state: AppState, bus: InboxBus, brokers: String) {
    let consumer_state = state.clone();
    let consumer_bus = bus.clone();
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            match run_consumer(&consumer_state, &consumer_bus, &brokers).await {
                Ok(()) => return,
                Err(e) => {
                    error!(error = %e, "inbox consumer stopped; restarting");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(MAX_BACKOFF);
                }
            }
        }
    });
    tokio::spawn(async move {
        loop {
            tokio::time::sleep(SWEEP_INTERVAL).await;
            match db::inbox_push_backlog(&state.pool).await {
                Ok((pending, failed)) => {
                    metrics::gauge!("herald_push_pending").set(pending as f64);
                    metrics::gauge!("herald_push_failed").set(failed as f64);
                }
                Err(e) => warn!(error = %e, "push backlog gauge failed"),
            }
            match db::inbox_due_pushes(&state.pool).await {
                Ok(ids) => {
                    for id in ids {
                        send_push(&state, &id).await;
                    }
                }
                Err(e) => warn!(error = %e, "push sweep failed"),
            }
        }
    });
}

async fn run_consumer(state: &AppState, bus: &InboxBus, brokers: &str) -> anyhow::Result<()> {
    let consumer: StreamConsumer = ClientConfig::new()
        .set("bootstrap.servers", brokers)
        .set("group.id", GROUP)
        .set("enable.auto.commit", "false")
        .set("auto.offset.reset", "earliest")
        .create()?;
    consumer.subscribe(&[TOPIC_EVENTS])?;
    info!(topic = TOPIC_EVENTS, group = GROUP, "inbox consumer started");

    loop {
        let msg = consumer.recv().await?;
        let raw = msg.payload().unwrap_or_default();

        let mut backoff = Duration::from_millis(500);
        loop {
            match handle(state, bus, raw).await {
                Ok(()) => break,
                Err(Failure::Poison(reason)) => {
                    send_dead(bus, msg.key().unwrap_or_default(), raw, &reason).await;
                    break;
                }
                Err(Failure::Retry(reason)) => {
                    error!(error = %reason, offset = msg.offset(), "notification not stored yet; retrying");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(MAX_BACKOFF);
                }
            }
        }
        // Only after the event is stored or safely in the dead topic.
        consumer.commit_message(&msg, CommitMode::Sync)?;
    }
}

async fn send_dead(bus: &InboxBus, key: &[u8], raw: &[u8], reason: &str) {
    error!(reason = %reason, "notification event sent to the dead topic");
    metrics::counter!("herald_notifications_dead_total").increment(1);
    let body = json!({ "reason": reason, "event": String::from_utf8_lossy(raw) }).to_string();
    let key = String::from_utf8_lossy(key).to_string();
    let mut backoff = Duration::from_millis(500);
    while let Err(e) = bus.send(TOPIC_DEAD, &key, &body).await {
        error!(error = %e, "dead topic unavailable; retrying");
        tokio::time::sleep(backoff).await;
        backoff = (backoff * 2).min(MAX_BACKOFF);
    }
}

async fn handle(state: &AppState, bus: &InboxBus, raw: &[u8]) -> Result<(), Failure> {
    let mut ev: NotificationEvent = serde_json::from_slice(raw)
        .map_err(|e| Failure::Poison(format!("unreadable event: {e}")))?;

    if ev.event_id.is_empty() || ev.recipient_pial.is_empty() || ev.kind.is_empty() {
        return Err(Failure::Poison("event is missing a required field".into()));
    }
    if !matches!(ev.channel.as_str(), "push_inbox" | "inbox" | "push") {
        return Err(Failure::Poison(format!("unknown channel {}", ev.channel)));
    }
    // Other brains know the person, not the persona. The inbox belongs to their
    // primary account, which feed-engine records.
    if ev.recipient_account.is_empty() {
        ev.recipient_account = primary_account(state, &ev.recipient_pial).await?;
    }
    // A person is not told about their own actions.
    if ev.actor_account.as_deref() == Some(ev.recipient_account.as_str()) {
        return Ok(());
    }
    if let Err(e) = identity::resolve_target(&state.manhattan, &ev.recipient_pial).await {
        let why = match e {
            TargetError::Empty => "no recipient identity",
            TargetError::NexusShard => "recipient is a persona shard",
            TargetError::Malformed => "recipient identity is malformed",
            TargetError::Unknown => "recipient identity does not resolve",
        };
        return Err(Failure::Poison(why.into()));
    }

    let inserted = db::inbox_insert(&state.pool, &ev)
        .await
        .map_err(|e| Failure::Retry(e.to_string()))?;
    if inserted {
        metrics::counter!("herald_notifications_stored_total", "channel" => ev.channel.clone()).increment(1);
        if let Some(created) = ev.created_at {
            let age = (Utc::now() - created).num_milliseconds().max(0) as f64 / 1000.0;
            metrics::histogram!("herald_notification_store_delay_seconds").record(age);
        }
    }
    if ev.channel != "push" {
        bus.announce_unread(state, &ev.recipient_account)
            .await
            .map_err(Failure::Retry)?;
    }
    if inserted && ev.channel != "inbox" && ev.push.is_some() {
        let pushed_state = state.clone();
        let event_id = ev.event_id.clone();
        tokio::spawn(async move { send_push(&pushed_state, &event_id).await });
    }
    Ok(())
}

async fn primary_account(state: &AppState, pial: &str) -> Result<String, Failure> {
    let url = format!("{}/api/internal/pial/{}/primary-account", state.feed_engine_url, pial);
    let resp = state
        .http
        .get(url)
        .header("x-internal-key", &state.internal_api_key)
        .send()
        .await
        .map_err(|e| Failure::Retry(e.to_string()))?;
    if resp.status() == reqwest::StatusCode::NOT_FOUND {
        return Err(Failure::Poison("recipient has no account".into()));
    }
    if !resp.status().is_success() {
        return Err(Failure::Retry(format!("feed-engine answered {}", resp.status())));
    }
    let body: Value = resp.json().await.map_err(|e| Failure::Retry(e.to_string()))?;
    body["account"]
        .as_str()
        .map(str::to_string)
        .ok_or_else(|| Failure::Retry("feed-engine named no account".into()))
}

/// Claims one row's push and sends it. A row that was claimed by another attempt
/// is left alone.
async fn send_push(state: &AppState, event_id: &str) {
    let claim = match db::inbox_claim_push(&state.pool, event_id).await {
        Ok(Some(c)) => c,
        Ok(None) => return,
        Err(e) => {
            warn!(error = %e, "push claim failed");
            return;
        }
    };
    let response = handlers::notify(
        State(state.clone()),
        Json(NotifyRequest {
            pial_shard_id: claim.pial,
            notification_type: claim.kind,
            title: claim.title,
            body: claim.body,
            icon_url: claim.icon_url,
            action_url: claim.action_url,
            notification_id: Some(event_id.to_string()),
        }),
    )
    .await
    .into_response();
    let ok = response.status().is_success();
    metrics::counter!("herald_push_attempts_total", "outcome" => if ok { "ok" } else { "failed" }).increment(1);
    let result = axum::body::to_bytes(response.into_body(), 64 * 1024)
        .await
        .map(|b| String::from_utf8_lossy(&b).to_string())
        .unwrap_or_default();
    if let Err(e) = db::inbox_finish_push(&state.pool, event_id, ok, &result).await {
        warn!(error = %e, "recording push result failed");
    }
}

// ── Internal routes ──────────────────────────────────────────────────────────

/// Compares without an early exit, so timing reveals nothing about the key.
fn authorized(state: &AppState, headers: &HeaderMap) -> bool {
    let want = state.internal_api_key.as_bytes();
    let got = headers
        .get("x-internal-key")
        .map(|v| v.as_bytes())
        .unwrap_or_default();
    if want.is_empty() || want.len() != got.len() {
        return false;
    }
    want.iter().zip(got).fold(0u8, |acc, (a, b)| acc | (a ^ b)) == 0
}

/// Guards the routes only other brains may call.
pub async fn require_internal(
    State(state): State<AppState>,
    request: axum::extract::Request,
    next: axum::middleware::Next,
) -> axum::response::Response {
    if !authorized(&state, request.headers()) {
        return forbidden();
    }
    next.run(request).await
}

fn forbidden() -> axum::response::Response {
    (StatusCode::FORBIDDEN, Json(json!({"error": "forbidden"}))).into_response()
}

fn db_error(what: &str, e: anyhow::Error) -> axum::response::Response {
    error!(error = %e, what, "inbox query failed");
    (
        StatusCode::INTERNAL_SERVER_ERROR,
        Json(json!({"error": "db_error"})),
    )
        .into_response()
}

pub async fn list(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(account): Path<String>,
    Query(q): Query<HashMap<String, String>>,
) -> axum::response::Response {
    if !authorized(&state, &headers) {
        return forbidden();
    }
    let limit = q
        .get("limit")
        .and_then(|v| v.parse::<i64>().ok())
        .unwrap_or(50)
        .clamp(1, MAX_LIST);
    let before = q
        .get("before")
        .and_then(|v| DateTime::parse_from_rfc3339(v).ok())
        .map(|t| t.with_timezone(&Utc))
        .unwrap_or_else(Utc::now);
    let kinds: Vec<String> = q
        .get("kinds")
        .map(|v| v.split(',').map(str::to_string).collect())
        .unwrap_or_default();
    match db::inbox_list(&state.pool, &account, limit, before, &kinds).await {
        Ok(rows) => Json(json!({ "notifications": rows })).into_response(),
        Err(e) => db_error("list", e),
    }
}

pub async fn unread(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(account): Path<String>,
) -> axum::response::Response {
    if !authorized(&state, &headers) {
        return forbidden();
    }
    match db::inbox_unread(&state.pool, &account).await {
        Ok(n) => Json(json!({ "unread": n })).into_response(),
        Err(e) => db_error("unread", e),
    }
}

pub async fn group(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((account, id)): Path<(String, String)>,
) -> axum::response::Response {
    if !authorized(&state, &headers) {
        return forbidden();
    }
    match db::inbox_group(&state.pool, &account, &id).await {
        Ok(Some((kind, ids))) => Json(json!({ "kind": kind, "ids": ids })).into_response(),
        Ok(None) => (StatusCode::NOT_FOUND, Json(json!({"error": "not_found"}))).into_response(),
        Err(e) => db_error("group", e),
    }
}

#[derive(Deserialize)]
pub struct ReadRequest {
    #[serde(default)]
    ids: Vec<String>,
    #[serde(default)]
    all: bool,
}

pub async fn mark_read(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(account): Path<String>,
    Json(req): Json<ReadRequest>,
) -> axum::response::Response {
    if !authorized(&state, &headers) {
        return forbidden();
    }
    if let Err(e) = db::inbox_mark_read(&state.pool, &account, &req.ids, req.all).await {
        return db_error("mark_read", e);
    }
    // Every device learns the new count, not only the one that marked it.
    if let Some(bus) = &state.inbox_bus {
        if let Err(e) = bus.announce_unread(&state, &account).await {
            warn!(error = %e, "announcing unread after read failed");
        }
    }
    match db::inbox_unread(&state.pool, &account).await {
        Ok(n) => Json(json!({ "unread": n })).into_response(),
        Err(e) => db_error("unread", e),
    }
}

pub async fn actors(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(account): Path<String>,
    Query(q): Query<HashMap<String, String>>,
) -> axum::response::Response {
    if !authorized(&state, &headers) {
        return forbidden();
    }
    let kind = q.get("kind").cloned().unwrap_or_default();
    let days = q
        .get("days")
        .and_then(|v| v.parse::<i32>().ok())
        .unwrap_or(30);
    match db::inbox_distinct_actors(&state.pool, &account, &kind, days).await {
        Ok(n) => Json(json!({ "actors": n })).into_response(),
        Err(e) => db_error("actors", e),
    }
}

#[derive(Deserialize)]
pub struct PurgeRequest {
    account: String,
}

/// Removes an account's inbox and every row naming it to other people.
pub async fn purge(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<PurgeRequest>,
) -> axum::response::Response {
    if !authorized(&state, &headers) {
        return forbidden();
    }
    match db::inbox_purge(&state.pool, &req.account).await {
        Ok(n) => Json(json!({ "deleted": n })).into_response(),
        Err(e) => db_error("purge", e),
    }
}

#[derive(Deserialize)]
pub struct MissingRequest {
    event_ids: Vec<String>,
}

/// Which of these event IDs are not stored. Feed-engine's reconciliation asks this.
pub async fn missing(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<MissingRequest>,
) -> axum::response::Response {
    if !authorized(&state, &headers) {
        return forbidden();
    }
    match db::inbox_missing(&state.pool, &req.event_ids).await {
        Ok(ids) => Json(json!({ "missing": ids })).into_response(),
        Err(e) => db_error("missing", e),
    }
}
