//! Query helpers for the naming and graph plane.
//!
//! The one piece of real logic here is edge placement: root and depth are materialised
//! inside the inserting transaction, so a thread read is `WHERE root = $1 AND depth <= N`
//! — one round trip, no recursion, and no way for the database to hand back a cycle.
//! Placement does not assume a parent arrives before its children: an edge whose
//! subject is the root of an existing tree re-roots that tree as it lands.
//!
//! Every statement here is exercised against a real Postgres by `db_tests.rs`.

use sqlx::{PgExecutor, Postgres, Transaction};
use uuid::Uuid;

use chrono::{DateTime, Utc};

use crate::models::{AssocRow, EdgeRow, NameRow, NodeRow};

/// A graph bounded by construction. An insert that would exceed this is refused, so no
/// reader ever has to defend against an unbounded walk.
pub const MAX_DEPTH: i32 = 64;

pub enum EdgeInsertError {
    /// The (subject, predicate, object) triple already exists. Uniqueness is structural.
    Duplicate,
    /// Placing this edge would put it, or something already placed beneath its
    /// subject, deeper than MAX_DEPTH. Carries the depth that was refused.
    TooDeep(i32),
    /// The object's chain already bottoms out at the subject, so this edge
    /// would close it into a loop. Refused for ever: no retry changes a cycle.
    Cycle,
    /// The chain was being re-rooted under this insert faster than it could be
    /// locked, or two inserts each waited on the other's chain. Transient; the
    /// caller should retry.
    Contended,
    Db(sqlx::Error),
}

impl From<sqlx::Error> for EdgeInsertError {
    fn from(e: sqlx::Error) -> Self {
        EdgeInsertError::Db(e)
    }
}

/// True when the error is a Postgres unique-violation (SQLSTATE 23505).
pub fn is_unique_violation(e: &sqlx::Error) -> bool {
    matches!(e, sqlx::Error::Database(db) if db.code().as_deref() == Some("23505"))
}

/// True when Postgres aborted this transaction to break a deadlock (SQLSTATE
/// 40P01). Between edge placements that only happens when two inserts would
/// together close a cycle; the survivor commits and the retry of the loser is
/// then refused as a cycle, or succeeds if the shape was not one after all.
pub fn is_deadlock(e: &sqlx::Error) -> bool {
    matches!(e, sqlx::Error::Database(db) if db.code().as_deref() == Some("40P01"))
}

/// Advisory-lock key derived from a node id. Every placement decision locks the
/// ROOT of each tree it touches (see `lock_chain_root`), so two inserts that
/// would move the same tree serialise on the same key.
fn lock_key(node_id: Uuid) -> i64 {
    let bytes = node_id.as_bytes();
    let mut head = [0u8; 8];
    head.copy_from_slice(&bytes[..8]);
    i64::from_be_bytes(head)
}

async fn lock_node(tx: &mut Transaction<'_, Postgres>, node_id: Uuid) -> Result<(), sqlx::Error> {
    sqlx::query("SELECT pg_advisory_xact_lock($1)")
        .bind(lock_key(node_id))
        .execute(&mut **tx)
        .await?;
    Ok(())
}

/// The shared form: held by every writer that adds a leaf UNDER a tree, so
/// that thousands of replies to one post place themselves concurrently. It
/// conflicts only with the exclusive lock the two rare writers take — an
/// insert that moves the whole tree (`insert_edge` with the root as subject)
/// and a delete inside it — and those wait for the leaves in flight, then
/// exclude everything while they run.
async fn lock_node_shared(
    tx: &mut Transaction<'_, Postgres>,
    node_id: Uuid,
) -> Result<(), sqlx::Error> {
    sqlx::query("SELECT pg_advisory_xact_lock_shared($1)")
        .bind(lock_key(node_id))
        .execute(&mut **tx)
        .await?;
    Ok(())
}

/// The edge a new child of `node` would be placed under: `node`'s outgoing edge
/// with the smallest depth, oldest first. `None` when `node` has no outgoing
/// edge under this predicate, in which case `node` is the bottom of its own
/// chain and a child of it is rooted at `node` itself.
///
/// The choice is sticky. A subject may carry several outgoing edges under one
/// predicate (a work quoting two works, a nexus containing two identities); the
/// parent chosen when a child is placed is the one it follows for ever, so the
/// DAG is projected onto one tree per root and every thread read stays a single
/// indexed range.
async fn first_outgoing(
    tx: &mut Transaction<'_, Postgres>,
    node: Uuid,
    predicate: &str,
) -> Result<Option<(Uuid, i32)>, sqlx::Error> {
    sqlx::query_as(
        "SELECT root, depth
           FROM edges
          WHERE subject = $1 AND predicate = $2
          ORDER BY depth ASC, created_at ASC
          LIMIT 1",
    )
    .bind(node)
    .bind(predicate)
    .fetch_optional(&mut **tx)
    .await
}

