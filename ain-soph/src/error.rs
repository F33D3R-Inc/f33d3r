use axum::{
    http::StatusCode,
    response::{IntoResponse, Response},
    Json,
};
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

    /// The caller named an owner that does not resolve to an identity. There is
    /// no fall-back to storing the name as given: a wallet keyed on a pointer
    /// follows the pointer when it is transferred.
    #[error("owner does not resolve to an identity: {0}")]
    UnresolvedIdentity(String),

    /// Manhattan could not be reached to resolve a name that is not itself an
    /// identity. Refusing is deliberate — a retryable 503 is strictly better
    /// than a wallet attributed to the wrong person.
    #[error("identity plane unavailable: cannot resolve owner")]
    IdentityPlaneUnavailable,

    /// The request proved neither an end-user identity nor peership. Ain Soph
    /// moves money, so this is the default answer for anything that arrives
    /// without credentials — reaching the port is not authority.
    #[error("authentication required: this endpoint moves value")]
    Unauthenticated,

    /// The caller authenticated as one identity and named another as the one
    /// whose value moves. Answered loudly rather than silently substituting the
    /// authenticated identity, so a caller that meant something else finds out.
    #[error("you may only move value as your own identity")]
    ActingAsAnother,

    /// A platform operation reached with an end-user session. There is no person
    /// on whose behalf a ledger checkpoint could be made.
    #[error("this endpoint is service-to-service only")]
    ServiceOnly,
}

impl IntoResponse for WalletError {
    fn into_response(self) -> Response {
        let (status, code) = match &self {
            WalletError::InsufficientBalance => {
                (StatusCode::UNPROCESSABLE_ENTITY, "insufficient_balance")
            }
            WalletError::AccountNotFound(_) => (StatusCode::NOT_FOUND, "account_not_found"),
            WalletError::InvalidAmount => (StatusCode::BAD_REQUEST, "invalid_amount"),
            WalletError::SelfTransfer => (StatusCode::BAD_REQUEST, "self_transfer"),
            WalletError::ProposalNotFound => (StatusCode::NOT_FOUND, "proposal_not_found"),
            WalletError::ProposalNotActive => {
                (StatusCode::UNPROCESSABLE_ENTITY, "proposal_not_active")
            }
            WalletError::AlreadyVoted => (StatusCode::CONFLICT, "already_voted"),
            WalletError::InvalidOption => (StatusCode::BAD_REQUEST, "invalid_option"),
            WalletError::BadRequest(_) => (StatusCode::BAD_REQUEST, "bad_request"),
            WalletError::UnresolvedIdentity(_) => {
                (StatusCode::UNPROCESSABLE_ENTITY, "unresolved_identity")
            }
            WalletError::IdentityPlaneUnavailable => (
                StatusCode::SERVICE_UNAVAILABLE,
                "identity_plane_unavailable",
            ),
            WalletError::Unauthenticated => (StatusCode::UNAUTHORIZED, "authentication_required"),
            WalletError::ActingAsAnother => (StatusCode::FORBIDDEN, "acting_as_another_identity"),
            WalletError::ServiceOnly => (StatusCode::FORBIDDEN, "service_only"),
            WalletError::Db(_) => (StatusCode::INTERNAL_SERVER_ERROR, "db_error"),
        };
        (
            status,
            Json(json!({ "error": code, "message": self.to_string() })),
        )
            .into_response()
    }
}
