//! Wire and row types for the naming and graph plane.
//!
//! Every name column is read back as `name::text` — the column itself is CITEXT so
//! resolution is case-insensitive, and the cast keeps the decode side plain text.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use uuid::Uuid;

// ── Vocabulary ────────────────────────────────────────────────────────────────

/// The only kinds a node may have. Mirrors the CHECK constraint in migration 0001.
pub const KINDS: [&str; 8] = [
    "identity", "work", "media", "key", "address", "facet",
    // A verified natural person, who may hold several identities. Owned by
    // verity — see migration 0003. Deliberately not an identity: conflating the
    // person with the account is how a multi-persona platform leaks one persona
    // into another.
    "nexus",
    // A speakable, rotatable contact name resolving to an identity — see
    // migration 0006. Distinct from `address`: a Number carries a contact policy
    // and is never a bearer secret.
    "number",
];

/// The only predicates an edge may carry. Mirrors the CHECK constraint in migration 0001.
pub const PREDICATES: [&str; 7] = [
    "quotes",
    "replies_to",
    "authored_by",
    "derived_from",
    "resolves_to",
    "signs_for",
    "contains",
];

// ── Row types ─────────────────────────────────────────────────────────────────

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct NodeRow {
    pub node_id: Uuid,
    pub kind: String,
    pub owner: String,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct NameRow {
    pub name: String,
    pub node_id: Uuid,
    pub namespace: String,
    pub is_primary: bool,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub revoked_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct EdgeRow {
    pub subject: Uuid,
    pub predicate: String,
    pub object: Uuid,
    pub root: Uuid,
    pub depth: i32,
    pub created_at: DateTime<Utc>,
}

// ── Requests ──────────────────────────────────────────────────────────────────

/// Create a node, optionally binding its primary name in the same transaction.
/// `name` must be the fully namespaced form, `<namespace>:<value>`.
#[derive(Debug, Deserialize)]
pub struct CreateNodeReq {
    pub kind: String,
    /// Ignored. Ownership is derived from the node's kind via the authority map
    /// (migration 0002), because a brain asserting its own ownership is how two
    /// stores of one truth get created, each convinced it is authoritative.
    ///
    /// The field is accepted and discarded rather than rejected so that callers
    /// written against the pre-0002 contract keep working instead of failing
    /// with a 422 that says nothing about why.
    #[serde(default)]
    pub owner: Option<String>,
    pub name: Option<String>,
    pub namespace: Option<String>,
}

/// Bind an additional name to an existing node.
#[derive(Debug, Deserialize)]
pub struct CreateNameReq {
    pub name: String,
    pub namespace: String,
    pub node_id: Uuid,
    #[serde(default)]
    pub is_primary: bool,
}

/// Resolve many names in one call. This is what kills the denormalised-handle copying:
/// one round trip resolves every name on a page.
#[derive(Debug, Deserialize)]
pub struct ResolveBatchReq {
    pub names: Vec<String>,
}

#[derive(Debug, Deserialize)]
pub struct EdgeReq {
    pub subject: Uuid,
    pub predicate: String,
    pub object: Uuid,
}

#[derive(Debug, Deserialize)]
pub struct CreateAddressReq {
    pub identity_node_id: Uuid,
}

// ── Responses ─────────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct NodeResp {
    pub node: NodeRow,
    pub names: Vec<NameRow>,
}

/// The hot path's answer: one name, one node, one indexed lookup — or one
/// cache hit, see `cache.rs`.
#[derive(Debug, Clone, Serialize, sqlx::FromRow)]
pub struct ResolveResp {
    pub name: String,
    pub node_id: Uuid,
    pub kind: String,
    pub owner: String,
    pub status: String,
}

#[derive(Debug, Serialize)]
pub struct ResolveBatchResp {
    pub resolved: BTreeMap<String, ResolveResp>,
    pub missing: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct ThreadResp {
    pub root: Uuid,
    pub max_depth: i32,
    pub count: usize,
    /// Present when this page was full: pass it back as `after` for the next
    /// page. Absent means the listing is complete.
    pub next_cursor: Option<String>,
    pub edges: Vec<EdgeRow>,
}

/// A freshly minted address. `address` is the shareable `XXXX-XXXX` form.
#[derive(Debug, Serialize)]
pub struct AddressResp {
    pub address: String,
    pub address_node_id: Uuid,
    pub identity_node_id: Uuid,
    pub status: String,
    pub created_at: DateTime<Utc>,
}

/// The capability check. Deliberately narrow: possession of a live address proves the
/// right to reach this identity, and nothing about the identity's other names.
#[derive(Debug, Serialize)]
pub struct AddressResolveResp {
    pub identity_node_id: Uuid,
    pub status: String,
}

#[derive(Debug, Serialize)]
pub struct AddressRevokeResp {
    pub address: String,
    pub address_node_id: Uuid,
    pub identity_node_id: Option<Uuid>,
    pub status: String,
    pub revoked_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Serialize)]
pub struct AddressListItem {
    pub address: String,
    pub address_node_id: Uuid,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub revoked_at: Option<DateTime<Utc>>,
}

// ── Internal query rows ───────────────────────────────────────────────────────

/// Row shape behind `GET /v1/identities/:node_id/addresses` before the namespace prefix
/// is stripped from the stored name.
#[derive(Debug, sqlx::FromRow)]
pub struct AddressListRow {
    pub name: String,
    pub address_node_id: Uuid,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub revoked_at: Option<DateTime<Utc>>,
}

// ── Numbers ───────────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct CreateNumberReq {
    pub identity_node_id: Uuid,
    /// A Number this identity previously held and wants back.
    ///
    /// Absent is the ordinary case: draw a fresh Number. Present is a reclaim,
    /// and it is honoured only for a Number this identity really did hold — see
    /// `db::name_was_held_by` for why that restriction is what keeps the endpoint
    /// from answering "is this Number free?" for names belonging to strangers.
    ///
    /// A reclaim is a mint in every other respect: it counts against the
    /// concurrent cap, it counts against the minting rate, and the Number comes
    /// back as a NEW node carrying none of its previous policy, label, lease or
    /// admissions.
    #[serde(default)]
    pub reclaim: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct NumberResp {
    pub number: String,
    pub number_node_id: Uuid,
    pub identity_node_id: Uuid,
    pub status: String,
    pub created_at: DateTime<Utc>,
}

/// Uniform for live, revoked, unknown and malformed alike: same fields, same
/// status code, same minimum duration. Only `found` differs, and it is false for
/// everything that is not a live Number.
#[derive(Debug, Serialize)]
pub struct NumberResolveResp {
    pub found: bool,
    pub number_node_id: Option<Uuid>,
    pub identity_node_id: Option<Uuid>,
}

#[derive(Debug, Serialize)]
pub struct NumberRevokeResp {
    pub number: String,
    pub number_node_id: Uuid,
    pub identity_node_id: Option<Uuid>,
    pub status: String,
    pub revoked_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Serialize)]
