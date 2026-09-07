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
    extract::DefaultBodyLimit,
    routing::{get, post},
    Router,
};

use crate::observ;
use crate::security::abuse::MAX_BODY_BYTES;
use crate::state::AppState;

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
        .with_state(state)
        .layer(axum::middleware::from_fn(observ::http_middleware))
}
