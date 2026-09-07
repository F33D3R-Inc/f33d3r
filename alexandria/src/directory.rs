//! Author attribution, resolved rather than remembered.
//!
//! An author is an identity: `pial:<uuid>`, the root that never moves. The
//! handle a catalog page prints beside a title is a pointer that registry-brain
//! owns and may transfer, sell or reclaim at any time. alexandria used to keep
//! its own copy of that pointer, which meant a transfer left the catalog
//! attributing titles to whoever held the handle last.
//!
//! So the copy is gone and the pointer is looked up. `resolve_batch` answers a
//! whole page of identity names in one round trip — the cheap lookup whose
//! absence is why the handle was copied in the first place.
//!
//! Two round trips are needed rather than one, and the reason is worth stating:
//! `resolve` maps a name to a node, but Manhattan has no batch endpoint for the
//! reverse direction — the names a set of nodes answers to. So this reads the
//! page's distinct identities in one batch and then fetches each distinct
//! node's live names, bounded and concurrent. The fan-out is over DISTINCT
//! AUTHORS, not over rows: a page of a hundred titles by three authors costs
//! one batch plus three lookups, never a lookup per row.

use std::collections::{HashMap, HashSet};

use uuid::Uuid;

use manhattan_client::{pial_name, Manhattan, ManhattanError, NS_HANDLE};

/// How many node lookups are in flight at once. Distinct authors on one page,
/// not rows, so this ceiling is generous.
const FANOUT: usize = 16;

/// What the naming plane says about one author, at the moment it was asked.
#[derive(Debug, Clone, serde::Serialize)]
pub struct AuthorIdentity {
    /// Manhattan's id for the identity node. Carried so a caller can follow
    /// edges from it; never stored in an alexandria row.
    pub node_id: String,
    /// The handle this identity currently answers to, or `None` when it holds
    /// no active handle. `None` is a fact about the identity, not a failure to
    /// look it up — a failure is an error and is returned as one.
    pub handle: Option<String>,
}

/// Resolve a page's authors in one batch plus a bounded fan-out.
///
/// Identities that do not resolve are absent from the result: a name that
/// Manhattan does not know is a fact, and the caller renders the title without
/// a handle. A transport or configuration failure is NOT that, and comes back
/// as an error so the caller can refuse to serve stale-looking attribution.
pub async fn resolve_authors(
    manhattan: &Manhattan,
    pial_ids: impl IntoIterator<Item = Uuid>,
) -> Result<HashMap<Uuid, AuthorIdentity>, ManhattanError> {
    let mut seen: HashSet<Uuid> = HashSet::new();
    let mut wanted: Vec<Uuid> = Vec::new();
    for id in pial_ids {
        if seen.insert(id) {
            wanted.push(id);
        }
    }
    if wanted.is_empty() {
        return Ok(HashMap::new());
    }

    let names: Vec<String> = wanted.iter().map(|id| pial_name(&id.to_string())).collect();
    let resolved = manhattan.resolve_batch(&names).await?;

    // Pair each PIAL back to the node its name resolved to. Names Manhattan did
    // not know simply do not appear.
    let mut pairs: Vec<(Uuid, String)> = Vec::with_capacity(resolved.len());
    for (pial_id, name) in wanted.iter().zip(names.iter()) {
        if let Some(node) = resolved.get(name) {
            pairs.push((*pial_id, node.node_id.clone()));
        }
    }

    let mut out: HashMap<Uuid, AuthorIdentity> = HashMap::with_capacity(pairs.len());
    for chunk in pairs.chunks(FANOUT) {
        let mut set = tokio::task::JoinSet::new();
        for (pial_id, node_id) in chunk.iter().cloned() {
            let mh = manhattan.clone();
            set.spawn(async move {
                let names = mh.get_node(&node_id).await;
                (pial_id, node_id, names)
            });
        }
        while let Some(joined) = set.join_next().await {
            // A panicked lookup task is a bug in this process, not a missing
            // name, and must not be reported as either success or "no handle".
            let (pial_id, node_id, result) = joined
                .map_err(|e| ManhattanError::Transport(format!("author lookup task: {e}")))?;
            let node = result?;
            out.insert(
                pial_id,
                AuthorIdentity {
                    node_id,
                    handle: node.name_in(NS_HANDLE),
                },
            );
        }
    }

    Ok(out)
}
