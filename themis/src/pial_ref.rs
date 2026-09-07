//! `PialRef` — the one definition of "a PIAL" inside themis.
//!
//! # Why this type exists
//!
//! Themis stores an identity reference in fifteen columns under eight different
//! names, in two different Postgres types:
//!
//!   UUID columns   shops.seller_pial, listings.shop_pial, purchases.buyer_pial,
//!                  marketplace_subscriptions.buyer_pial / .seller_pial
//!   TEXT columns   creator_eligibility.pial_id, subscription_plans.creator_pial_id,
//!                  subscriptions.subscriber_pial_id / .creator_pial_id,
//!                  ppv_items.creator_pial_id, ppv_purchases.buyer_pial_id,
//!                  tips.sender_pial_id / .recipient_pial_id,
//!                  commerce_transactions.payer_pial_id / .creator_pial_id
//!
//! Eight spellings for one concept is why nothing can be joined across brains
//! with confidence, and this is a database that moves money: paying the wrong
//! identity because two columns disagreed about what a PIAL is would be the
//! worst failure this codebase can produce.
//!
//! Renaming fifteen columns in one pass across a money-handling database is a
//! destructive, wide, hard-to-reverse change and is deliberately NOT done here.
//! Instead every one of those columns is read into and written from this single
//! type, so there is exactly one Rust-level definition of a PIAL while the
//! column names still differ. The rename becomes a mechanical follow-up rather
//! than a redefinition.
//!
//! # Canonical form
//!
//! A PIAL is a UUID. This type holds it as a `Uuid` — not a `String` — so the
//! spelling of the text form can never be a source of disagreement. The two
//! accessors below exist because the columns have two Postgres types, and that
//! split is intentionally visible at every bind site rather than hidden behind
//! an `Encode` impl that would silently pick the wrong wire type:
//!
//!   `.uuid()`  bind to a UUID column
//!   `.text()`  bind to a TEXT column — always lowercase and hyphenated
//!   `.name()`  the Manhattan name, `pial:<uuid>`
//!
//! Reading is the other direction and needs no such split: `Decode` accepts
//! either Postgres type, so `try_get::<PialRef, _>` works on all fifteen.
//!
//! # Resolution
//!
//! Themis does not own identity — elohim-veni does (manhattan/migrations/
//! 0002_authority_map.sql). So a PIAL this brain is about to pay, subscribe,
//! or unlock content for is a NAME that must be resolved through Manhattan,
//! never a row this brain assumed. `require_identity` is that gate, and it is
//! the only place the policy is written down.

use std::sync::atomic::{AtomicBool, Ordering};

use serde::{de, Deserialize, Deserializer, Serialize, Serializer};
use sqlx::{
    error::BoxDynError,
    postgres::{PgTypeInfo, PgValueRef},
    Decode, Postgres, Type, ValueRef,
};
use uuid::Uuid;

use manhattan_client::{pial_name, Manhattan, ManhattanError, KIND_IDENTITY};

// ── The type ──────────────────────────────────────────────────────────────────

/// A reference to an identity. Never an identity itself: themis holds
/// references, elohim-veni holds the identity, Manhattan holds the mapping.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct PialRef(Uuid);

impl PialRef {
    /// Parse a PIAL from any text form. Stored form is always canonical, so two
    /// spellings of the same identity can never become two rows.
    pub fn parse(s: &str) -> Result<Self, PialParseError> {
        Uuid::parse_str(s.trim())
            .map(PialRef)
            .map_err(|_| PialParseError(s.to_string()))
    }

    /// Bind to a UUID column.
    pub fn uuid(&self) -> Uuid {
        self.0
    }

    /// Bind to a TEXT column. Canonical lowercase hyphenated form, always.
    pub fn text(&self) -> String {
        self.0.hyphenated().to_string()
    }

    /// This identity's Manhattan name. The root that never moves — a handle is
    /// a transferable pointer and is never used in its place.
    pub fn name(&self) -> String {
        pial_name(&self.text())
    }
}

impl std::fmt::Display for PialRef {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.0.hyphenated())
    }
}

// ── Parse errors ──────────────────────────────────────────────────────────────

#[derive(Debug)]
pub struct PialParseError(String);

impl std::fmt::Display for PialParseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // The offending value is not echoed: a PIAL is an identity reference and
        // does not belong in an error string that may be logged or returned.
        let _ = &self.0;
        write!(f, "not a PIAL: expected a UUID identity reference")
    }
}

impl std::error::Error for PialParseError {}

// ── serde ─────────────────────────────────────────────────────────────────────

impl Serialize for PialRef {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&self.text())
    }
}

impl<'de> Deserialize<'de> for PialRef {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl de::Visitor<'_> for V {
            type Value = PialRef;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("a PIAL identity reference (UUID)")
            }
            fn visit_str<E: de::Error>(self, v: &str) -> Result<PialRef, E> {
                PialRef::parse(v).map_err(de::Error::custom)
            }
        }
        d.deserialize_str(V)
    }
}

// ── sqlx: one type, both column shapes ────────────────────────────────────────

impl Type<Postgres> for PialRef {
    fn type_info() -> PgTypeInfo {
        <Uuid as Type<Postgres>>::type_info()
    }

    /// Accept both column shapes on read. This is the compatibility the fifteen
    /// columns need today and the reason a single type can already stand behind
    /// all of them, before any of them is renamed or retyped.
    fn compatible(ty: &PgTypeInfo) -> bool {
        <Uuid as Type<Postgres>>::compatible(ty) || <String as Type<Postgres>>::compatible(ty)
    }
}

