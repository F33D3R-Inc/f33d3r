//! Who is asking, and whether they may touch verity's evidence and legal
//! records.
//!
//! # What this guards
//!
//! Verity holds the KYC evidence for every verification attempt, the
//! document-reuse index, the 2257 records and the encrypted ID copies behind
//! them. Writing any of it decides what the platform can later prove about a
//! person; reading it exposes who a person is. Neither is self-service.
//!
//! Before this module the router carried one layer — observability — and
//! reaching the port was the whole authorisation check: an unauthenticated
//! `POST /v1/admin/tier` raised any account to the top tier, and a forged
//! `/v1/kyc/submit` promoted one. Those routes no longer exist (identity owns
//! the level now), and the rule they taught stays: deny by default.
//!
//! # The convention
//!
//! The one the rest of the estate already uses, in both directions:
//!
//!   * `X-Internal-Key` vs `INTERNAL_API_KEY` — service-to-service. feed-engine
//!     forwards KYC evidence and 2257 records after authenticating the session;
//!     ain-soph records AML withdrawals as a peer.
//!   * `X-Pial-Identity` — the end-user identity feed-engine injects from the
//!     session after authenticating it, with the inbound `Cookie` stripped in
//!     the same Director so a browser cannot forge it.
//!
//! [`Caller`] is the two of them as one extractor, and it denies by default.
//!
//! # The rule
//!
//! Every evidence and compliance route — read or write — and every mutating
//! NEXUS route requires a [`Caller::Service`]. An end-user session naming
//! itself is not authority to file its own evidence or read anyone's.

use axum::{
    async_trait,
    extract::{FromRef, FromRequestParts},
    http::{request::Parts, HeaderMap, StatusCode},
    response::Json,
};
use serde_json::{json, Value};

/// Service-to-service shared secret. Same header and env var as every other
/// brain that guards an internal endpoint.
pub const HEADER_INTERNAL_KEY: &str = "X-Internal-Key";

/// The end-user identity feed-engine injects after authenticating the session.
pub const HEADER_PIAL_IDENTITY: &str = "X-Pial-Identity";

/// The configured `INTERNAL_API_KEY`, held in state so the extractor can reach
/// it without reading the environment on every request.
#[derive(Clone, Debug)]
pub struct InternalApiKey(pub String);

impl InternalApiKey {
    /// Read the key from the environment.
    ///
    /// An unset or empty key is not a soft configuration warning here. Verity
    /// holds legal records; a brain that cannot tell a peer from a stranger
    /// must not start and quietly accept both. `main` refuses to boot on the
    /// empty case, matching the posture ain-soph takes on the ledger.
    pub fn from_env() -> Self {
        Self(std::env::var("INTERNAL_API_KEY").unwrap_or_default())
    }

    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }
}

/// True when the request carries the configured internal key.
///
/// An empty configured key can never match. Without that guard an unset
/// `INTERNAL_API_KEY` would turn the service lane into a blanket bypass — the
/// exact failure this module exists to remove.
pub(crate) fn internal_key_ok(headers: &HeaderMap, cfg_key: &str) -> bool {
    headers
        .get(HEADER_INTERNAL_KEY)
        .and_then(|v| v.to_str().ok())
        .map(|k| !cfg_key.is_empty() && k == cfg_key)
        .unwrap_or(false)
}

fn auth_err(status: StatusCode, code: &str, msg: &str) -> (StatusCode, Json<Value>) {
    (status, Json(json!({ "error": code, "message": msg })))
}

/// The authenticated origin of a request.
///
/// There is deliberately no third variant for "anonymous". A request that
/// proves neither identity nor peership is rejected by the extractor and never
/// becomes a `Caller` at all, so no handler can forget to check.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Caller {
    /// Another brain, holding the shared secret. It names the subject its call
    /// concerns explicitly, in the body or path — feed-engine forwarding KYC
    /// evidence or a 2257 record, ain-soph recording an AML withdrawal.
    Service,

    /// An end user, as authenticated by feed-engine. Never sufficient for an
    /// evidence or compliance route — see [`Caller::require_service`].
    Identity(String),
}

#[async_trait]
impl<S> FromRequestParts<S> for Caller
where
    InternalApiKey: FromRef<S>,
    S: Send + Sync,
{
    type Rejection = (StatusCode, Json<Value>);

    async fn from_request_parts(parts: &mut Parts, state: &S) -> Result<Self, Self::Rejection> {
        let cfg = InternalApiKey::from_ref(state);

        if internal_key_ok(&parts.headers, &cfg.0) {
            return Ok(Caller::Service);
        }

        // A present-but-wrong internal key is a peer that is misconfigured or an
        // attacker guessing. Either way it is not an end user, so it does not
        // fall through to the identity lane.
        if parts.headers.contains_key(HEADER_INTERNAL_KEY) {
            tracing::warn!("refused: X-Internal-Key present but does not match INTERNAL_API_KEY");
            return Err(auth_err(
                StatusCode::UNAUTHORIZED,
                "authentication_required",
                "X-Internal-Key does not match INTERNAL_API_KEY",
            ));
        }

        match parts
            .headers
            .get(HEADER_PIAL_IDENTITY)
            .and_then(|v| v.to_str().ok())
        {
            Some(raw) if !raw.trim().is_empty() => Ok(Caller::Identity(raw.trim().to_string())),
            _ => Err(auth_err(
                StatusCode::UNAUTHORIZED,
                "authentication_required",
                "this endpoint requires X-Internal-Key or X-Pial-Identity",
            )),
        }
    }
}