/// Bounds the lock-and-verify loop below. It only ever repeats when another
/// transaction re-rooted the chain between the read and the lock, and each
/// repeat waits for that transaction to finish, so the loop settles in one or
/// two turns in practice; the bound is the guarantee rather than the plan.
const CHAIN_LOCK_TURNS: usize = 16;

/// Takes a SHARED lock on the root of the chain `node` currently hangs from,
/// and returns the placement that chain offers a child of `node`.
///
/// The root of a chain is the ONE key every write that can move that chain
/// takes: an insert whose subject is the root (it makes the root hang from
/// something else, which re-roots the whole tree — see `insert_edge`) locks it
/// exclusively as its subject; an insert under the chain locks it here, shared,
/// so leaves under a hot root never wait on one another. But the root is
/// learned by reading, and between that read and the lock the chain can be
/// re-rooted by exactly the write this is meant to exclude. So the read is
/// repeated after the lock, and only a root that survived the lock counts.
/// A session never conflicts with its own locks, so a chain already held
/// exclusively as a subject is simply held again.
async fn lock_chain_root(
    tx: &mut Transaction<'_, Postgres>,
    node: Uuid,
    predicate: &str,
) -> Result<Option<(Uuid, i32)>, EdgeInsertError> {
    for _ in 0..CHAIN_LOCK_TURNS {
        let before = first_outgoing(tx, node, predicate).await?;
        let key = before.map(|(root, _)| root).unwrap_or(node);
        lock_node_shared(tx, key).await?;
        let after = first_outgoing(tx, node, predicate).await?;
        if after.map(|(root, _)| root).unwrap_or(node) == key {
            return Ok(after);
        }
    }
    Err(EdgeInsertError::Contended)
}

