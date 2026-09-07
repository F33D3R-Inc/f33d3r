//! Turning the value herald is handed into a NAME, and resolving that name.
//!
//! # The finding this module encodes
//!
//! Herald's three tables call their identity column `pial_shard_id`. Only
//! herald, lore and verity's NEXUS tables use that spelling; the other nine
//! brains say `pial_id`. It looked like a synonym. It is not — and it is not the
//! same thing herald actually stores either:
//!
//!   * In verity and lore, `pial_shard_id` is
//!     `hex(sha256(pial_uuid || ":nexus-pial-shard-v1"))` — a blinded 64-hex
//!     alias for one PIAL, so the NEXUS tables never hold a raw PIAL. It is
//!     1:1 with a PIAL and irreversible without the PIAL in hand.
//!   * In herald, every value that has ever arrived is the raw PIAL UUID.
//!     feed-engine's `HeraldNotify` and `heraldSubscribeJSON` both pass
//!     `users.pial_id::text` under the JSON key `pial_shard_id`, and
//!     `/v1/preferences/:pial_shard_id` is called with `user.PIALID`.
//!
//! So herald's column is misnamed, not differently-typed: it holds identities.
//! A NEXUS shard is a persona alias belonging to an identity, and it is verity's
//! to interpret — herald cannot invert the hash and has no authority to mint a
//! name for it. The naming plane models that distinction directly: `KIND_NEXUS`
//! in the `manhattan-client` crate is the verified natural person who may hold several
//! identities, and it is deliberately *not* an identity. Herald resolves
//! identities; the person behind them is somebody else's node to register.
//!
//! Both shapes fit `VARCHAR(64)`, which is why nothing has ever caught a mix. If
//! a blinded shard were ever posted here, herald would register a second
//! identity node for a person who already has one and deliver that person's
//! notifications to it — the wrong persona of the right human, which is the
//! exact separability guarantee F33D3R sells. So the ambiguity is refused at the
//! door here and again at the table (`herald_is_pial` in `db.rs`), and it is
//! never guessed.
//!
//! # Resolution policy
//!
//! Herald does not own identity; elohim-veni does. Herald therefore resolves a
//! name and never asserts one:
//!
//!   * A name that Manhattan authoritatively does not know, or that resolves to
//!     a node which is not `active`, is refused. That is the stale-or-reused
//!     identifier case, and delivering to it is how a notification reaches
//!     someone else.
//!   * A naming plane that is unset or unreachable yields `Unverified`. The
//!     value has still passed the shape gate and its registration is already
//!     durably queued in the outbox, so herald proceeds and says so. Failing
//!     closed here would turn a naming-plane blip into a total push outage while
//!     protecting against nothing: an unreachable Manhattan is silent about
//!     staleness, it does not assert it.

use axum::{http::StatusCode, response::IntoResponse, Json};
use serde_json::json;
use tracing::warn;

use manhattan_client::{pial_name, Manhattan, ManhattanError};

/// The outcome of turning an inbound identifier into a resolved name.
pub enum Resolution {
    /// Manhattan knows this name and the node behind it is active.
    Verified(String),
    /// Shape-valid, registration queued, but the naming plane could not confirm
    /// it right now. Carries the reason so the log says which.
    Unverified { name: String, reason: String },
}

impl Resolution {
    pub fn name(&self) -> &str {
        match self {
            Resolution::Verified(n) => n,
            Resolution::Unverified { name, .. } => name,
        }
    }
}

/// Every way an inbound identifier can fail to name somebody herald may notify.
#[derive(Debug)]
pub enum TargetError {
    /// No identifier at all.
    Empty,
    /// A NEXUS-derived persona shard. Verity's to resolve, not herald's.
    NexusShard,
    /// Neither a PIAL nor a recognisable shard.
    Malformed,
    /// Manhattan says this name does not resolve, or resolves to something
    /// revoked. The two are deliberately indistinguishable.
    Unknown,
}

