use axum::{http::StatusCode, response::{IntoResponse, Response}, Json};
use serde_json::json;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum AstraonError {
    #[error("not found")]
    NotFound,
    #[error("database error: {0}")]
    Db(#[from] sqlx::Error),
    #[error("internal error: {0}")]
    Internal(#[from] anyhow::Error),
}

impl IntoResponse for AstraonError {
    fn into_response(self) -> Response {
        let (status, msg) = match &self {
            AstraonError::NotFound  => (StatusCode::NOT_FOUND, "not_found"),
            AstraonError::Db(_)     => (StatusCode::INTERNAL_SERVER_ERROR, "db_error"),
            AstraonError::Internal(_) => (StatusCode::INTERNAL_SERVER_ERROR, "internal"),
        };
        tracing::error!(error = %self, "astraon error");
        (status, Json(json!({ "error": msg, "message": self.to_string() }))).into_response()
    }
}

pub type Result<T> = std::result::Result<T, AstraonError>;
