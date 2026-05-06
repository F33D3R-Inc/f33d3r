use axum::{http::StatusCode, response::{IntoResponse, Response}, Json};
use serde_json::json;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum WalletError {
    #[error("database error: {0}")]
    Db(#[from] sqlx::Error),

    #[error("insufficient balance")]
    InsufficientBalance,

    #[error("account not found: {0}")]
    AccountNotFound(String),

    #[error("invalid amount: must be > 0")]
    InvalidAmount,

    #[error("self-transfer not allowed")]
    SelfTransfer,

    #[error("proposal not found")]
    ProposalNotFound,

    #[error("proposal has ended or is not active")]
    ProposalNotActive,

    #[error("you have already voted on this proposal")]
    AlreadyVoted,

    #[error("invalid option: must be one of the proposal's options")]
    InvalidOption,

    #[error("invalid input: {0}")]
    BadRequest(String),
}

impl IntoResponse for WalletError {
    fn into_response(self) -> Response {
        let (status, code) = match &self {
            WalletError::InsufficientBalance  => (StatusCode::UNPROCESSABLE_ENTITY, "insufficient_balance"),
            WalletError::AccountNotFound(_)   => (StatusCode::NOT_FOUND, "account_not_found"),
            WalletError::InvalidAmount        => (StatusCode::BAD_REQUEST, "invalid_amount"),
            WalletError::SelfTransfer         => (StatusCode::BAD_REQUEST, "self_transfer"),
            WalletError::ProposalNotFound     => (StatusCode::NOT_FOUND, "proposal_not_found"),
            WalletError::ProposalNotActive    => (StatusCode::UNPROCESSABLE_ENTITY, "proposal_not_active"),
            WalletError::AlreadyVoted         => (StatusCode::CONFLICT, "already_voted"),
            WalletError::InvalidOption        => (StatusCode::BAD_REQUEST, "invalid_option"),
            WalletError::BadRequest(_)        => (StatusCode::BAD_REQUEST, "bad_request"),
            WalletError::Db(_)                => (StatusCode::INTERNAL_SERVER_ERROR, "db_error"),
        };
        (status, Json(json!({ "error": code, "message": self.to_string() }))).into_response()
    }
}
