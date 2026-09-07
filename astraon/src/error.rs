use axum::{
    http::StatusCode,
    response::{IntoResponse, Response},
    Json,
};
use serde_json::json;
use thiserror::Error;

use manhattan_client::ManhattanError;

#[derive(Debug, Error)]
pub enum AstraonError {
    #[error("not found")]
    NotFound,
    #[error("bad request: {0}")]
    BadRequest(String),
    #[error("database error: {0}")]
    Db(#[from] sqlx::Error),
    /// A name did not resolve, or the naming plane could not be reached. Never
    /// downgraded to "assume the caller was right" — an unresolvable name means
    /// astraon does not know whose analytics it was asked for.
    #[error("{0}")]
    Manhattan(#[from] ManhattanError),
    #[error("internal error: {0}")]
    Internal(#[from] anyhow::Error),
}

impl IntoResponse for AstraonError {
    fn into_response(self) -> Response {
        let (status, msg) = match &self {
            AstraonError::NotFound => (StatusCode::NOT_FOUND, "not_found"),
            AstraonError::BadRequest(_) => (StatusCode::BAD_REQUEST, "bad_request"),
            AstraonError::Db(_) => (StatusCode::INTERNAL_SERVER_ERROR, "db_error"),
            AstraonError::Manhattan(e) => match e {
                // The name is genuinely absent (or revoked — deliberately the
                // same answer), so this is the caller's 404, not a fault here.
                ManhattanError::NotFound => (StatusCode::NOT_FOUND, "not_found"),
                // No MANHATTAN_URL in this deployment. Analytics that needs a
                // name resolved cannot be served, and says so.
                ManhattanError::NotConfigured => {
                    (StatusCode::SERVICE_UNAVAILABLE, "manhattan_not_configured")
                }
                _ => (StatusCode::BAD_GATEWAY, "manhattan_error"),
            },
            AstraonError::Internal(_) => (StatusCode::INTERNAL_SERVER_ERROR, "internal"),
        };
        tracing::error!(error = %self, "astraon error");
        (
            status,
            Json(json!({ "error": msg, "message": self.to_string() })),
        )
            .into_response()
    }
}

pub type Result<T> = std::result::Result<T, AstraonError>;