impl<'r> Decode<'r, Postgres> for PialRef {
    fn decode(value: PgValueRef<'r>) -> Result<Self, BoxDynError> {
        let ty = value.type_info().into_owned();
        if <Uuid as Type<Postgres>>::compatible(&ty) {
            Ok(PialRef(<Uuid as Decode<Postgres>>::decode(value)?))
        } else {
            // A TEXT column holds whatever a writer put there. A value that is
            // not a UUID is a corrupt identity reference and is surfaced as a
            // decode error rather than carried forward into a payment.
            let raw = <String as Decode<Postgres>>::decode(value)?;
            Ok(PialRef::parse(&raw)?)
        }
    }
}

// ── Content addresses ─────────────────────────────────────────────────────────

/// A reference to a piece of content. Themis does not own works — feed-engine
/// does — so it references them by name and resolves the name; it never assumes
/// another brain's id scheme.
///
/// Both spellings below are DERIVED names (manhattan/migrations/
/// 0004_allocated_namespaces.sql): computed from the entity itself, so two
/// brains that compute one always agree and whichever meets the content first
/// may bind it. That is exactly why themis may register these and may not
/// register a handle.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ContentRef {
    /// `uuid:<id>` — an entity id.
    Uuid(Uuid),
    /// `cid:sha256:<hex>` — the same thing by content address.
    Sha256(String),
}

impl ContentRef {
    /// Parse a content reference out of the free-text content identifiers this
    /// brain stores (`listings.content_hash`, `ppv_items.content_id`,
    /// `tips.content_id`). Returns None for anything that cannot be named,
    /// because an unnameable reference must not become a fabricated name.
    pub fn parse(s: &str) -> Option<Self> {
        let s = s.trim();
        if s.is_empty() {
            return None;
        }
        if let Ok(id) = Uuid::parse_str(s) {
            return Some(ContentRef::Uuid(id));
        }
        let hex = s.strip_prefix("sha256:").unwrap_or(s);
        if hex.len() == 64 && hex.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Some(ContentRef::Sha256(hex.to_ascii_lowercase()));
        }
        None
    }

    pub fn name(&self) -> String {
        match self {
            ContentRef::Uuid(id) => manhattan_client::uuid_name(&id.hyphenated().to_string()),
            ContentRef::Sha256(hex) => manhattan_client::cid_name(&format!("sha256:{hex}")),
        }
    }
}

// ── The identity gate ─────────────────────────────────────────────────────────

/// Why a PIAL could not be established as an identity Manhattan agrees exists.
#[derive(Debug)]
pub enum IdentityError {
    /// The name resolves to a node that is not active. A revoked or tombstoned
    /// identity must never be paid, subscribed to, or handed a content key.
    NotActive,
    /// The name resolves to something that is not an identity at all. One name
    /// is one node, so this means the `pial:` namespace has been minted into by
    /// something that had no business doing so — never a value to pay against.
    NotAnIdentity(String),
    /// The naming plane is configured but could not answer. Refusing is the
    /// safe direction: the registration is already queued durably in the
    /// outbox, so the caller loses nothing by retrying.
    Unreachable(String),
}

impl std::fmt::Display for IdentityError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            IdentityError::NotActive => write!(f, "identity is not active"),
            IdentityError::NotAnIdentity(kind) => {
                write!(f, "name resolves to a {kind} node, not an identity")
            }
            IdentityError::Unreachable(e) => write!(f, "identity could not be resolved: {e}"),
        }
    }
}

impl std::error::Error for IdentityError {}

static UNCONFIGURED_LOGGED: AtomicBool = AtomicBool::new(false);

/// The single gate every money, payout, subscription and content-key path in
/// this brain passes through before it acts on a PIAL.
///
/// `ensure_node` resolves the name and registers it if nothing answers to it
/// yet. Registration is not ownership: Manhattan assigns the owner from the
/// node's kind, so an identity registered here is owned by elohim-veni exactly
/// as it would be if elohim-veni had registered it first. Which is the point —
/// whoever encounters an identity first records it, and there is still only
/// ever one node behind the name.
///
/// The one branch that does not refuse is `NotConfigured`: a deployment that
/// has not been wired to the naming plane yet keeps accumulating registrations
/// in the transactional outbox and delivers them whole once it is. Nothing is
/// lost and nothing is silently skipped — the drain says so loudly at startup,
/// and this says so once.
pub async fn require_identity(manhattan: &Manhattan, pial: &PialRef) -> Result<(), IdentityError> {
    match manhattan.ensure_node(KIND_IDENTITY, &pial.name()).await {
        Ok(node) if node.kind != KIND_IDENTITY => Err(IdentityError::NotAnIdentity(node.kind)),
        Ok(node) if node.status == "active" => Ok(()),
        Ok(_) => Err(IdentityError::NotActive),
        Err(ManhattanError::NotConfigured) => {
            if !UNCONFIGURED_LOGGED.swap(true, Ordering::Relaxed) {
                tracing::warn!(
                    "MANHATTAN_URL is not set: identities are being queued in manhattan_outbox \
                     but not resolved — payment paths are proceeding on unresolved names"
                );
            }
            Ok(())
        }
        Err(e) => Err(IdentityError::Unreachable(e.to_string())),
    }
}

/// Register a content reference by name so the thing this brain sells has one
/// identity across the platform rather than one id per brain. Best effort by
/// design: a listing must not fail to be created because the naming plane is
/// briefly unreachable — the outbox row written in the same transaction as the
/// row itself is what makes this durable, and this call is only the fast path.
pub async fn register_content(manhattan: &Manhattan, kind: &str, content: &ContentRef) {
    if !manhattan.configured() {
        return;
    }
    if let Err(e) = manhattan.ensure_node(kind, &content.name()).await {
        tracing::warn!(
            "manhattan: registering {}: {e} (queued in outbox)",
            content.name()
        );
    }
}
