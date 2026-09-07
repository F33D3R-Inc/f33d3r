//! How Auralis turns whatever a caller sent into an identity Manhattan resolved.
//!
//! Auralis does not own identity. `elohim-veni` does — `node_kind_owners` in
//! Manhattan's migration 0002 says so, and Manhattan refuses identity facts
//! from anyone else. So every host, participant, request and block row in
//! `f33d3r_frequencies` references a NAME, and that name is resolved here.
//!
//! This module is lore/src/identity.rs with the brain's name changed. The
//! shard namespace is kept: NEXUS-facing callers send `shard:<64-hex>` and a
//! shard resolves like any other alias — or fails loudly, never by being
//! reinterpreted as a PIAL.

use manhattan_client::{handle_name, name as compose_name, pial_name, Manhattan, ManhattanError};

/// The NEXUS blinded-identity namespace.
pub const NS_SHARD: &str = "shard";

pub fn shard_name(shard: &str) -> String {
    compose_name(NS_SHARD, shard)
}

/// The value half of a `<namespace>:<value>` name.
pub fn bare_value(name: &str) -> &str {
    name.split_once(':').map(|(_, v)| v).unwrap_or(name)
}

/// Why an identity reference could not become a name Auralis may store.
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

enum Inbound {
    Pial(String),
    Shard(String),
    Handle(String),
}

pub fn is_uuid(s: &str) -> bool {
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

/// Resolve an identity reference to the canonical `pial:<uuid>` name Auralis
/// stores. The returned string is the only identity key this brain writes.
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bare_value_strips_namespace() {
        assert_eq!(bare_value("pial:abc"), "abc");
        assert_eq!(bare_value("abc"), "abc");
    }

    #[test]
    fn uuid_shape() {
        assert!(is_uuid("c0ffee00-0000-4000-8000-000000000001"));
        assert!(!is_uuid("c0ffee00-0000-4000-8000-00000000000"));
        assert!(!is_uuid("not-a-uuid"));
    }
}
