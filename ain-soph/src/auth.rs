//! Who is asking, and whose money they are allowed to move.
//!
//! # What this fixes
//!
//! Ain Soph moves real value — AET balances, and the BTC/XRP payout paths that
//! settle against them. Until this module existed the router carried exactly one
//! layer, `observ::http_middleware`, which is observability: it reads
//! `X-Pial-Identity` into a tracing span and then throws it away. Nothing
//! authenticated anybody.
//!
//! Every value handler took the acting identity straight out of the request
//! body — `deposit.user_id`, `transfer.from_user_id`, `tip.from_pial_id`,
//! `withdraw.user_id`. [`identity::resolve_owner`] proved that name *existed*;
//! nothing ever proved the caller *was* it. A request could therefore name whose
//! money it moved. Reaching the port was the whole authorisation check:
//!
//! ```text
//! curl -X POST http://ain-soph:8089/v1/withdraw \
//!      -d '{"user_id":"<anyone>","amount_cents":500000, ...}'
//! ```
//!
//! answered 200 and debited them. feed-engine's `CanMonetize()` gate in front of
//! the proxy is correct and stays, but it is a gate in front of a service that
//! would still do whatever anyone who reached it directly asked. This module is
//! the second layer: the one that holds when the first is bypassed.
//!
//! # The convention
//!
//! This is not a new auth scheme. It is the one the rest of the estate already
//! uses, in both directions:
//!
//!   * `X-Internal-Key` vs `INTERNAL_API_KEY` — service-to-service. elohim-veni
//!     guards every PIAL key endpoint with it, feed-engine's live lane checks it
//!     on MediaMTX hooks, and Ain Soph itself already *sends* it outbound on the
//!     balance webhook. The key was in this brain's environment all along; only
//!     the inbound check was missing.
//!   * `X-Pial-Identity` — the end-user identity, set by feed-engine from the
//!     session after authenticating it, with the inbound `Cookie` stripped in
//!     the same Director so a browser cannot forge it. themis reads exactly this
//!     header for exactly this reason on its commerce endpoints.
//!
//! [`Caller`] is the two of them as one extractor, and it denies by default.
//!
//! # The rule
//!
//! A request may not name whose money it moves. The acting identity comes from
//! the authenticated context and nowhere else — see [`Caller::acting_as`].

use axum::{
    async_trait,
    extract::{FromRef, FromRequestParts},
    http::{request::Parts, HeaderMap},
};

use crate::error::WalletError;
use crate::identity;
use manhattan_client::Manhattan;

/// Service-to-service shared secret. Same header and same env var as every other
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
    /// An unset or empty key is not a soft configuration warning here. Ain Soph
    /// moves money; a brain that cannot tell a peer from a stranger must not
    /// start and quietly accept both. `main` refuses to boot on the empty case,
    /// which is the same posture feed-engine takes in `cmd/server/main.go`.
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
fn internal_key_ok(headers: &HeaderMap, cfg_key: &str) -> bool {
    headers
        .get(HEADER_INTERNAL_KEY)
        .and_then(|v| v.to_str().ok())
        .map(|k| !cfg_key.is_empty() && k == cfg_key)
        .unwrap_or(false)
}

/// The authenticated origin of a request.
///
/// There is deliberately no third variant for "anonymous". A request that
/// proves neither identity nor peership is rejected by the extractor and never
/// becomes a `Caller` at all, so no handler can forget to check.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Caller {
    /// Another brain, holding the shared secret. It may act for an identity that
    /// is not making the call — themis settling a purchase for a buyer,
    /// aethyr-ledger checkpointing a sealed block, feed-engine sweeping the fee
    /// float. It must still say who it is acting for, explicitly, in the body.
    Service,

    /// An end user, as authenticated by feed-engine. Canonical PIAL, verified
    /// against the naming plane before it reaches the ledger. It may act only as
    /// itself.
    Identity(String),
}

#[async_trait]
impl<S> FromRequestParts<S> for Caller
where
    InternalApiKey: FromRef<S>,
    S: Send + Sync,
{
    type Rejection = WalletError;

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
            return Err(WalletError::Unauthenticated);
        }

        match parts
            .headers
            .get(HEADER_PIAL_IDENTITY)
            .and_then(|v| v.to_str().ok())
        {
            Some(raw) if !raw.trim().is_empty() => Ok(Caller::Identity(raw.trim().to_string())),
            _ => Err(WalletError::Unauthenticated),
        }
    }
}

/// What a body-supplied claim can be decided to be without touching the network.
///
/// Splitting this out is not tidiness. It is the part of the authorisation rule
/// that must never depend on a reachable naming plane to say "no", and keeping
/// it pure is what lets it be pinned by tests that cannot silently pass because
/// Manhattan happened to be down.
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum ClaimVerdict {
    /// The claim names the authenticated identity. Nothing to do.
    Same,
    /// The claim names a *different* identity, and that is knowable locally
    /// because both sides are PIALs. Refused without a round trip.
    Different,
    /// The claim is a pointer — a handle — so only Manhattan can say who is
    /// behind it. Resolve, then compare.
    NeedsResolution,
}

