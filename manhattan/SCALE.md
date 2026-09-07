# Manhattan at 800M MAU

What the naming and graph plane must carry if F33D3R reaches the scale of the
largest social platforms, where the current design holds, and what has to
change. Numbers are order-of-magnitude, worked from public figures for
platforms of that size, and are written down so the next person does not have
to redo them.

## The load

| Quantity | Estimate | Where it lands |
|---|---|---|
| Identities | 1 × 10^9 lifetime | `nodes`, `names` (pial + handle each) |
| Works | 5 × 10^8 / day, 2 × 10^11 lifetime | `nodes`, `names` (uuid + cid each), `edges` (authored_by) |
| Replies and quotes | 1 × 10^8 / day | `edges` (replies_to, quotes) |
| Follows | 1 × 10^9 / day peak churn, 10^11 lifetime | `assocs` |
| Naming-plane writes | 30k / s sustained, 100k / s peak | `/v1/apply` |
| Resolutions | 1 to 5 million / s | `/v1/resolve`, `/v1/resolve/batch` |

The read side is the estate's whole page-render traffic; fourteen brains
resolve names for every page. The write side is bounded by works and follows.

## What already holds

**Placement under a viral post.** Every reply to a post takes a SHARED
advisory lock on the post's chain root; only the two rare writers that move or
trim a tree take the exclusive one. Leaves under one root do not wait on each
other. `db_tests::a_hot_root_takes_concurrent_replies_and_survives_being_rerooted_under_load`
proves the shape; the ceiling is Postgres's write throughput, not a lock.

**Resolution.** `/v1/resolve` and `/v1/resolve/batch` are answered from an
in-process cache invalidated by Postgres NOTIFY at commit (migration 0009,
`cache.rs`). A replica answers hits in microseconds and takes no connection.
At 5M/s across replicas the database sees only misses and fills — the
working set of hot names, once per replica per minute. Sized by
`RESOLVE_CACHE_ENTRIES` (default 500k, about 100 MB).

**Delivery.** A drain sends a whole batch of outbox rows to `/v1/apply` as the
triggers wrote them and gets one verdict per row. One request per 200 rows,
looped while batches are full: a single drain lands several thousand rows a
second against one Manhattan replica, up from about twenty. Idempotence is
decided by the plane, so every brain's drain is the same crate
(`libs/manhattan-outbox`, `feed-engine/internal/manhattan/drain.go`).

**Correctness under disorder.** Edges arrive from a dozen drains with no
ordering between them; a child that lands before its parent is re-rooted when
the parent lands, and the database owns that invariant (`insert_edge`).

## What does not hold, in the order it will fail

### 1. One Postgres primary for writes — fails around 30k writes/s

Every `apply` op is one transaction with two to four statements on a single
primary. A well-tuned primary on fast storage does perhaps 20k such
transactions a second. The follow graph alone can exceed that at peak.

**The change:** shard by node. Every write in this plane is keyed by a node
id (edges by subject, assocs by subject, names by node), and every read is
by name or by node. Names are the routing problem: a name must resolve to a
shard before the shard can be asked. So:

- A **name directory** — `names` alone, replicated, keyed by name — answers
  "which shard holds this node". It is the thing the cache already fronts.
- **Node shards** hold `nodes`, `edges`, `assocs` for a range of node ids.
  Edge placement never crosses a shard because a chain's root and its leaves
  are placed by the *object's* shard — a reply lives with the post it replies
  to. Assocs are written to both endpoints' shards (TAO does the same), so
  `out` and `in` listings are each one shard.

This is a schema change with a migration plan, not a code change. Nothing in
the HTTP contract moves; `/v1/apply` and `/v1/resolve` stay, and the routing
is inside Manhattan. Build it when the primary passes 50% of its write
budget, which the `manhattan_apply_ops_total` rate will show.

### 2. `edges` and `names` as single tables — fails around 10^10 rows

Index maintenance on a 200-billion-row table is the problem, not query
speed; every insert touches four indexes. Postgres partitioning by hash of
the subject (edges) or the node id (names) keeps each partition's indexes in
memory and lets old partitions be vacuumed independently. Do this before
sharding — it is the same partition key, so the partitions become the shards
later.

### 3. Read replicas for the misses

Cache misses still go to the primary. Point misses at streaming replicas.
The cache's invalidation already tolerates replica lag: a fill from a lagging
replica is refused if the name was announced after the read began, exactly as
a fill from a slow primary read is. `resolve_one` needs a second pool; the
listener stays on the primary.

### 4. Outbox lanes in the brains — fails around 5k writes/s per brain

One drain per brain delivers in id order. At feed-engine's share of 30k
writes/s, one drain's serial loop is short by an order of magnitude even
batched. Rows about different entities do not depend on each other; rows
about one entity do. So: a `lane` column on `manhattan_outbox`, set by the
trigger to a hash of the row's principal entity name (the node for a `node`
or `name` row, the subject for an `edge` or `assoc` row), and N drains each
holding the advisory lock for one lane. Order is per lane; an edge whose
object is in another lane may briefly answer `retry` until that lane lands
the node, which is the ordering hazard the drain already handles.

This needs a migration in every brain's database and a `lane` parameter on
the shared drain. Do it when a brain's backlog grows across a whole day.

### 5. Manhattan replicas

Manhattan is stateless above Postgres. Run as many replicas as the resolve
rate needs behind the existing internal load balancer; each keeps its own
cache and its own listener, and a replica that cannot hear announcements
bypasses its cache until it can. Nothing is shared between replicas but the
database.

## What to watch

- `manhattan_apply_ops_total` by outcome: `retry` rising means a dependency
  is landing late somewhere; `error` rising means the plane is faulting.
- `manhattan_resolve_cache_total` by outcome: a hit rate under 95% at scale
  means the cache is undersized or a listener keeps reconnecting.
- `manhattan_resolve_cache_hearing`: zero on any replica is an incident —
  that replica is serving every resolve from the database.
- Each brain's `/health` backlog: a number that only grows is the drain not
  landing, and the reason is in `manhattan_outbox.last_error`.