/// Creates an edge with root and depth materialised, and keeps every edge
/// already placed beneath its subject correct.
///
/// Placement rule: the parent of a new edge (S, P, O) is O's first outgoing
/// edge under P. With a parent, the new edge inherits its root and sits one
/// deeper; without one, the new edge is the bottom of a fresh chain rooted at O.
///
/// # Arrival order is not assumed
///
/// Edges reach this plane through a dozen independent outbox drains with no
/// ordering between them, so a child can land before its parent: (S, P, O)
/// arrives, is rooted at O with depth 1, and only later does (O, P, X) arrive
/// and place O under X. Every edge already rooted at O is then wrong — it
/// belongs to X's tree, one level deeper than it was — and nothing else would
/// ever revisit it. So when the new edge's subject is the root of an existing
/// tree, that whole tree is re-rooted in the same transaction:
///
/// ```sql
/// UPDATE edges SET root = $new_root, depth = depth + $new_depth
///  WHERE root = $subject AND predicate = $predicate
/// ```
///
/// Materialised root makes that one indexed statement, and it means the
/// database owns the invariant "root and depth describe the chain as it is"
/// rather than trusting the queues to deliver parents first. `MAX_DEPTH` is
/// enforced over the re-rooted tree too, so a chain can no more grow past the
/// ceiling from the top than from the bottom.
///
/// # Locking
///
/// Two keys. The subject, EXCLUSIVE: it is the root of any tree that is about
/// to be moved, and the key a child of the subject holds (shared) while
/// placing itself at the subject. And the root of the chain the object hangs
/// from, SHARED: the tree the new edge joins. Shared is what lets a post with
/// five thousand replies a second take them all at once — leaves under one
/// root never wait on each other, only on the rare write that moves or trims
/// the tree, which takes the exclusive form and waits for the leaves in
/// flight. The subject lock is taken first; the chain lock is taken by
/// `lock_chain_root`, which verifies the root after locking it. Two inserts
/// can only wait on each other's keys when each subject sits at the bottom
/// of the other's chain — a cycle in the making — and Postgres resolves that
/// by aborting one of them, which the caller reports as contention for the
/// queue to retry; the retry then meets the cycle refusal below.
///
/// # Cycles
///
/// If O's chain bottoms out at S, then (S, P, O) would close it into a loop,
/// and re-rooting S's tree under itself would leave root and depth meaning
/// nothing. That insert is refused. A subject that already hangs from another
/// chain may still gain a second outgoing edge that points back into its own
/// ancestry — the placement tree follows its first parent and stays a tree — so
/// only the corrupting shape is refused, not every graph-theoretic cycle.
pub async fn insert_edge(
    tx: &mut Transaction<'_, Postgres>,
    subject: Uuid,
    predicate: &str,
    object: Uuid,
) -> Result<EdgeRow, EdgeInsertError> {
    lock_node(tx, subject).await?;
    let parent = lock_chain_root(tx, object, predicate).await?;

    let (root, depth) = match parent {
        Some((parent_root, parent_depth)) => (parent_root, parent_depth + 1),
        None => (object, 1),
    };

    if root == subject {
        return Err(EdgeInsertError::Cycle);
    }
    if depth > MAX_DEPTH {
        return Err(EdgeInsertError::TooDeep(depth));
    }

    // The deepest edge currently rooted at the subject, which after re-rooting
    // will sit that much further down. Zero when nothing is rooted there — the
    // ordinary parent-before-child case, and every case where the subject
    // already hangs from a chain.
    let deepest_below: i32 = sqlx::query_scalar(
        "SELECT COALESCE(MAX(depth), 0) FROM edges WHERE root = $1 AND predicate = $2",
    )
    .bind(subject)
    .bind(predicate)
    .fetch_one(&mut **tx)
    .await?;
    if deepest_below + depth > MAX_DEPTH {
        return Err(EdgeInsertError::TooDeep(deepest_below + depth));
    }

    let row: Option<EdgeRow> = sqlx::query_as(
        "INSERT INTO edges (subject, predicate, object, root, depth)
         VALUES ($1, $2, $3, $4, $5)
         ON CONFLICT (subject, predicate, object) DO NOTHING
         RETURNING subject, predicate, object, root, depth, created_at",
    )
    .bind(subject)
    .bind(predicate)
    .bind(object)
    .bind(root)
    .bind(depth)
    .fetch_optional(&mut **tx)
    .await?;
    let row = row.ok_or(EdgeInsertError::Duplicate)?;

    if deepest_below > 0 {
        sqlx::query(
            "UPDATE edges
                SET root = $1, depth = depth + $2
              WHERE root = $3 AND predicate = $4",
        )
        .bind(root)
        .bind(depth)
        .bind(subject)
        .bind(predicate)
        .execute(&mut **tx)
        .await?;
    }

    Ok(row)
}

pub enum EdgeDeleteError {
    /// Other edges were placed beneath this one; removing it would leave their
    /// materialised root and depth stale.
    HasDescendants(i64),
    NotFound,
    /// The chain was being re-rooted under this delete faster than it could be
    /// locked. Transient; the caller should retry.
    Contended,
    Db(sqlx::Error),
}

impl From<sqlx::Error> for EdgeDeleteError {
    fn from(e: sqlx::Error) -> Self {
        EdgeDeleteError::Db(e)
    }
}

/// Removes an edge, refusing to orphan anything placed beneath it.
///
/// An edge X is placed from this one when X.object == subject under the same
/// predicate, so that is exactly what is counted. Two exclusive locks make the
/// count binding: the subject, which an insert of such a child holds while it
/// is rooted AT the subject, and the root of this edge's chain, which an
/// insert of such a child holds (shared) while it is placed UNDER the subject
/// — and which is re-read after locking, because the chain can be re-rooted
/// between the read and the lock (see `lock_chain_root`). A delete therefore
/// waits for every leaf in flight under its tree and briefly excludes new
/// ones; deletes are rare and leaves are not, which is the right way round.
pub async fn delete_edge(
    tx: &mut Transaction<'_, Postgres>,
    subject: Uuid,
    predicate: &str,
    object: Uuid,
) -> Result<EdgeRow, EdgeDeleteError> {
    lock_node(tx, subject).await?;

    let mut locked_root: Option<Uuid> = None;
    for _ in 0..CHAIN_LOCK_TURNS {
        let root: Option<Uuid> = sqlx::query_scalar(
            "SELECT root FROM edges WHERE subject = $1 AND predicate = $2 AND object = $3",
        )
        .bind(subject)
        .bind(predicate)
        .bind(object)
        .fetch_optional(&mut **tx)
        .await?;
        let Some(root) = root else {
            return Err(EdgeDeleteError::NotFound);
        };
        if locked_root == Some(root) {
            break;
        }
        lock_node(tx, root).await?;
        locked_root = Some(root);
    }
    if locked_root.is_none() {
        return Err(EdgeDeleteError::Contended);
    }

    let descendants: i64 =
        sqlx::query_scalar("SELECT COUNT(*) FROM edges WHERE object = $1 AND predicate = $2")
            .bind(subject)
            .bind(predicate)
            .fetch_one(&mut **tx)
            .await?;

    if descendants > 0 {
        return Err(EdgeDeleteError::HasDescendants(descendants));
    }

    let row: Option<EdgeRow> = sqlx::query_as(
        "DELETE FROM edges
          WHERE subject = $1 AND predicate = $2 AND object = $3
      RETURNING subject, predicate, object, root, depth, created_at",
    )
    .bind(subject)
    .bind(predicate)
    .bind(object)
    .fetch_optional(&mut **tx)
    .await?;

    row.ok_or(EdgeDeleteError::NotFound)
}