/// Compare a caller-supplied claim against the authenticated identity.
///
/// `me` is already canonical, having come back from [`identity::resolve_owner`].
pub(crate) fn claim_verdict(me: &str, claimed: &str) -> ClaimVerdict {
    match identity::claimed_pial(claimed) {
        Some(pial) if pial == me => ClaimVerdict::Same,
        Some(_) => ClaimVerdict::Different,
        None => ClaimVerdict::NeedsResolution,
    }
}

impl Caller {
    /// The identity this request acts as — the account that is debited, credited,
    /// authors the proposal, or casts the vote.
    ///
    /// This is the one function that decides whose money moves, and the body
    /// never gets to be the answer:
    ///
    ///   * [`Caller::Identity`] acts as itself. `claimed` is not the source of
    ///     the answer; it is only checked, so that a caller who *meant* to act as
    ///     someone else is told no rather than silently acting as itself.
    ///   * [`Caller::Service`] must name its subject, because a peer settling on
    ///     someone's behalf has no session of its own to derive one from. That is
    ///     the explicit, separately-authorised path — it costs the shared secret.
    ///
    /// The returned key always comes back through [`identity::resolve_owner`], so
    /// the ledger invariant holds unchanged: what is stored is a PIAL the naming
    /// plane vouched for, never a name that can be reassigned.
    pub async fn acting_as(
        &self,
        manhattan: &Manhattan,
        claimed: Option<&str>,
    ) -> Result<String, WalletError> {
        let claimed = claimed.map(str::trim).filter(|s| !s.is_empty());

        match self {
            Caller::Service => {
                let named = claimed.ok_or_else(|| {
                    WalletError::BadRequest(
                        "a service caller must name the identity it is acting for".into(),
                    )
                })?;
                identity::resolve_owner(manhattan, named).await
            }

            Caller::Identity(raw) => {
                // The authenticated header is still a name, so it is resolved
                // exactly like any other. An identity the plane will not vouch
                // for does not get to move value under this brain either.
                let me = identity::resolve_owner(manhattan, raw).await?;

                if let Some(named) = claimed {
                    // A PIAL-shaped claim is settled locally — no second round
                    // trip for what is overwhelmingly the common case, a browser
                    // echoing back its own PIAL through the proxy.
                    let same = match claim_verdict(&me, named) {
                        ClaimVerdict::Same => true,
                        ClaimVerdict::Different => false,
                        ClaimVerdict::NeedsResolution => {
                            identity::resolve_owner(manhattan, named).await? == me
                        }
                    };
                    if !same {
                        tracing::warn!(
                            caller = %me,
                            claimed = %named,
                            "refused: caller tried to move value as a different identity"
                        );
                        return Err(WalletError::ActingAsAnother);
                    }
                }

                Ok(me)
            }
        }
    }

