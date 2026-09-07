//! One error type, one place that decides what a caller is told.
//!
//! Same shape as astraon/src/error.rs and ain-soph/src/error.rs: a thiserror
//! enum, an `IntoResponse` that maps each variant to a status and a machine
//! code, and a `ManhattanError` fan-out that never downgrades "the naming
//! plane could not vouch for this name" into "assume the caller was right".

use axum::{
    http::StatusCode,
    response::{IntoResponse, Response},
    Json,
};
use serde_json::json;
use thiserror::Error;

use manhattan_client::ManhattanError;

use crate::domain::lifecycle::InvalidTransition;
use crate::identity::ResolveError;

#[derive(Debug, Error)]
pub enum AuralisError {
    #[error("not found")]
    NotFound,
    #[error("bad request: {0}")]
    BadRequest(String),
    /// No credentials, or credentials that do not match.
    #[error("unauthenticated")]
    Unauthenticated,
    /// Authenticated, but the role does not permit this action.
    #[error("forbidden: {0}")]
    Forbidden(String),
    /// The Frequency is in a state where this cannot happen.
    #[error("{0}")]
    InvalidTransition(#[from] InvalidTransition),
    /// A precondition on the current state failed: locked, full, blocked,
    /// already ended. 409 with a code the caller can act on.
    #[error("conflict: {0}")]
    Conflict(&'static str),
    /// The row changed between read and write. Handlers retry a bounded
    /// number of times before surfacing it.
    #[error("version conflict")]
    VersionConflict,
    #[error("rate limited: {0}")]
    RateLimited(&'static str),
    #[error("{0}")]
    Identity(ResolveError),
    #[error("database error: {0}")]
    Db(#[from] sqlx::Error),
    #[error("redis error: {0}")]
    Redis(#[from] redis::RedisError),
    #[error("{0}")]
    Manhattan(#[from] ManhattanError),
    #[error("internal error: {0}")]
    Internal(#[from] anyhow::Error),
}

impl From<ResolveError> for AuralisError {
    fn from(e: ResolveError) -> Self {
        AuralisError::Identity(e)
    }
}

impl AuralisError {
    pub fn status(&self) -> StatusCode {
        use AuralisError::*;
        match self {
            NotFound => StatusCode::NOT_FOUND,
            BadRequest(_) => StatusCode::BAD_REQUEST,
            Unauthenticated => StatusCode::UNAUTHORIZED,
            Forbidden(_) => StatusCode::FORBIDDEN,
            InvalidTransition(_) | Conflict(_) | VersionConflict => StatusCode::CONFLICT,
            RateLimited(_) => StatusCode::TOO_MANY_REQUESTS,
            Identity(e) => match e {
                ResolveError::Malformed(_) => StatusCode::BAD_REQUEST,
                ResolveError::Unresolved(_) => StatusCode::NOT_FOUND,
                ResolveError::Unavailable(_) => StatusCode::SERVICE_UNAVAILABLE,
            },
            Manhattan(e) => match e {
                ManhattanError::NotFound => StatusCode::NOT_FOUND,
                ManhattanError::NotConfigured => StatusCode::SERVICE_UNAVAILABLE,
                _ => StatusCode::BAD_GATEWAY,
            },
            Db(_) | Redis(_) | Internal(_) => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }

    pub fn code(&self) -> &'static str {
        use AuralisError::*;
        match self {
            NotFound => "not_found",
            BadRequest(_) => "bad_request",
            Unauthenticated => "unauthenticated",
            Forbidden(_) => "forbidden",
            InvalidTransition(_) => "invalid_transition",
            Conflict(code) => code,
            VersionConflict => "version_conflict",
            RateLimited(_) => "rate_limited",
            Identity(e) => match e {
                ResolveError::Malformed(_) => "identity_malformed",
                ResolveError::Unresolved(_) => "identity_unresolved",
                ResolveError::Unavailable(_) => "identity_plane_unavailable",
            },
            Manhattan(e) => match e {
                ManhattanError::NotFound => "not_found",
                ManhattanError::NotConfigured => "manhattan_not_configured",
                _ => "manhattan_error",
            },
            Db(_) => "db_error",
            Redis(_) => "redis_error",
            Internal(_) => "internal",
        }
    }
}

impl IntoResponse for AuralisError {
    fn into_response(self) -> Response {
        let status = self.status();
        // A 5xx is this brain's fault and is logged as an error; a 4xx is the
        // caller's and is logged at warn so a flood of refusals is visible
        // without drowning real failures.
        if status.is_server_error() {
            tracing::error!(error = %self, code = self.code(), "auralis error");
        } else {
            tracing::warn!(error = %self, code = self.code(), "auralis refused");
        }
        (
            status,
            Json(json!({ "error": self.code(), "message": self.to_string() })),
        )
            .into_response()
    }
}

pub type Result<T> = std::result::Result<T, AuralisError>;