/// One edge by its key, or nothing. The read a writer uses to confirm that a
/// refused insert is nonetheless present: a primary-key probe, so the answer
/// never depends on how many other edges the subject carries.
pub async fn fetch_edge<'e, E>(
    exec: E,
    subject: Uuid,
    predicate: &str,
    object: Uuid,
) -> Result<Option<EdgeRow>, sqlx::Error>
where
    E: PgExecutor<'e>,
{
    sqlx::query_as(
        "SELECT subject, predicate, object, root, depth, created_at
           FROM edges
          WHERE subject = $1 AND predicate = $2 AND object = $3",
    )
    .bind(subject)
    .bind(predicate)
    .bind(object)
    .fetch_optional(exec)
    .await
}

pub async fn fetch_node<'e, E>(exec: E, node_id: Uuid) -> Result<Option<NodeRow>, sqlx::Error>
where
    E: PgExecutor<'e>,
{
    sqlx::query_as(
        "SELECT node_id, kind, owner, status, created_at, updated_at
           FROM nodes WHERE node_id = $1",
    )
    .bind(node_id)
    .fetch_optional(exec)
    .await
}

/// Active names bound to a node, primary first.
pub async fn fetch_active_names<'e, E>(exec: E, node_id: Uuid) -> Result<Vec<NameRow>, sqlx::Error>
where
    E: PgExecutor<'e>,
{
    sqlx::query_as(
        "SELECT name::text AS name, node_id, namespace, is_primary, status, created_at, revoked_at
           FROM names
          WHERE node_id = $1 AND status = 'active'
          ORDER BY is_primary DESC, created_at ASC",
    )
    .bind(node_id)
    .fetch_all(exec)
    .await
}

/// One name row in any status. Revoked rows are returned too — they are the record that
/// says a name was retired, and they must never be silently treated as absent.
pub async fn fetch_name<'e, E>(exec: E, name: &str) -> Result<Option<NameRow>, sqlx::Error>
where
    E: PgExecutor<'e>,
{
    // A name may now have history: many revoked rows and at most one active row
    // (migration 0005). The live binding is the answer to "who holds this name";
    // the most recent revoked row is the answer only when nobody holds it.
    sqlx::query_as(
        "SELECT name::text AS name, node_id, namespace, is_primary, status, created_at, revoked_at
           FROM names
          WHERE name = $1::citext
          ORDER BY (status = 'active') DESC, created_at DESC
          LIMIT 1",
    )
    .bind(name)
    .fetch_optional(exec)
    .await
}

/// The brain that owns a kind of node, from the authority map. `None` means the
/// kind is unclaimed and the creating brain keeps it.
///
/// This is read per write rather than cached: the map changes when ownership of
/// a domain moves between brains, and a cached answer would let a brain keep
/// writing to something it no longer owns until the next restart.
pub async fn owner_for_kind(pool: &sqlx::PgPool, kind: &str) -> sqlx::Result<Option<String>> {
    let row: Option<(String,)> =
        sqlx::query_as("SELECT owner FROM node_kind_owners WHERE kind = $1")
            .bind(kind)
            .fetch_optional(pool)
            .await?;
    Ok(row.map(|r| r.0))
}