impl Caller {
    /// Gate for the platform's own records. They belong to the platform, not to
    /// the account they concern, so an end-user session is not sufficient
    /// authority — otherwise an account could file its own KYC pass or read
    /// another person's evidence.
    pub fn require_service(&self) -> Result<(), (StatusCode, Json<Value>)> {
        match self {
            Caller::Service => Ok(()),
            Caller::Identity(pial) => {
                tracing::warn!(
                    caller = %pial,
                    "refused: end-user identity called a service-only Verity endpoint"
                );
                Err(auth_err(
                    StatusCode::FORBIDDEN,
                    "service_only",
                    "this endpoint is service-to-service only; \
                     KYC evidence and compliance records are not self-service",
                ))
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    fn headers(pairs: &[(&str, &str)]) -> HeaderMap {
        let mut h = HeaderMap::new();
        for (k, v) in pairs {
            h.insert(
                axum::http::HeaderName::from_bytes(k.as_bytes()).unwrap(),
                HeaderValue::from_str(v).unwrap(),
            );
        }
        h
    }

    #[test]
    fn internal_key_matches_when_configured() {
        assert!(internal_key_ok(
            &headers(&[("X-Internal-Key", "s3cret")]),
            "s3cret"
        ));
    }

    #[test]
    fn internal_key_rejects_wrong_value() {
        assert!(!internal_key_ok(
            &headers(&[("X-Internal-Key", "guess")]),
            "s3cret"
        ));
    }

    /// An unset INTERNAL_API_KEY must never authenticate anybody. If it did, a
    /// misconfigured deployment would silently reopen the exact hole this module
    /// closes — every caller would arrive as `Caller::Service` and could file any
    /// account's evidence.
    #[test]
    fn empty_configured_key_never_authenticates() {
        assert!(!internal_key_ok(&headers(&[("X-Internal-Key", "")]), ""));
        assert!(!internal_key_ok(
            &headers(&[("X-Internal-Key", "anything")]),
            ""
        ));
        assert!(!internal_key_ok(&HeaderMap::new(), ""));
    }

    #[test]
    fn missing_internal_key_header_is_not_a_service() {
        assert!(!internal_key_ok(&HeaderMap::new(), "s3cret"));
    }

    /// A platform record is not reachable with an end-user session. This is the
    /// rule the whole module exists for: an account cannot file its own evidence.
    #[test]
    fn identity_cannot_call_service_only_routes() {
        let caller = Caller::Identity("c0ffee00-0000-4000-8000-000000000001".into());
        assert!(caller.require_service().is_err());
        assert!(Caller::Service.require_service().is_ok());
    }

    /// A request with no credentials at all never becomes a `Caller`, so there is
    /// no anonymous value in the type for a handler to accidentally honour.
    #[test]
    fn caller_has_no_anonymous_variant() {
        fn assert_exhaustive(c: &Caller) -> bool {
            match c {
                Caller::Service => true,
                Caller::Identity(_) => true,
            }
        }
        assert!(assert_exhaustive(&Caller::Service));
    }

    /// Structural pin: every evidence and compliance handler takes the
    /// `Caller` extractor and gates on `require_service`. A new route that
    /// forgets it fails here rather than in production — the regression that
    /// would silently reopen the hole while every other test still passed.
    ///
    /// The check is on the handler source, so it cannot be satisfied by a
    /// handler that merely accepts `Caller` and never enforces it.
    #[test]
    fn every_record_handler_requires_a_service_caller() {
        for (file, src, handlers) in [
            (
                "kyc.rs",
                include_str!("kyc.rs"),
                &["pub async fn submit(", "pub async fn summary("][..],
            ),
            (
                "compliance.rs",
                include_str!("compliance.rs"),
                &[
                    "pub async fn record(",
                    "pub async fn get(",
                    "pub async fn content_removed(",
                ][..],
            ),
        ] {
            for handler in handlers {
                let start = src
                    .find(handler)
                    .unwrap_or_else(|| panic!("missing {handler} in {file}"));
                let sig_end = src[start..]
                    .find(") -> Result<")
                    .unwrap_or_else(|| panic!("unterminated signature for {handler} in {file}"))
                    + start;
                assert!(
                    src[start..sig_end].contains("caller: Caller"),
                    "{file}: {handler} does not take the Caller extractor — it would \
                     accept unauthenticated requests."
                );
                let next = src[sig_end..]
                    .find("\npub async fn ")
                    .map(|i| sig_end + i)
                    .unwrap_or(src.len());
                assert!(
                    src[sig_end..next].contains("caller.require_service()"),
                    "{file}: {handler} takes Caller but never calls \
                     caller.require_service() — the authority check is missing."
                );
            }
        }
    }
}