impl TargetError {
    pub fn status(&self) -> StatusCode {
        match self {
            TargetError::Empty | TargetError::NexusShard | TargetError::Malformed => {
                StatusCode::BAD_REQUEST
            }
            TargetError::Unknown => StatusCode::NOT_FOUND,
        }
    }

    pub fn code(&self) -> &'static str {
        match self {
            TargetError::Empty => "identity_required",
            TargetError::NexusShard => "nexus_shard_not_resolvable",
            TargetError::Malformed => "identity_malformed",
            TargetError::Unknown => "identity_unknown",
        }
    }

    pub fn message(&self) -> &'static str {
        match self {
            TargetError::Empty => "pial_shard_id is required and must be a PIAL UUID",
            TargetError::NexusShard => {
                "pial_shard_id looks like a NEXUS-derived persona shard. Herald resolves \
                 pial:<uuid> through Manhattan and cannot invert a blinded shard; send the PIAL"
            }
            TargetError::Malformed => {
                "pial_shard_id must be a PIAL UUID — herald resolves it as the name pial:<uuid>"
            }
            TargetError::Unknown => {
                "pial_shard_id does not resolve to an active identity in the naming plane"
            }
        }
    }
}

impl IntoResponse for TargetError {
    fn into_response(self) -> axum::response::Response {
        (
            self.status(),
            Json(json!({ "error": self.code(), "detail": self.message() })),
        )
            .into_response()
    }
}

/// A PIAL is a UUID. Nothing else is.
fn is_pial_uuid(v: &str) -> bool {
    let b = v.as_bytes();
    if b.len() != 36 {
        return false;
    }
    for (i, c) in b.iter().enumerate() {
        let ok = match i {
            8 | 13 | 18 | 23 => *c == b'-',
            _ => c.is_ascii_hexdigit(),
        };
        if !ok {
            return false;
        }
    }
    true
}

/// A NEXUS persona shard is 64 hex characters — `sha256` hex, as produced by
/// `PersonaShardFromPIAL`. Recognising it is what lets herald refuse it by name
/// rather than as a generic malformed string, so the caller is told which
/// mistake it made. Case is accepted either way: a shard that arrives uppercased
/// is still a shard, and should be named as one rather than falling through to
/// "malformed".
fn is_nexus_shard(v: &str) -> bool {
    v.len() == 64 && v.bytes().all(|c| c.is_ascii_hexdigit())
}

/// Classify an inbound identifier and, if it is an identity, produce its
/// canonical Manhattan name. No network call; this is the shape gate.
pub fn pial_target(raw: &str) -> Result<String, TargetError> {
    let v = raw.trim();
    if v.is_empty() {
        return Err(TargetError::Empty);
    }
    if is_pial_uuid(v) {
        return Ok(pial_name(&v.to_ascii_lowercase()));
    }
    if is_nexus_shard(v) {
        return Err(TargetError::NexusShard);
    }
    Err(TargetError::Malformed)
}

/// Shape-gate an identifier and then resolve its name through Manhattan.
pub async fn resolve_target(manhattan: &Manhattan, raw: &str) -> Result<Resolution, TargetError> {
    let name = pial_target(raw)?;

    match manhattan.resolve(&name).await {
        Ok(node) if node.status == "active" => Ok(Resolution::Verified(name)),
        Ok(node) => {
            warn!(
                name = %name,
                status = %node.status,
                "identity resolves but is not active — refusing to treat it as a push target"
            );
            Err(TargetError::Unknown)
        }
        Err(ManhattanError::NotFound) => Err(TargetError::Unknown),
        // MANHATTAN_URL unset. The drain announces this once at boot; repeating
        // it per request would drown the log without adding a fact.
        Err(ManhattanError::NotConfigured) => Ok(Resolution::Unverified {
            name,
            reason: "MANHATTAN_URL is not set".to_string(),
        }),
        Err(e) => {
            let reason = e.to_string();
            warn!(name = %name, error = %reason, "naming plane unreachable — proceeding unverified");
            Ok(Resolution::Unverified { name, reason })
        }
    }
}