/// The brain that governs a namespace — who may bind and retire names in it.
/// `None` means the namespace is unclaimed, and naming falls back to node
/// ownership. A namespace should not need a migration before it can be used,
/// only before it becomes contested.
pub async fn authority_for_namespace(
    pool: &sqlx::PgPool,
    namespace: &str,
) -> sqlx::Result<Option<String>> {
    let row: Option<(String,)> =
        sqlx::query_as("SELECT brain FROM namespace_authorities WHERE namespace = $1")
            .bind(namespace)
            .fetch_optional(pool)
            .await?;
    Ok(row.map(|r| r.0))
}

/// Whether a namespace's names are ALLOCATED (granted to someone, and therefore
/// only the governing brain may bind them) or DERIVED (computed from the entity,
/// so any brain that encounters it may bind it). See migration 0004.
///
/// Returns the governing brain only when the namespace is allocated; a derived
/// or unclaimed namespace yields None, meaning "no allocation right applies".
pub async fn allocation_authority(
    pool: &sqlx::PgPool,
    namespace: &str,
) -> sqlx::Result<Option<String>> {
    let row: Option<(String,)> = sqlx::query_as(
        "SELECT brain FROM namespace_authorities WHERE namespace = $1 AND allocated",
    )
    .bind(namespace)
    .fetch_optional(pool)
    .await?;
    Ok(row.map(|r| r.0))
}

// Two readers used to live here: `namespace_is_reusable` and `name_was_revoked`.
// Both are gone. Migration 0008 made reuse a question with three answers — never,
// immediately, or after a hold measured against the database's own clock — and
// answering it from two separate reads would let a hold expire between them. The
// whole rule is now one statement inside `may_bind_name` below, which is the only
// place that ever needed it.

// ── The one gate every path that binds a name passes ──────────────────────────

/// Lock class for name-scoped advisory locks. Postgres keeps the two-integer
/// lock space separate from the single-bigint space `lock_key` uses for nodes,
/// so a name lock can never collide with an edge lock.
const NAME_LOCK_CLASS: i32 = 0x4D48; // 'MH'

/// Case-folded advisory-lock key for a name. Names are CITEXT, so two spellings
/// are one name and must take one lock.
fn name_lock_key(name: &str) -> i32 {
    let mut hash: u32 = 0x811c_9dc5;
    for b in name.to_lowercase().bytes() {
        hash ^= b as u32;
        hash = hash.wrapping_mul(0x0100_0193);
    }
    hash as i32
}

/// Holds this name for the rest of the transaction. Every write that decides a
/// name's binding — create, bind, revoke — takes it, so a check and the insert
/// that depends on it cannot straddle a concurrent revoke's commit.
pub async fn lock_name(tx: &mut Transaction<'_, Postgres>, name: &str) -> Result<(), sqlx::Error> {
    sqlx::query("SELECT pg_advisory_xact_lock($1, $2)")
        .bind(NAME_LOCK_CLASS)
        .bind(name_lock_key(name))
        .execute(&mut **tx)
        .await?;
    Ok(())
}

/// The answer to "may this name be bound now", with the reason when it may
/// not. The two refusals are different facts and are reported to callers under
/// different conflict codes: a caller that receives `NeverReissued` must stop,
/// while one that receives `InQuarantine` is being told when to come back.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BindVerdict {
    Bindable,
    /// Revoked, and the namespace never reissues a revoked name.
    NeverReissued,
    /// Revoked, and the namespace's hold on it has not yet run out.
    InQuarantine,
}

impl BindVerdict {
    pub fn is_bindable(self) -> bool {
        self == BindVerdict::Bindable
    }
}