pub struct NumberListItem {
    pub number: String,
    pub number_node_id: Uuid,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub revoked_at: Option<DateTime<Utc>>,
    /// Whether this is one of the Crockford base32 Numbers minted before the
    /// digit format. Owner-facing only: it travels on the OWNER'S OWN listing so
    /// somebody holding an older Number can be shown that it is older and choose
    /// to mint a new one. No resolve carries it, and no refusal mentions it.
    pub legacy: bool,
}

#[derive(Debug, sqlx::FromRow)]
pub struct NumberListRow {
    pub name: String,
    pub number_node_id: Uuid,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub revoked_at: Option<DateTime<Utc>>,
}

// ── Associations ──────────────────────────────────────────────────────────────

/// One recorded association. No root, no depth: an association has no lineage,
/// which is exactly why it does not live in `edges` — see migration 0007.
#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct AssocRow {
    pub subject: Uuid,
    pub assoc: String,
    pub object: Uuid,
    pub created_at: DateTime<Utc>,
}

/// Write or retract an association.
///
/// Name-addressed, unlike `EdgeReq`, and deliberately so. An association is
/// asserted by a brain that owns NEITHER endpoint node — feed-engine says who
/// follows whom, but identity nodes belong to elohim-veni — so requiring node
/// ids would force that brain to first learn and then carry another brain's
/// primary keys. Names are the only identifier it is allowed to hold, so names
/// are what this endpoint takes and what Manhattan resolves.
#[derive(Debug, Deserialize)]
pub struct AssocReq {
    pub assoc: String,
    pub subject_name: String,
    pub object_name: String,
}

