//! HTTP surface. Internal only: every route needs `X-Internal-Key`, and every
//! route that acts for a person needs `X-Pial-Identity` (see `auth`).
//!
//! Route order follows the house: `/metrics` first, `/health` second, then
//! the domain, `.with_state()` and the observability layer last.

pub mod frequencies;
pub mod health;
pub mod internal;
pub mod moderation;
pub mod ops;
pub mod participants;
pub mod view;

use axum::{
    extract::{DefaultBodyLimit, Request, State},
    http::Method,
    middleware::Next,
    response::Response,
    routing::{get, post},
    Router,
};
use uuid::Uuid;

use crate::observ;
use crate::security::abuse::MAX_BODY_BYTES;
use crate::state::AppState;

/// The Frequency a request may have changed. A beat or a token request changes
/// nothing about who may speak.
fn changed_frequency(method: &Method, path: &str) -> Option<Uuid> {
    if method != Method::POST {
        return None;
    }
    let mut segments = path.split('/');
    segments.find(|s| *s == "frequencies")?;
    let id = Uuid::parse_str(segments.next()?).ok()?;
    match segments.next() {
        Some(
            "heartbeat" | "media_token" | "react" | "remind" | "pins" | "clips_enabled" | "replay"
            | "remove_replay",
        ) => None,
        _ => Some(id),
    }
}

/// LiveKit telling us someone connected. The reconcile this wakes grants them what the
/// database says right now, and removes them if they no longer belong, within moments.
async fn livekit_webhook(
    State(s): State<AppState>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> axum::http::StatusCode {
    let authorization = headers
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if !s.media.webhook_is_genuine(authorization, &body) {
        return axum::http::StatusCode::UNAUTHORIZED;
    }
    if let Ok(event) = serde_json::from_slice::<serde_json::Value>(&body) {
        if event["event"].as_str() == Some("participant_joined") {
            if let Some(room) = event["room"]["name"].as_str().and_then(|n| Uuid::parse_str(n).ok()) {
                s.media.kick(room);
            }
        }
    }
    axum::http::StatusCode::OK
}

/// One place wakes the audio plane, so a new route cannot forget to.
async fn wake_media(State(s): State<AppState>, req: Request, next: Next) -> Response {
    let changed = changed_frequency(req.method(), req.uri().path());
    let res = next.run(req).await;
    if let Some(id) = changed {
        if res.status().is_success() {
            s.media.kick(id);
        }
    }
    res
}

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(health::health))
        .route("/ready", get(health::ready))
        // ── frequencies ──────────────────────────────────────────────────
        .route(
            "/v1/frequencies",
            post(frequencies::create).get(frequencies::list),
        )
        .route("/v1/frequencies/:id", get(frequencies::get))
        .route("/v1/frequencies/:id/events", get(frequencies::events))
        .route("/v1/frequencies/:id/update", post(frequencies::update))
        .route("/v1/frequencies/:id/schedule", post(frequencies::schedule))
        .route("/v1/frequencies/:id/start", post(frequencies::start))
        .route("/v1/frequencies/:id/end", post(frequencies::end))
        .route("/v1/frequencies/:id/cancel", post(frequencies::cancel))
        .route("/v1/frequencies/:id/lock", post(frequencies::lock))
        .route(
            "/v1/frequencies/:id/requests_open",
            post(frequencies::requests_open),
        )
        .route("/v1/hosts/:pial/open", get(frequencies::host_open))
        .route(
            "/v1/participants/:pial/current",
            get(frequencies::participant_current),
        )
        // ── participants ─────────────────────────────────────────────────
        .route("/v1/frequencies/:id/tune_in", post(participants::tune_in))
        .route("/v1/frequencies/:id/leave", post(participants::leave))
        .route(
            "/v1/frequencies/:id/heartbeat",
            post(participants::heartbeat),
        )
        .route("/v1/frequencies/:id/pins", post(moderation::pin))
        .route(
            "/v1/frequencies/:id/clips_enabled",
            post(frequencies::clips_enabled),
        )
        .route("/webhooks/livekit", post(livekit_webhook))
        .route("/v1/frequencies/:id/replay", post(frequencies::replay_file))
        .route(
            "/internal/v1/frequencies/:id/remove_replay",
            post(frequencies::remove_replay),
        )
        .route(
            "/v1/frequencies/:id/replay/delete",
            post(frequencies::delete_replay),
        )
        .route("/v1/frequencies/:id/remind", post(participants::remind))
        .route("/v1/frequencies/:id/reminders", get(participants::reminders))
        .route("/v1/frequencies/:id/react", post(participants::react))
        .route("/v1/frequencies/:id/people", get(participants::people))
        .route("/v1/frequencies/:id/step_down", post(moderation::step_down))
        .route(
            "/v1/frequencies/:id/participants/:pial/promote",
            post(moderation::promote),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/transfer",
            post(moderation::transfer),
        )
        .route(
            "/v1/frequencies/:id/media_token",
            post(participants::media_token),
        )
        .route(
            "/v1/frequencies/:id/request_mic",
            post(participants::request_mic),
        )
        .route(
            "/v1/frequencies/:id/requests/:rid/withdraw",
            post(participants::withdraw),
        )
        .route(
            "/v1/frequencies/:id/requests/:rid/upvote",
            post(participants::upvote),
        )
        // ── moderation ───────────────────────────────────────────────────
        .route(
            "/v1/frequencies/:id/invites/:pial",
            post(moderation::invite),
        )
        .route(
            "/v1/frequencies/:id/requests/:rid/approve",
            post(moderation::approve),
        )
        .route(
            "/v1/frequencies/:id/requests/:rid/decline",
            post(moderation::decline),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/mute",
            post(moderation::mute),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/unmute",
            post(moderation::unmute),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/demote",
            post(moderation::demote),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/remove",
            post(moderation::remove),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/block",
            post(moderation::block),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/unblock",
            post(moderation::unblock),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/cohost",
            post(moderation::add_cohost),
        )
        .route(
            "/v1/frequencies/:id/participants/:pial/uncohost",
            post(moderation::remove_cohost),
        )
        // ── platform ─────────────────────────────────────────────────────
        .route(
            "/internal/v1/frequencies/:id/terminate",
            post(internal::terminate),
        )
        .layer(DefaultBodyLimit::max(MAX_BODY_BYTES))
        .layer(axum::middleware::from_fn_with_state(state.clone(), wake_media))
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware))
}
