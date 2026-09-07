//! Who is asking, and whether they are allowed to change what a person is
//! *permitted to do*.
//!
//! # What this fixes
//!
//! Verity is the first layer of the money and age gates. `kyc_tier` /
//! `creator_tier`, the attestations behind them, the 2257 adult-content record,
//! and the CSAM verdict are all decided here, and every one of them is read
//! downstream as authority: feed-engine's `CanMonetize()` gates creator status,
//! the marketplace, subscriptions and withdrawal on the identity tier, and
//! elohim-veni's decision engine denies `monetize` / `payout_activation` when
//! Verity reports a tier below the bar.
//!
//! Until this module existed the router carried exactly one layer,
//! `observ::http_middleware` — observability. It reads `X-Pial-Identity` into a
//! tracing span and then throws it away. Nothing authenticated anybody, and
//! every mutating handler took its subject straight out of the request body or
//! path. Reaching the port was the whole authorisation check:
//!
//! ```text
//! curl -X POST http://verity:8095/v1/admin/tier \
//!      -d '{"pial_id":"<anyone>","tier":3,"admin_id":"x","reason":"x"}'
//! ```
//!
//! answered 200 and raised that account to the top creator tier — payout
//! enabled — from the LAN, with no credentials at all. `/v1/kyc/submit` was the
//! same: forge a passed `gov_id` and the account is promoted. An
//! `/v1/admin/*` route in particular must never be callable without credentials.
//!
//! # The convention
//!
//! This is not a new auth scheme. It is the one the rest of the estate already
//! uses, in both directions — the same one ain-soph and themis adopted:
//!
//!   * `X-Internal-Key` vs `INTERNAL_API_KEY` — service-to-service. Every brain
//!     that changes what a person may do is reached this way: feed-engine
//!     forwards KYC results, risk signals and 2257 records after authenticating
//!     the session; elohim-veni, zodacare, caeor and ain-soph call in as peers.
//!   * `X-Pial-Identity` — the end-user identity feed-engine injects from the
//!     session after authenticating it, with the inbound `Cookie` stripped in
//!     the same Director so a browser cannot forge it.
//!
//! [`Caller`] is the two of them as one extractor, and it denies by default.
//!
//! # The rule
//!
//! Every route that *changes* what a person is permitted to do — tier, KYC,
//! risk, compliance record, NEXUS link, AML aggregate — is a platform decision
//! made by a brain, never by the account it concerns. So those routes require a
//! [`Caller::Service`]: an end-user session naming itself is not authority to
//! raise its own tier. The read paths remain internal by deployment (prod
//! publishes no port; local binds the loopback address).

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
    /// decides who may earn money; a brain that cannot tell a peer from a
    /// stranger must not start and quietly accept both. `main` refuses to boot
    /// on the empty case, matching the posture ain-soph takes on the ledger.
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
    /// concerns explicitly, in the body or path — feed-engine forwarding a KYC
    /// result, elohim-veni recording a decision, ain-soph recording an AML
    /// withdrawal.
    Service,

    /// An end user, as authenticated by feed-engine. Present so a future
    /// self-service read or write can be expressed, but never sufficient for a
    /// platform decision — see [`Caller::require_service`].
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
    /// Gate for operations that decide what a person is permitted to do. These
    /// belong to the platform, not to the account they concern, so an end-user
    /// session is not sufficient authority for them — otherwise an account could
    /// raise its own tier, forge its own KYC pass, or clear its own risk score.
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
                     a tier/KYC/compliance decision is not self-service",
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
    /// closes — every caller would arrive as `Caller::Service` and could set any
    /// account's tier.
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

    /// A platform decision is not reachable with an end-user session. This is the
    /// rule the whole module exists for: an account cannot raise its own tier.
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

    /// Structural pin: every mutating handler in `main.rs` must take the `Caller`
    /// extractor and gate on `require_service`. A new tier/KYC/compliance route
    /// that forgets it fails here rather than in production — the regression that
    /// would silently reopen the hole while every other test still passed.
    ///
    /// The check is deliberately on the source of `main.rs`, so it cannot be
    /// satisfied by a handler that merely accepts `Caller` and never enforces it.
    #[test]
    fn every_mutating_handler_requires_a_service_caller() {
        let src = include_str!("main.rs");
        for handler in [
            "async fn decide(",
            "async fn kyc_submit(",
            "async fn risk_signal(",
            "async fn admin_set_tier(",
            "async fn compliance_2257_create(",
            "async fn compliance_csam_scan(",
        ] {
            let start = src
                .find(handler)
                .unwrap_or_else(|| panic!("missing {handler}"));
            let sig_end = src[start..]
                .find(") -> Res")
                .or_else(|| src[start..].find(") ->"))
                .unwrap_or_else(|| panic!("unterminated signature for {handler}"))
                + start;
            let sig = &src[start..sig_end];
            assert!(
                sig.contains("caller: Caller") || sig.contains("caller:Caller"),
                "{handler} does not take the Caller extractor — it would accept \
                 unauthenticated requests."
            );
            // The body must actually enforce it. Look between this handler and the
            // next `async fn` for the require_service call.
            let body_start = sig_end;
            let next = src[body_start..]
                .find("\nasync fn ")
                .map(|i| body_start + i)
                .unwrap_or(src.len());
            assert!(
                src[body_start..next].contains("caller.require_service()"),
                "{handler} takes Caller but never calls caller.require_service() — \
                 the authority check is missing."
            );
        }
    }
}