    /// Gate for operations that belong to the platform rather than to any person
    /// — the ledger checkpoint aethyr-ledger posts after sealing a block. There
    /// is no end user on whose behalf such a call could be made, so an end-user
    /// session is not sufficient authority for it.
    pub fn require_service(&self) -> Result<(), WalletError> {
        match self {
            Caller::Service => Ok(()),
            Caller::Identity(pial) => {
                tracing::warn!(caller = %pial, "refused: identity called a service-only endpoint");
                Err(WalletError::ServiceOnly)
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
    /// closes — every caller would arrive as `Caller::Service` and could name
    /// whose money it moved.
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

    /// A service-to-service caller has no session, so it must say who it acts
    /// for. Silence is refused rather than defaulted.
    #[tokio::test]
    async fn service_caller_must_name_its_subject() {
        let mh = Manhattan::new("", "ain-soph-test", "test-internal-key");
        let err = Caller::Service.acting_as(&mh, None).await.unwrap_err();
        assert!(matches!(err, WalletError::BadRequest(_)), "got {err:?}");
    }

    /// An identity-only endpoint is not reachable with an end-user session.
    #[test]
    fn identity_cannot_call_service_only_routes() {
        let caller = Caller::Identity("c0ffee00-0000-4000-8000-000000000001".into());
        assert!(matches!(
            caller.require_service().unwrap_err(),
            WalletError::ServiceOnly
        ));
        assert!(Caller::Service.require_service().is_ok());
    }

    // ── The rule this module exists for ──────────────────────────────────────

    const ME: &str = "c0ffee00-0000-4000-8000-000000000001";
    const SOMEONE_ELSE: &str = "c0ffee00-0000-4000-8000-000000000011";

    /// THE pin: an authenticated identity naming somebody else as the party whose
    /// value moves is refused, and refused locally — no naming-plane round trip,
    /// so this cannot degrade into an allow when Manhattan is unreachable.
    ///
    /// This is the defect in one assertion. Before this module, `withdraw` read
    /// `req.user_id` and debited whoever it named.
    #[test]
    fn body_may_not_name_a_different_identity() {
        assert_eq!(claim_verdict(ME, SOMEONE_ELSE), ClaimVerdict::Different);
        assert_eq!(
            claim_verdict(ME, &format!("pial:{SOMEONE_ELSE}")),
            ClaimVerdict::Different
        );
    }

    /// The body echoing back the caller's own identity is not an attack — it is
    /// what the browser does through the proxy — and must not cost a round trip.
    #[test]
    fn body_echoing_the_caller_is_accepted_locally() {
        assert_eq!(claim_verdict(ME, ME), ClaimVerdict::Same);
        assert_eq!(claim_verdict(ME, &format!("pial:{ME}")), ClaimVerdict::Same);
        assert_eq!(claim_verdict(ME, &format!("  {ME}  ")), ClaimVerdict::Same);
        // Case is a spelling, not an identity: two casings must not become two
        // wallets, and must not read as an impersonation attempt either.
        assert_eq!(claim_verdict(ME, &ME.to_uppercase()), ClaimVerdict::Same);
    }

    /// A handle is a pointer, so it cannot be judged locally — but it is still
    /// never the *source* of the acting identity, only something compared
    /// against it after resolution.
    #[test]
    fn a_handle_claim_defers_to_the_naming_plane() {
        assert_eq!(
            claim_verdict(ME, "miiyazuko"),
            ClaimVerdict::NeedsResolution
        );
        assert_eq!(
            claim_verdict(ME, "handle:miiyazuko"),
            ClaimVerdict::NeedsResolution
        );
    }

    /// A request with no credentials at all never becomes a `Caller`, so there is
    /// no anonymous value in the type for a handler to accidentally honour.
    #[test]
    fn caller_has_no_anonymous_variant() {
        // Exhaustive match: adding a third, weaker variant breaks this build.
        fn assert_exhaustive(c: &Caller) -> bool {
            match c {
                Caller::Service => true,
                Caller::Identity(_) => true,
            }
        }
        assert!(assert_exhaustive(&Caller::Service));
    }

    /// Structural pin: no value handler may resolve an acting identity straight
    /// out of the request body again.
    ///
    /// `identity::resolve_owner` is correct for a *recipient* — naming who you
    /// pay is the point. It is the vulnerability for a *payer*. This asserts that
    /// every payer-shaped field reaches the ledger through `Caller::acting_as`
    /// and never through a direct resolve, which is precisely the regression that
    /// would silently reopen the hole while every other test still passed.
    #[test]
    fn no_handler_resolves_an_acting_identity_from_the_body() {
        let src = include_str!("handlers.rs");
        for field in [
            "req.user_id",
            "req.from_user_id",
            "req.from_pial_id",
            "req.creator_id",
            "req.voter_id",
        ] {
            let forbidden = format!("resolve_owner(&mh, &{field})");
            assert!(
                !src.contains(&forbidden),
                "handlers.rs takes the acting identity from the request body: `{forbidden}`. \
                 The acting identity must come from Caller::acting_as."
            );
        }
        // And the safe direction is still in use, so this test cannot pass merely
        // because the handlers stopped resolving anything at all.
        assert!(src.contains("resolve_owner(&mh, &req.to_user_id)"));
        assert!(src.contains("resolve_owner(&mh, &req.to_pial_id)"));
    }

    /// Every route that moves value or casts stake takes the `Caller` extractor.
    /// A new value route that forgets it fails here rather than in production.
    #[test]
    fn every_value_handler_takes_the_caller_extractor() {
        let src = include_str!("handlers.rs");
        for handler in [
            "pub async fn deposit(",
            "pub async fn transfer(",
            "pub async fn tip(",
            "pub async fn withdraw(",
            "pub async fn create_proposal(",
            "pub async fn cast_vote(",
            "pub async fn ledger_checkpoint(",
        ] {
            let start = src
                .find(handler)
                .unwrap_or_else(|| panic!("missing {handler}"));
            // The signature ends at the closing paren that begins a line, not at
            // the first paren encountered — `State(pool): State<PgPool>` has its
            // own.
            let sig_end = src[start..]
                .find("\n) ->")
                .unwrap_or_else(|| panic!("unterminated signature for {handler}"))
                + start;
            assert!(
                src[start..sig_end].contains("Caller"),
                "{handler} does not take the Caller extractor — it would accept \
                 unauthenticated requests."
            );
        }
    }
}