/// Locks the name and answers whether it may be bound now.
///
/// A revoked name comes back only if its namespace says it may, and only once
/// any hold that namespace imposes has run out. Three rules, one question:
///
///   * `reusable = FALSE` — never. A rotated contact address, a PIAL. Retiring
///     one is a security act and re-minting it to a stranger would deliver their
///     predecessor's mail.
///   * `reusable = TRUE`, no quarantine — immediately. A handle is meant to
///     move, and moving it is the registrar's whole purpose.
///   * `reusable = TRUE`, quarantined — after the hold. A Number, held for
///     twelve months so every card and slide it was printed on has left
///     circulation before anybody else can be reached through it.
///
/// # Self-reclaim
///
/// `reclaimer` is the node the new binding would resolve to, when the namespace
/// has such a thing. The quarantine exists to stop a STRANGER receiving contact
/// meant for the previous holder — so when the previous holder IS the caller,
/// the risk it guards against does not exist and the hold is waived entirely.
/// Stale contact arriving at a reclaimed Number reaches the person it was always
/// meant for. This is also the honest answer for an annual conference that wants
/// its Number back on next year's badge: quarantine must not punish the recurring
/// use it was never aimed at.
///
/// The waiver requires EVERY revoked binding of the name to have resolved to that
/// same node. One earlier stranger in the history and the full hold applies, so
/// the waiver cannot be assembled out of somebody else's history.
///
/// `None` means the caller has no such node — every namespace whose names do not
/// resolve to anything — and nothing is waived.
///
/// # One clock, one statement
///
/// The comparison is made in SQL against the database's own `NOW()` — the same
/// clock that stamped `revoked_at` — so there is no skew to reason about, exactly
/// as with a Number's lease. It is ONE statement because the answer must be
/// atomic with respect to that clock: reading the rule and the revocation
/// separately would let a hold expire between the two reads.
///
/// Since migration 0005 the uniqueness index is partial on `status = 'active'`,
/// so it no longer refuses a revoked name on its own — this is the only thing
/// that does, and every entry point that inserts into `names` goes through here
/// rather than repeating the rule.
///
/// # Nothing is inherited
///
/// A reissued name is a NEW node with a new `node_id`. Nothing keyed to the
/// previous holder's node — a Number's policy row, its label, its lease, its
/// admission ledger, the contact requests opened through it — is reachable from
/// the new one, so a reissue transfers no state and no pending obligation. That
/// holds for a self-reclaim too: an owner gets their Number back, not the spent
/// budget and stale ledger it had when they retired it.
pub async fn may_bind_name(
    tx: &mut Transaction<'_, Postgres>,
    name: &str,
    namespace: &str,
    reclaimer: Option<Uuid>,
) -> Result<BindVerdict, sqlx::Error> {
    lock_name(tx, name).await?;
    let (verdict,): (String,) = sqlx::query_as(
        "SELECT CASE
             -- Never revoked: bindable, whatever the namespace says. This is the
             -- ordinary first allocation of a fresh name.
             WHEN NOT EXISTS (
                 SELECT 1 FROM names
                  WHERE name = $1::citext AND status = 'revoked'
             ) THEN 'bindable'
             -- Otherwise the namespace decides, and an unknown namespace decides
             -- against reuse rather than for it.
             ELSE COALESCE((
                 SELECT CASE
                     WHEN NOT a.reusable THEN 'never'
                     WHEN a.quarantine_seconds IS NULL THEN 'bindable'
                     -- Self-reclaim: every revoked binding of this name
                     -- resolved to the very node asking for it back.
                     WHEN $3::UUID IS NOT NULL AND NOT EXISTS (
                             SELECT 1 FROM names n
                              WHERE n.name = $1::citext
                                AND n.status = 'revoked'
                                AND NOT EXISTS (
                                    SELECT 1 FROM edges e
                                     WHERE e.subject   = n.node_id
                                       AND e.predicate = 'resolves_to'
                                       AND e.object    = $3::UUID))
                          THEN 'bindable'
                     -- Otherwise the hold, against the database's own clock.
                     WHEN NOT EXISTS (
                             SELECT 1 FROM names n
                              WHERE n.name = $1::citext
                                AND n.status = 'revoked'
                                AND COALESCE(n.revoked_at, n.created_at)
                                    + (a.quarantine_seconds * INTERVAL '1 second') > NOW())
                          THEN 'bindable'
                     ELSE 'quarantine'
                 END
                   FROM namespace_authorities a
                  WHERE a.namespace = $2
             ), 'never')
         END",
    )
    .bind(name)
    .bind(namespace)
    .bind(reclaimer)
    .fetch_one(&mut **tx)
    .await?;
    Ok(match verdict.as_str() {
        "bindable" => BindVerdict::Bindable,
        "quarantine" => BindVerdict::InQuarantine,
        _ => BindVerdict::NeverReissued,
    })
}

/// Whether this name has a revoked binding that resolved to `holder`.
///
/// The gate on asking for a SPECIFIC name back. Without it, a reclaim endpoint
/// would answer "is this name free?" for any name a stranger cared to type —
/// an enumeration oracle over the whole address space, handed out one question
/// at a time. With it, the question the endpoint actually answers is "did YOU
/// hold this?", which is about the caller and discloses nothing about anybody
/// else's name.
pub async fn name_was_held_by<'e, E>(exec: E, name: &str, holder: Uuid) -> sqlx::Result<bool>
where
    E: PgExecutor<'e>,
{
    let (held,): (bool,) = sqlx::query_as(
        "SELECT EXISTS (
             SELECT 1
               FROM names n
               JOIN edges e ON e.subject = n.node_id AND e.predicate = 'resolves_to'
              WHERE n.name = $1::citext
                AND n.status = 'revoked'
                AND e.object = $2)",
    )
    .bind(name)
    .bind(holder)
    .fetch_one(exec)
    .await?;
    Ok(held)
}

