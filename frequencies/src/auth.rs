//! Who is asking, and who they are acting as.
//!
//! Auralis is an internal brain. Every control-plane call arrives from Nantar
//! (or from a peer brain, or an operator with the shared secret); the browser
//! never reaches these routes. The one browser-facing path is the signaling
//! socket, which authenticates with a token this brain minted — see
//! `security::tokens`. So the convention here is the estate's, in the strict
//! form:
//!
//!   * `X-Internal-Key` vs `INTERNAL_API_KEY` is REQUIRED on every route. A
//!     request without it, or with the wrong one, is refused before a handler
//!     runs. There is no "identity lane" that bypasses the key, because there
//!     is no legitimate caller of these routes that is not a service.
//!   * `X-Pial-Identity` names the person the service is acting FOR: the user
//!     who pressed Tune In, the host who pressed End. Nantar sets it after
//!     authenticating the session and strips the inbound Cookie. It is a name
//!     and is resolved through Manhattan before anything is written.
//!
//! Two extractors, so a route states which it needs:
//!
//!   * [`Service`] — the shared secret only. Platform operations: moderation
//!     termination, reconciliation, health of internals.
//!   * [`Actor`] — the shared secret AND an identity. Every user action.
//!
//! A request may not name whose Frequency it controls in the body. The acting
//! identity comes from the header and nowhere else.

use axum::{
    async_trait,
    extract::{FromRef, FromRequestParts},
    http::{request::Parts, HeaderMap},
};

use crate::error::AuralisError;
use crate::identity;
use manhattan_client::Manhattan;

/// Service-to-service shared secret. Same header and same env var as every
/// other brain that guards an internal endpoint.
pub const HEADER_INTERNAL_KEY: &str = "X-Internal-Key";

/// The end-user identity Nantar injects after authenticating the session.
pub const HEADER_PIAL_IDENTITY: &str = "X-Pial-Identity";

/// Correlation id, set by Nantar and echoed by observ::http_middleware.
pub const HEADER_REQUEST_ID: &str = "X-Request-ID";

/// The configured `INTERNAL_API_KEY`, held in state so the extractor can reach
/// it without reading the environment on every request.
#[derive(Clone, Debug)]
pub struct InternalApiKey(pub String);

