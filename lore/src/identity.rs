//! How lore turns whatever a caller sent into an identity Manhattan resolved.
//!
//! Lore does not own identity. `elohim-veni` does — `node_kind_owners` in
//! Manhattan's migration 0002 says so, and Manhattan refuses identity facts
//! from anyone else. So every XP total, achievement grant and progression row
//! in `f33d3r_lore` references a NAME, and that name is resolved here.
//!
//! # The two vocabularies
//!
//! Lore's tables were keyed on `pial_shard_id`, a second name for the same
//! concept that only lore, herald and verity's NEXUS tables use. It is not a
//! different concept:
//!
//!     pial_shard_id = hex(sha256(pial_id || ":nexus-pial-shard-v1"))
//!
//! defined identically in `elohim-veni/src/handlers.rs`, in
//! `feed-engine/internal/nexus/session.go`, and in SQL via pgcrypto in
//! `feed-engine/internal/handler/handlers.go`. It is a total, deterministic,
//! one-way function of a single PIAL. `verity`'s `nexus_persona_links` carries
//! `UNIQUE (pial_shard_id)`, so a shard belongs to exactly one NEXUS and stands
//! for exactly one PIAL. The multi-persona layer sits ABOVE the PIAL — one
//! NEXUS groups many PIALs — so a shard is a blinded ALIAS of an identity, not
//! a persona beneath one.
//!
//! That makes a shard a name, in its own namespace, for a node that already has
//! a `pial:` name. It is never a second identity, and this module never mints
//! one from a shard.
//!
//! # Why a shard cannot be resolved locally
//!
//! sha256 does not run backwards. A brain holding only a shard cannot recover
//! the PIAL behind it, which is exactly why the shard had to be copied into
//! every table that wanted to name a person. Manhattan is the way out: bind
//! `shard:<hex>` as an additional name on the identity node and the shard
//! resolves like any other name.
//!
//! Lore may not perform that binding. `shard` claims no namespace authority, so
//! Manhattan falls back to node ownership, and identity nodes belong to
//! elohim-veni — the answer is 403. So lore resolves shards and never writes
//! them. A shard sent here before someone has bound it fails loudly with 404
//! rather than being reinterpreted as a PIAL, because reinterpreting it would
//! credit one person's XP to a name that is not theirs.

use manhattan_client::{handle_name, name as compose_name, pial_name, Manhattan, ManhattanError};

/// The NEXUS blinded-identity namespace. Unclaimed in `namespace_authorities`,
/// which is what lets a shard become a name rather than a parallel identifier.
pub const NS_SHARD: &str = "shard";

pub fn shard_name(shard: &str) -> String {
    compose_name(NS_SHARD, shard)
}

/// The value half of a `<namespace>:<value>` name.
pub fn bare_value(name: &str) -> &str {
    name.split_once(':').map(|(_, v)| v).unwrap_or(name)
}

/// Why an identity reference could not become a name lore may store.
///
/// Three outcomes, deliberately distinct: the caller sent something that is not
/// a name, the name does not resolve, or the resolver could not be reached.
/// Collapsing the third into the second is how a naming plane starts silently
/// losing people.
#[derive(Debug)]
pub enum ResolveError {
    Malformed(String),
    Unresolved(String),
    Unavailable(String),
}

impl std::fmt::Display for ResolveError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ResolveError::Malformed(m) => write!(f, "{m}"),
            ResolveError::Unresolved(m) => write!(f, "{m}"),
            ResolveError::Unavailable(m) => write!(f, "{m}"),
        }
    }
}

impl std::error::Error for ResolveError {}

/// What a caller handed us, once it has been recognised.
enum Inbound {
    Pial(String),
    Shard(String),
    Handle(String),
}

fn is_uuid(s: &str) -> bool {
    if s.len() != 36 {
        return false;
    }
    s.chars().enumerate().all(|(i, c)| match i {
        8 | 13 | 18 | 23 => c == '-',
        _ => c.is_ascii_hexdigit(),
    })
}

fn is_shard(s: &str) -> bool {
    s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit())
}

fn is_handle(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 64
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-' || c == '.')
}

/// Recognise a reference. A bare value is accepted because the shape of a PIAL
/// and the shape of a shard cannot be confused — 36 characters with dashes
/// against 64 hex characters — and callers written before the naming plane
/// existed send bare values.
fn classify(raw: &str) -> Option<Inbound> {
    let raw = raw.trim();
    if raw.is_empty() {
        return None;
    }
    let (namespace, value) = match raw.split_once(':') {
        Some((ns, v)) => (ns.to_ascii_lowercase(), v.to_string()),
        None => (String::new(), raw.to_string()),
    };
    let value = value.trim().to_ascii_lowercase();

    match namespace.as_str() {
        "pial" if is_uuid(&value) => Some(Inbound::Pial(value)),
        "shard" if is_shard(&value) => Some(Inbound::Shard(value)),
        "handle" if is_handle(&value) => Some(Inbound::Handle(value)),
        "" if is_uuid(&value) => Some(Inbound::Pial(value)),
        "" if is_shard(&value) => Some(Inbound::Shard(value)),
        _ => None,
    }
}

/// Resolve an identity reference to the canonical `pial:<uuid>` name that lore
/// stores. The returned string is the only identity key lore writes to disk.
pub async fn resolve(manhattan: &Manhattan, raw: &str) -> Result<String, ResolveError> {
    let inbound = classify(raw).ok_or_else(|| {
        ResolveError::Malformed(format!(
            "{raw:?} is not an identity name: expected pial:<uuid>, shard:<64-hex>, \
             handle:<handle>, a bare PIAL uuid, or a bare 64-hex NEXUS shard"
        ))
    })?;

    match inbound {
        // A PIAL is already the canonical name, so resolution here is a check,
        // not a translation. Manhattan not knowing this identity yet is not an
        // error: any brain may be the first to encounter someone, and the
        // outbox trigger registers the node in the same transaction as the row
        // that needed it.
        Inbound::Pial(id) => {
            let name = pial_name(&id);
            match manhattan.resolve(&name).await {
                Ok(node) if node.status == "active" => Ok(name),
                Ok(node) => Err(ResolveError::Unresolved(format!(
                    "{name} resolves to a node whose status is {}",
                    node.status
                ))),
                Err(ManhattanError::NotFound) | Err(ManhattanError::NotConfigured) => Ok(name),
                Err(e) => Err(ResolveError::Unavailable(e.to_string())),
            }
        }
        // An alias carries no PIAL of its own. It resolves or it fails; there is
        // no local fallback, because the local fallback is the drift.
        Inbound::Shard(hex) => resolve_alias(manhattan, shard_name(&hex)).await,
        Inbound::Handle(handle) => resolve_alias(manhattan, handle_name(&handle)).await,
    }
}

async fn resolve_alias(manhattan: &Manhattan, name: String) -> Result<String, ResolveError> {
    match manhattan.resolve_pial(&name).await {
        Ok(pial) => Ok(pial_name(&pial)),
        Err(ManhattanError::NotFound) => Err(ResolveError::Unresolved(format!(
            "{name} does not resolve to an identity"
        ))),
        Err(ManhattanError::NotConfigured) => Err(ResolveError::Unavailable(format!(
            "{name} is an alias and can only be resolved through Manhattan, \
             but MANHATTAN_URL is not set"
        ))),
        Err(e) => Err(ResolveError::Unavailable(e.to_string())),
    }
}
