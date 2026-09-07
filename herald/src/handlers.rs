use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::IntoResponse,
    Json,
};
use base64::{engine::general_purpose::STANDARD as B64_STANDARD, Engine as _};
use serde_json::json;
use sqlx::PgPool;
use tracing::{info, warn};
use web_push::{
    ContentEncoding, IsahcWebPushClient, SubscriptionInfo, VapidSignatureBuilder, WebPushClient,
    WebPushMessageBuilder,
};

use crate::db;
use crate::identity::{self, Resolution};
use crate::models::{NotifyRequest, SubscribeRequest, UpdatePreferencesRequest};
use manhattan_client::Manhattan;

// ── Shared application state ──────────────────────────────────────────────────

#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    pub vapid_pub_key: String,
    pub vapid_private_key: String, // base64-encoded PEM
    pub vapid_subject: String,
    /// The naming plane. Herald resolves identities through it and never
    /// asserts one — elohim-veni owns identity, herald owns delivery.
    pub manhattan: Manhattan,
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health(State(state): State<AppState>) -> impl IntoResponse {
    // Backlog depth is the honest measure of the naming handoff: the drain stops
    // on its head row by design, so a backlog that only grows is the signal that
    // registrations have stopped landing. Reporting `manhattan: true` while the
    // queue climbs would be a health check that lies.
    let outbox_backlog = db::manhattan_backlog_depth(&state.pool).await;
    let unresolvable = db::count_unresolvable_targets(&state.pool).await;

    if let Err(ref e) = outbox_backlog {
        warn!(error = %e, "manhattan_backlog_depth failed");
    }
    if let Err(ref e) = unresolvable {
        warn!(error = %e, "count_unresolvable_targets failed");
    }

    // A count that could not be read is reported as null, never as zero. Zero is
    // a claim; null is the truth when the query failed.
    Json(json!({
        "status":                 "ok",
        "service":                "herald",
        "manhattan":              state.manhattan.configured(),
        "manhattan_outbox_depth": outbox_backlog.ok(),
        "unresolvable_targets":   unresolvable.ok(),
    }))
}

// ── GET /v1/vapid-public-key ──────────────────────────────────────────────────

pub async fn vapid_public_key(State(state): State<AppState>) -> impl IntoResponse {
    Json(json!({ "public_key": state.vapid_pub_key }))
}

// ── POST /v1/subscriptions ────────────────────────────────────────────────────