// ── The association plane ─────────────────────────────────────────────────────
//
// Associations are the other half of the graph, and they are deliberately not
// edges. `insert_edge` above takes an advisory lock and reads a parent, because
// an edge has to be PLACED — its root and depth are derived from what is already
// there. An association has no placement: it is a fact about a pair, true or
// absent. So there is no lock here, no parent read, and no transaction — an
// insert is one statement whose only failure mode is the primary key.

/// The brain permitted to assert an association of this type. `None` means the
/// association type does not exist; unlike a namespace, an unclaimed association
/// is not "anyone may write it" — an association is a claim one brain makes
/// about two nodes it does not own, and "whoever gets there first" is never the
/// right rule for that.
///
/// Read per write rather than cached, for the same reason `owner_for_kind` is:
/// when authority over a relationship moves between brains, a cached answer lets
/// the old brain keep writing until the next restart.
pub async fn assoc_authority(pool: &sqlx::PgPool, assoc: &str) -> sqlx::Result<Option<String>> {
    let row: Option<(String,)> =
        sqlx::query_as("SELECT brain FROM assoc_authorities WHERE assoc = $1")
            .bind(assoc)
            .fetch_optional(pool)
            .await?;
    Ok(row.map(|r| r.0))
}

/// Records an association. `Ok(None)` means it was already recorded, which is
/// the end state the caller wanted — an at-least-once writer replaying is the
/// normal case, not an error.
pub async fn insert_assoc<'e, E>(
    exec: E,
    subject: Uuid,
    assoc: &str,
    object: Uuid,
) -> Result<Option<AssocRow>, sqlx::Error>
where
    E: PgExecutor<'e>,
{
    sqlx::query_as(
        "INSERT INTO assocs (subject, assoc, object)
         VALUES ($1, $2, $3)
         ON CONFLICT (subject, assoc, object) DO NOTHING
         RETURNING subject, assoc, object, created_at",
    )
    .bind(subject)
    .bind(assoc)
    .bind(object)
    .fetch_optional(exec)
    .await
}

/// Removes an association. `Ok(None)` means it was not there.
///
/// Nothing hangs off an association, so unlike `delete_edge` this cannot orphan
/// anything and never has to refuse. That absence of a refusal path is the whole
/// argument for the second table: an unrooted fact is cheap to retract, and a
/// retraction that can fail is a retraction that leaves stale authorisation
/// behind.
pub async fn delete_assoc<'e, E>(
    exec: E,
    subject: Uuid,
    assoc: &str,
    object: Uuid,
) -> Result<Option<AssocRow>, sqlx::Error>
where
    E: PgExecutor<'e>,
{
    sqlx::query_as(
        "DELETE FROM assocs
          WHERE subject = $1 AND assoc = $2 AND object = $3
      RETURNING subject, assoc, object, created_at",
    )
    .bind(subject)
    .bind(assoc)
    .bind(object)
    .fetch_optional(exec)
    .await
}

/// Both directions between two nodes, in one round trip.
///
/// Written as two primary-key probes joined by UNION ALL rather than one
/// `WHERE (a,b) OR (b,a)`: the OR form can decline to use the index and degrade
/// into a scan of one node's whole association list, and the caller of this is a
/// security decision with a fixed time budget. Each half is an exact PK lookup.
///
/// Returns (forward, reverse) as the moment each was recorded, so a caller can
/// tell "not associated" from "associated a second ago" without a second call.
pub async fn assoc_between<'e, E>(
    exec: E,
    assoc: &str,
    a: Uuid,
    b: Uuid,
) -> Result<(Option<DateTime<Utc>>, Option<DateTime<Utc>>), sqlx::Error>
where
    E: PgExecutor<'e>,
{
    let rows: Vec<(bool, DateTime<Utc>)> = sqlx::query_as(
        "SELECT TRUE  AS forward, created_at FROM assocs
          WHERE subject = $2 AND assoc = $1 AND object = $3
         UNION ALL
         SELECT FALSE AS forward, created_at FROM assocs
          WHERE subject = $3 AND assoc = $1 AND object = $2",
    )
    .bind(assoc)
    .bind(a)
    .bind(b)
    .fetch_all(exec)
    .await?;

    let mut forward = None;
    let mut reverse = None;
    for (is_forward, at) in rows {
        if is_forward {
            forward = Some(at);
        } else {
            reverse = Some(at);
        }
    }
    Ok((forward, reverse))
}

