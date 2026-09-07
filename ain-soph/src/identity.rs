//! Wallet ownership, resolved through the Manhattan naming plane.
//!
//! # What this fixes
//!
//! `accounts.user_id` is a bare TEXT primary key with no shape, no foreign key
//! and no resolution, and every write path auto-creates the row on first sight.
//! Whatever string a caller sent became a wallet. The doc comment claimed the
//! column held a PIAL; nothing enforced it, which is why a census of
//! `f33d3r_wallet` found no PIAL column at all — only `user_id`.
//!
//! That is not a naming inconvenience. This is the wallet:
//!
//!   * A handle is a POINTER, not an identity. registry-brain runs handle
//!     auction, transfer and reclaim, so a balance keyed on a handle is a
//!     balance that moves to whoever takes the handle next.
//!   * A foreign row id belongs to the brain that issued it and that brain may
//!     renumber it. Money keyed on it detaches from the person silently.
//!   * Two spellings of one person are two wallets that never reconcile, and
//!     the supply endpoint counts them as two account holders.
//!
//! # The rule
//!
//! A wallet is attributed to an identity Manhattan resolved, never to a name
//! that can be reassigned. Every caller-supplied owner string passes through
//! [`resolve_owner`] before it reaches the ledger, and what is stored is always
//! the PIAL that came out.
//!
//! # Why a PIAL costs no round trip
//!
//! A PIAL is the identity's own canonical name, so a caller that already sent
//! one has already named the identity — canonicalising it is local parsing, and
//! its registration in the graph plane is queued by trigger into
//! `manhattan_outbox` and delivered durably by the drain. Only a name that is
//! NOT the identity — a handle — requires a synchronous resolution, because the
//! whole point is that we must not persist that name.
//!
//! # Why an unresolvable name is refused
//!
//! There is no fall-back. A name that does not resolve does not get a wallet.
//! Storing the unresolved string "for now" is exactly how the shadow accounts
//! in this database came to exist.

use tracing::warn;
use uuid::Uuid;

use crate::error::WalletError;
use manhattan_client::{handle_name, pial_name, Manhattan, ManhattanError};

/// The platform float account. It is a book, not a person: fees accumulate here
/// and no identity owns it, so it is the one account key that is deliberately
/// not a PIAL and must never be resolved as one.
pub const FEE_ACCOUNT: &str = "ain_soph_fees";

/// Canonical form of a PIAL as an account key: the lowercase hyphenated UUID.
///
/// Canonicalising matters as much as validating. `A88986FF-…` and `a88986ff-…`
/// are one identity and must never become two wallets.
fn canonical_pial(value: &str) -> Option<String> {
    Uuid::parse_str(value)
        .ok()
        .map(|u| u.hyphenated().to_string())
}

/// Canonical form of a caller-supplied string *if* it names an identity
/// directly — `pial:<uuid>` or a bare `<uuid>`. `None` means the string is a
/// pointer (a handle) and only Manhattan can say who is behind it.
///
/// This exists so [`crate::auth::Caller::acting_as`] can settle the common case
/// — a browser echoing its own PIAL back through the proxy — with a local
/// comparison instead of a second round trip to the naming plane. It decides
/// nothing about ownership: whatever it returns is still only ever *compared*
/// against an identity that [`resolve_owner`] already vouched for.
pub fn claimed_pial(raw: &str) -> Option<String> {
    canonical_pial(raw.trim().strip_prefix("pial:").unwrap_or(raw.trim()))
}

/// Turn a caller-supplied owner string into the account key the ledger may use.
///
/// Accepted, in order:
///
///   `ain_soph_fees`  the platform float account, passed through unchanged
///   `pial:<uuid>`    an identity named explicitly — canonicalised, no I/O
///   `<uuid>`         an identity named bare — canonicalised, no I/O
///   `handle:<h>`     a pointer — resolved through Manhattan to its PIAL
///   `<anything>`     treated as a bare handle and resolved the same way
///
/// A UUID shape is necessary but not sufficient evidence of an identity — a
/// foreign row id is UUID-shaped too. The shape is what this function can check
/// locally; the outbox registration is what proves the name actually resolves,
/// and it says so loudly when it does not.
pub async fn resolve_owner(manhattan: &Manhattan, input: &str) -> Result<String, WalletError> {
    let raw = input.trim();
    if raw.is_empty() {
        return Err(WalletError::BadRequest(
            "owner identifier is required".into(),
        ));
    }
    if raw == FEE_ACCOUNT {
        return Ok(FEE_ACCOUNT.to_string());
    }

    // An identity named as an identity. UUID shape is not evidence: a foreign row
    // id is UUID-shaped too, so the name must actually resolve in the plane.
    let claimed = match raw.strip_prefix("pial:") {
        Some(rest) => Some(canonical_pial(rest).ok_or_else(|| {
            WalletError::UnresolvedIdentity(format!("{raw} is not a well-formed PIAL"))
        })?),
        None => canonical_pial(raw),
    };
    if let Some(pial) = claimed {
        let name = pial_name(&pial);
        return match manhattan.resolve_pial(&name).await {
            Ok(resolved) => canonical_pial(&resolved).ok_or_else(|| {
                WalletError::UnresolvedIdentity(format!("{name} resolved to a malformed PIAL"))
            }),
            Err(ManhattanError::NotFound) => Err(WalletError::UnresolvedIdentity(format!(
                "{name} is not a registered identity"
            ))),
            Err(e) => {
                warn!(name = %name, error = %e, "identity plane unavailable; refusing to key a wallet on an unverified id");
                Err(WalletError::IdentityPlaneUnavailable)
            }
        };
    }

    // Anything else is a pointer. Resolve it to the identity behind it and keep
    // only that — the pointer itself is transferable and must not be persisted.
    let handle = raw.strip_prefix("handle:").unwrap_or(raw);
    let name = handle_name(handle);
    match manhattan.resolve_pial(&name).await {
        Ok(pial) => canonical_pial(&pial).ok_or_else(|| {
            WalletError::UnresolvedIdentity(format!("{name} resolved to a malformed PIAL"))
        }),
        Err(ManhattanError::NotFound) => Err(WalletError::UnresolvedIdentity(format!(
            "{name} does not resolve to an identity"
        ))),
        Err(e) => {
            // Never degrade to storing the handle. A wallet keyed on a pointer
            // is worse than a wallet the caller has to retry for.
            warn!(name = %name, error = %e, "identity plane unavailable; refusing to key a wallet on an unresolved name");
            Err(WalletError::IdentityPlaneUnavailable)
        }
    }
}