/// What a write or retraction did, in the caller's own vocabulary of names.
#[derive(Debug, Serialize)]
pub struct AssocResp {
    pub assoc: String,
    pub subject_name: String,
    pub object_name: String,
    pub subject: Uuid,
    pub object: Uuid,
    pub created_at: Option<DateTime<Utc>>,
    /// False when the write was a replay of one already recorded, or a
    /// retraction of one that was already absent. Either way the end state is
    /// the one the caller asked for; this says whether this call is what
    /// produced it.
    pub changed: bool,
}

/// Both directions between two nodes, answered in one round trip.
///
/// `subject_resolved` and `object_resolved` are reported rather than folded into
/// a 404, because "this identity is not in the plane yet" and "these two are not
/// associated" are different facts and a security decision must be able to tell
/// them apart — both refuse, but only one of them is a bug worth logging.
#[derive(Debug, Serialize)]
pub struct AssocBetweenResp {
    pub assoc: String,
    pub subject_name: String,
    pub object_name: String,
    pub subject_resolved: bool,
    pub object_resolved: bool,
    pub forward: bool,
    pub reverse: bool,
    pub mutual: bool,
    pub forward_since: Option<DateTime<Utc>>,
    pub reverse_since: Option<DateTime<Utc>>,
}

/// One side of a node's associations, newest first. `entries` name the other
/// endpoint by its primary name where it has one.
#[derive(Debug, Serialize)]
pub struct AssocRangeResp {
    pub assoc: String,
    pub name: String,
    pub node_id: Uuid,
    pub side: &'static str,
    pub count: usize,
    /// Present when this page was full: pass it back as `after` for the next
    /// page. Absent means the listing is complete.
    pub next_cursor: Option<String>,
    pub entries: Vec<crate::db::AssocListRow>,
}

/// How many associations a node has on each side.
#[derive(Debug, Serialize)]
pub struct AssocCountResp {
    pub assoc: String,
    pub name: String,
    pub node_id: Uuid,
    pub out: i64,
    pub r#in: i64,
}

// ── Batch application ─────────────────────────────────────────────────────────

/// One outbox row, exactly as the brain's trigger wrote it: the op name and
/// the payload. The shapes are the ones every brain's `manhattan_outbox`
/// trigger produces, so a drain forwards rows verbatim and never rebuilds them.
#[derive(Debug, Clone, Deserialize)]
pub struct ApplyOp {
    pub op: String,
    #[serde(default)]
    pub payload: serde_json::Value,
}

#[derive(Debug, Deserialize)]
pub struct ApplyReq {
    pub ops: Vec<ApplyOp>,
}

/// What became of one op. `outcome` is the whole contract a drain acts on:
///
///   applied  — the write happened now.
///   already  — the end state the op wanted was already true; nothing to do.
///   retry    — Manhattan answered, and the answer is "not yet": a dependency
///              this queue has not delivered, a name held by another node, a
///              refusal that clears once something else is fixed. The op holds
///              its place in the queue and the attempt counts.
///   refused  — Manhattan answered, and no retry this queue could schedule will
///              change the answer. Quarantine it and carry on past it.
///   error    — Manhattan could not answer about this op: a fault on this side.
///              Hold the op's place; the attempt does not count.
///   skipped  — not attempted, because an earlier op in the batch came back
///              retry or error and order must hold. Untouched.
///
/// `code` is the same machine-readable code the single-op route would have
/// answered with, for the log line and the operator.
#[derive(Debug, Serialize)]
pub struct ApplyResult {
    pub outcome: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub code: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub node_id: Option<Uuid>,
}

#[derive(Debug, Serialize)]
pub struct ApplyResp {
    pub results: Vec<ApplyResult>,
}