impl InternalApiKey {
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
/// `INTERNAL_API_KEY` would turn every route into an open one.
fn internal_key_ok(headers: &HeaderMap, cfg_key: &str) -> bool {
    headers
        .get(HEADER_INTERNAL_KEY)
        .and_then(|v| v.to_str().ok())
        .map(|k| !cfg_key.is_empty() && k == cfg_key)
        .unwrap_or(false)
}

fn correlation_id(headers: &HeaderMap) -> String {
    headers
        .get(HEADER_REQUEST_ID)
        .or_else(|| headers.get("x-request-id"))
        .and_then(|v| v.to_str().ok())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_default()
}

/// A peer service holding the shared secret. Carries the correlation id so a
/// platform action still traces back to what caused it.
#[derive(Clone, Debug)]
pub struct Service {
    pub correlation_id: String,
}

#[async_trait]
impl<S> FromRequestParts<S> for Service
where
    InternalApiKey: FromRef<S>,
    S: Send + Sync,
{
    type Rejection = AuralisError;

    async fn from_request_parts(parts: &mut Parts, state: &S) -> Result<Self, Self::Rejection> {
        let cfg = InternalApiKey::from_ref(state);
        if !internal_key_ok(&parts.headers, &cfg.0) {
            tracing::warn!("refused: X-Internal-Key missing or does not match INTERNAL_API_KEY");
            return Err(AuralisError::Unauthenticated);
        }
        Ok(Service {
            correlation_id: correlation_id(&parts.headers),
        })
    }
}

/// A person, acting through a service that authenticated them.
///
/// `pial` is the canonical `pial:<uuid>` name the naming plane vouched for.
/// It is the only identity a handler may write.
#[derive(Clone, Debug)]
pub struct Actor {
    pub pial: String,
    pub correlation_id: String,
}

impl Actor {
    /// The bare uuid, for the event envelope's `pial_id` field.
    pub fn pial_id(&self) -> &str {
        identity::bare_value(&self.pial)
    }
}

/// What the request presented, before the name is resolved. Kept separate so
/// the header check is a pure function and the resolution is the one place
/// that touches the network.
#[derive(Debug, PartialEq, Eq)]
pub(crate) struct PresentedIdentity {
    pub raw: String,
    pub correlation_id: String,
}

pub(crate) fn presented_identity(
    headers: &HeaderMap,
    cfg_key: &str,
) -> Result<PresentedIdentity, AuralisError> {
    if !internal_key_ok(headers, cfg_key) {
        tracing::warn!("refused: X-Internal-Key missing or does not match INTERNAL_API_KEY");
        return Err(AuralisError::Unauthenticated);
    }
    match headers
        .get(HEADER_PIAL_IDENTITY)
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
    {
        Some(raw) if !raw.is_empty() => Ok(PresentedIdentity {
            raw: raw.to_string(),
            correlation_id: correlation_id(headers),
        }),
        _ => {
            tracing::warn!("refused: X-Pial-Identity missing on a route that acts for a person");
            Err(AuralisError::Unauthenticated)
        }
    }
}

/// Everything the `Actor` extractor needs from application state.
#[derive(Clone)]
pub struct ActorContext {
    pub key: InternalApiKey,
    pub manhattan: Manhattan,
}

#[async_trait]
impl<S> FromRequestParts<S> for Actor
where
    ActorContext: FromRef<S>,
    S: Send + Sync,
{
    type Rejection = AuralisError;

    async fn from_request_parts(parts: &mut Parts, state: &S) -> Result<Self, Self::Rejection> {
        let ctx = ActorContext::from_ref(state);
        let presented = presented_identity(&parts.headers, &ctx.key.0)?;
        // The authenticated header is still a name, so it is resolved exactly
        // like any other. An identity the plane will not vouch for does not
        // get to host, join or moderate anything under this brain.
        let pial = identity::resolve(&ctx.manhattan, &presented.raw).await?;
        Ok(Actor {
            pial,
            correlation_id: presented.correlation_id,
        })
    }
}

/// For read routes a service calls on a person's behalf: the service key has
/// already been checked by the [`Service`] extractor; if an identity header
/// is present it is resolved, otherwise the read is anonymous.
pub async fn optional_viewer(
    state: &crate::state::AppState,
    headers: &HeaderMap,
) -> Result<Option<String>, AuralisError> {
    match headers
        .get(HEADER_PIAL_IDENTITY)
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
    {
        Some(raw) if !raw.is_empty() => Ok(Some(identity::resolve(&state.manhattan, raw).await?)),
        _ => Ok(None),
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
    /// misconfigured deployment would open every control-plane route.
    #[test]
    fn empty_configured_key_never_authenticates() {
        assert!(!internal_key_ok(&headers(&[("X-Internal-Key", "")]), ""));
        assert!(!internal_key_ok(
            &headers(&[("X-Internal-Key", "anything")]),
            ""
        ));
        assert!(!internal_key_ok(&HeaderMap::new(), ""));
    }

    /// An identity header without the service key is not a caller. The browser
    /// cannot reach these routes, and if something else forged the header it
    /// still has to hold the secret.
    #[test]
    fn identity_without_internal_key_is_refused() {
        let err = presented_identity(
            &headers(&[("X-Pial-Identity", "c0ffee00-0000-4000-8000-000000000001")]),
            "s3cret",
        )
        .unwrap_err();
        assert!(matches!(err, AuralisError::Unauthenticated));
    }

    /// The service key alone is not a person. A user route needs both.
    #[test]
    fn service_key_without_identity_is_refused_on_actor_routes() {
        let err =
            presented_identity(&headers(&[("X-Internal-Key", "s3cret")]), "s3cret").unwrap_err();
        assert!(matches!(err, AuralisError::Unauthenticated));
    }

    #[test]
    fn both_headers_present_yields_the_raw_name_and_correlation() {
        let p = presented_identity(
            &headers(&[
                ("X-Internal-Key", "s3cret"),
                ("X-Pial-Identity", "  c0ffee00-0000-4000-8000-000000000001 "),
                ("X-Request-ID", "req-1"),
            ]),
            "s3cret",
        )
        .unwrap();
        assert_eq!(p.raw, "c0ffee00-0000-4000-8000-000000000001");
        assert_eq!(p.correlation_id, "req-1");
    }

    /// Structural pin: no handler may take the acting identity from a request
    /// body. Every route that acts for a person takes the `Actor` extractor.
    #[test]
    fn no_handler_reads_an_acting_identity_from_the_body() {
        for src in [
            include_str!("api/frequencies.rs"),
            include_str!("api/participants.rs"),
            include_str!("api/moderation.rs"),
        ] {
            for forbidden in [
                "body.pial",
                "body.actor",
                "req.pial",
                "req.actor",
                "body.host",
            ] {
                assert!(
                    !src.contains(forbidden),
                    "a handler takes the acting identity from the request body: `{forbidden}`"
                );
            }
        }
    }
}