/// One entry of an association listing: the other endpoint, named.
///
/// The name is the node's primary active name, because the callers of this
/// plane may hold names and nothing else. A node with no primary name is still
/// listed by id — the association is a fact whether or not the node is
/// currently nameable — and the caller decides what to do with it.
#[derive(Debug, serde::Serialize, sqlx::FromRow)]
pub struct AssocListRow {
    pub node_id: Uuid,
    pub name: Option<String>,
    pub created_at: DateTime<Utc>,
}

/// Which side of an association a listing walks.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AssocSide {
    /// Everything `node` associates with: `subject = node`. TAO's assoc_range.
    Out,
    /// Everything that associates with `node`: `object = node`. The inverse.
    In,
}

/// The N most recent associations on one side of a node, newest first, keyed
/// for the next page by (created_at, other node).
///
/// This is the read `idx_assocs_subject_time` and `idx_assocs_object_time`
/// were built for: one index range per call, walked from the newest row down,
/// so page ten costs what page one costs. `after` is the last row of the
/// previous page; rows strictly older than it (or equal in time and later by
/// id, so a timestamp shared by two rows never hides one) come back.
pub async fn assoc_range<'e, E>(
    exec: E,
    assoc: &str,
    node: Uuid,
    side: AssocSide,
    after: Option<(DateTime<Utc>, Uuid)>,
    limit: i64,
) -> Result<Vec<AssocListRow>, sqlx::Error>
where
    E: PgExecutor<'e>,
{
    let sql = match side {
        AssocSide::Out => {
            "SELECT a.object AS node_id, nm.name::text AS name, a.created_at
               FROM assocs a
               LEFT JOIN names nm ON nm.node_id = a.object
                                 AND nm.status = 'active' AND nm.is_primary
              WHERE a.subject = $1 AND a.assoc = $2
                AND ($3::timestamptz IS NULL
                     OR (a.created_at, a.object) < ($3::timestamptz, $4::uuid))
              ORDER BY a.created_at DESC, a.object DESC
              LIMIT $5"
        }
        AssocSide::In => {
            "SELECT a.subject AS node_id, nm.name::text AS name, a.created_at
               FROM assocs a
               LEFT JOIN names nm ON nm.node_id = a.subject
                                 AND nm.status = 'active' AND nm.is_primary
              WHERE a.object = $1 AND a.assoc = $2
                AND ($3::timestamptz IS NULL
                     OR (a.created_at, a.subject) < ($3::timestamptz, $4::uuid))
              ORDER BY a.created_at DESC, a.subject DESC
              LIMIT $5"
        }
    };
    sqlx::query_as(sql)
        .bind(node)
        .bind(assoc)
        .bind(after.map(|(t, _)| t))
        .bind(after.map(|(_, id)| id))
        .bind(limit)
        .fetch_all(exec)
        .await
}

/// How many associations a node has on each side: (out, in). TAO's
/// assoc_count, answered from the same two indexes without touching a row that
/// belongs to any other node — which is what makes a count that is never
/// maintained by application code, and therefore never drifts, affordable.
pub async fn assoc_count<'e, E>(exec: E, assoc: &str, node: Uuid) -> Result<(i64, i64), sqlx::Error>
where
    E: PgExecutor<'e>,
{
    sqlx::query_as(
        "SELECT (SELECT COUNT(*) FROM assocs WHERE subject = $1 AND assoc = $2),
                (SELECT COUNT(*) FROM assocs WHERE object  = $1 AND assoc = $2)",
    )
    .bind(node)
    .bind(assoc)
    .fetch_one(exec)
    .await
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Names are CITEXT: `Handle:Bob` and `handle:bob` are one name, so they must
    /// take one lock. If they did not, two spellings of the same name could bind
    /// concurrently and the never-reissued check would be reading stale history.
    #[test]
    fn name_lock_folds_case() {
        assert_eq!(name_lock_key("handle:bob"), name_lock_key("Handle:BOB"));
        assert_ne!(name_lock_key("handle:bob"), name_lock_key("handle:bo"));
    }
}