pub async fn subscribe(
    State(state): State<AppState>,
    Json(req): Json<SubscribeRequest>,
) -> impl IntoResponse {
    if req.device_id.is_empty()
        || req.endpoint.is_empty()
        || req.p256dh_key.is_empty()
        || req.auth_key.is_empty()
    {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "error": "device_id, endpoint, p256dh_key, and auth_key are required" })),
        )
            .into_response();
    }

    // A push subscription is a standing permission to reach a person's devices.
    // It must name an identity the naming plane resolved, not an identifier this
    // brain merely received — a stale or reused one delivers private
    // notifications to whoever holds it next.
    let resolved = match identity::resolve_target(&state.manhattan, &req.pial_shard_id).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Resolution::Unverified {
        ref name,
        ref reason,
    } = resolved
    {
        warn!(
            name = %name,
            reason = %reason,
            "registering push subscription against an unverified identity; registration is queued in the outbox"
        );
    }

    match db::upsert_subscription(
        &state.pool,
        &req.pial_shard_id,
        &req.device_id,
        &req.endpoint,
        &req.p256dh_key,
        &req.auth_key,
        req.user_agent.as_deref(),
    )
    .await
    {
        Ok(()) => {
            info!(
                identity = %resolved.name(),
                device_id = %req.device_id,
                "push subscription registered"
            );
            (StatusCode::OK, Json(json!({ "subscribed": true }))).into_response()
        }
        Err(e) => {
            warn!(error = %e, "upsert_subscription failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── DELETE /v1/subscriptions/:device_id ──────────────────────────────────────

pub async fn unsubscribe(
    State(state): State<AppState>,
    Path(device_id): Path<String>,
) -> impl IntoResponse {
    match db::deactivate_subscription(&state.pool, &device_id).await {
        Ok(true) => {
            info!(device_id = %device_id, "push subscription deactivated");
            (StatusCode::OK, Json(json!({ "unsubscribed": true }))).into_response()
        }
        Ok(false) => (
            StatusCode::NOT_FOUND,
            Json(json!({ "error": "subscription not found" })),
        )
            .into_response(),
        Err(e) => {
            warn!(error = %e, "deactivate_subscription failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── GET /v1/preferences/:pial_shard_id ───────────────────────────────────────

pub async fn get_preferences(
    State(state): State<AppState>,
    Path(pial_shard_id): Path<String>,
) -> impl IntoResponse {
    // A read needs the shape gate but not a round trip: reading defaults back
    // for a name that resolves to nobody discloses nothing and writes nothing.
    if let Err(e) = identity::pial_target(&pial_shard_id) {
        return e.into_response();
    }

    match db::get_preferences(&state.pool, &pial_shard_id).await {
        Ok(prefs) => (StatusCode::OK, Json(prefs)).into_response(),
        Err(e) => {
            warn!(error = %e, pial_shard_id = %pial_shard_id, "get_preferences failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── PATCH /v1/preferences/:pial_shard_id ─────────────────────────────────────

pub async fn update_preferences(
    State(state): State<AppState>,
    Path(pial_shard_id): Path<String>,
    Json(req): Json<UpdatePreferencesRequest>,
) -> impl IntoResponse {
    // This creates a preferences row keyed by identity, so it resolves the name
    // first for the same reason subscribe does.
    let resolved = match identity::resolve_target(&state.manhattan, &pial_shard_id).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Resolution::Unverified {
        ref name,
        ref reason,
    } = resolved
    {
        warn!(
            name = %name,
            reason = %reason,
            "updating preferences for an unverified identity; registration is queued in the outbox"
        );
    }

    match db::upsert_preferences(&state.pool, &pial_shard_id, &req).await {
        Ok(prefs) => {
            info!(identity = %resolved.name(), "notification preferences updated");
            (StatusCode::OK, Json(prefs)).into_response()
        }
        Err(e) => {
            warn!(error = %e, pial_shard_id = %pial_shard_id, "upsert_preferences failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── GET /v1/stats ─────────────────────────────────────────────────────────────

pub async fn stats(State(state): State<AppState>) -> impl IntoResponse {
    let active_subscriptions = db::count_active_subscriptions(&state.pool)
        .await
        .unwrap_or(0);
    let notifications_today = db::count_notifications_today(&state.pool)
        .await
        .unwrap_or(0);
    Json(json!({
        "service":               "herald",
        "active_subscriptions":  active_subscriptions,
        "notifications_today":   notifications_today,
    }))
}

// ── POST /v1/notify ───────────────────────────────────────────────────────────
// Called by Nantar (feed-engine) after creating a notification.
// Looks up user's push subscriptions + preferences, delivers via Web Push.

pub async fn notify(
    State(state): State<AppState>,
    Json(req): Json<NotifyRequest>,
) -> impl IntoResponse {
    // Resolve who is being notified before deciding anything else. Every branch
    // below reads or writes rows keyed by this identifier; if it names nobody
    // the naming plane vouches for, none of them should run.
    let resolved = match identity::resolve_target(&state.manhattan, &req.pial_shard_id).await {
        Ok(r) => r,
        Err(e) => return e.into_response(),
    };
    if let Resolution::Unverified {
        ref name,
        ref reason,
    } = resolved
    {
        // The subscriptions this will deliver to were themselves resolved when
        // they were registered, so delivery here is not an assumption about a
        // name — it is the consequence of one already checked.
        warn!(
            name = %name,
            reason = %reason,
            "notifying an unverified identity; delivering to subscriptions resolved at registration"
        );
    }

    // Check VAPID is configured
    if state.vapid_private_key.is_empty() {
        warn!("VAPID_PRIVATE_KEY not configured — skipping push delivery");
        return (
            StatusCode::OK,
            Json(json!({"sent": 0, "reason": "vapid_not_configured"})),
        )
            .into_response();
    }

    // Check user preferences
    let prefs = db::get_preferences(&state.pool, &req.pial_shard_id)
        .await
        .unwrap_or_else(|_| crate::models::NotificationPreferences {
            pial_shard_id: req.pial_shard_id.clone(),
            push_enabled: true,
            messages_enabled: true,
            likes_enabled: true,
            reposts_enabled: true,
            replies_enabled: true,
            follows_enabled: true,
            achievements_enabled: true,
            mentions_enabled: true,
            frequencies_enabled: true,
            quiet_hours_enabled: false,
            quiet_hours_start: 22,
            quiet_hours_end: 8,
            timezone: "UTC".into(),
        });

    if !prefs.push_enabled {
        return (
            StatusCode::OK,
            Json(json!({"sent": 0, "reason": "push_disabled"})),
        )
            .into_response();
    }

    let type_enabled = match req.notification_type.as_str() {
        "like" => prefs.likes_enabled,
        "repost" => prefs.reposts_enabled,
        "reply" | "mention" => prefs.replies_enabled,
        "follow" => prefs.follows_enabled,
        "dm" => prefs.messages_enabled,
        "achievement" => prefs.achievements_enabled,
        // Frequencies (Auralis): you were given the mic, made a co-host, or
        // invited. One preference covers the three.
        "frequency_speaker_accepted" | "frequency_cohost" | "frequency_invite" => {
            prefs.frequencies_enabled
        }
        "tip" | "subscribe" => true,
        _ => true,
    };
    if !type_enabled {
        return (
            StatusCode::OK,
            Json(json!({"sent": 0, "reason": "type_disabled"})),
        )
            .into_response();
    }

    // Get active subscriptions
    let subscriptions = match db::get_active_subscriptions(&state.pool, &req.pial_shard_id).await {
        Ok(s) => s,
        Err(e) => {
            warn!(error = %e, "get_active_subscriptions failed");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error": "db_error"})),
            )
                .into_response();
        }
    };

    if subscriptions.is_empty() {
        return (
            StatusCode::OK,
            Json(json!({"sent": 0, "reason": "no_subscriptions"})),
        )
            .into_response();
    }

    // Decode PEM from base64
    let pem_bytes = match B64_STANDARD.decode(&state.vapid_private_key) {
        Ok(b) => b,
        Err(e) => {
            warn!(error = %e, "VAPID_PRIVATE_KEY base64 decode failed");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error": "vapid_key_invalid"})),
            )
                .into_response();
        }
    };

    // Log notification attempt
    let log_id = db::log_notification(
        &state.pool,
        &req.pial_shard_id,
        &req.notification_type,
        &req.title,
        &req.body,
        req.icon_url.as_deref(),
        req.action_url.as_deref(),
    )
    .await
    .ok();

    // Build payload JSON — matches sw.js push handler expectations
    let payload = json!({
        "title": req.title,
        "body":  req.body,
        "icon":  req.icon_url.clone().unwrap_or_else(|| "/static/brand/logo.png".into()),
        "badge": "/static/brand/favicon.png",
        "tag":   format!("f33d3r-{}", req.notification_type),
        "data":  { "url": req.action_url.clone().unwrap_or_else(|| "/notifications".into()) },
        "vibrate": [200, 100, 200],
    })
    .to_string();

    // Build a PartialVapidSignatureBuilder once — reused per subscription.
    let partial_sig_builder =
        match VapidSignatureBuilder::from_pem_no_sub(std::io::Cursor::new(&pem_bytes)) {
            Ok(b) => b,
            Err(e) => {
                warn!(error = %e, "VapidSignatureBuilder::from_pem_no_sub failed");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(json!({"error": "vapid_build_failed"})),
                )
                    .into_response();
            }
        };

    let client = match IsahcWebPushClient::new() {
        Ok(c) => c,
        Err(e) => {
            warn!(error = %e, "IsahcWebPushClient::new failed");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error": "client_init_failed"})),
            )
                .into_response();
        }
    };

    // Send to each subscription
    let mut sent = 0usize;
    for sub in &subscriptions {
        let sub_info = SubscriptionInfo::new(&sub.endpoint, &sub.p256dh_key, &sub.auth_key);
        let sig = match partial_sig_builder.clone().add_sub_info(&sub_info).build() {
            Ok(s) => s,
            Err(e) => {
                warn!(error = %e, device_id = %sub.device_id, "VAPID sig build failed");
                continue;
            }
        };
        let mut builder = WebPushMessageBuilder::new(&sub_info);
        builder.set_payload(ContentEncoding::Aes128Gcm, payload.as_bytes());
        builder.set_vapid_signature(sig);
        let message = match builder.build() {
            Ok(m) => m,
            Err(e) => {
                warn!(error = %e, device_id = %sub.device_id, "push message build failed");
                continue;
            }
        };
        match client.send(message).await {
            Ok(_) => {
                sent += 1;
                info!(identity = %resolved.name(), device_id = %sub.device_id, "push delivered");
            }
            Err(e) => {
                warn!(error = %e, device_id = %sub.device_id, "push send failed");
                let msg = e.to_string();
                if msg.contains("410") || msg.contains("Gone") {
                    let _ = db::deactivate_subscription(&state.pool, &sub.device_id).await;
                }
            }
        }
    }

    if let Some(id) = log_id {
        let _ = db::mark_delivered(&state.pool, id, sent > 0).await;
    }

    (
        StatusCode::OK,
        Json(json!({ "sent": sent, "total": subscriptions.len() })),
    )
        .into_response()
}
