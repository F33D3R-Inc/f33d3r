//! The SQL in `db.rs`, proved against Postgres. See `testdb.rs` for how to
//! point these at a server; without one they report themselves skipped.

use std::collections::HashSet;

use chrono::{Duration, Utc};
use sqlx::PgPool;
use uuid::Uuid;

use crate::db::{
    self, AssocSide, BindVerdict, EdgeDeleteError, EdgeInsertError, MAX_DEPTH,
};
use crate::testdb;

const P: &str = "replies_to";

async fn insert(pool: &PgPool, s: Uuid, o: Uuid) -> Result<(Uuid, i32), String> {
    let mut tx = pool.begin().await.expect("begin");
    let out = db::insert_edge(&mut tx, s, P, o).await;
    match out {
        Ok(row) => {
            tx.commit().await.expect("commit");
            Ok((row.root, row.depth))
        }
        Err(EdgeInsertError::Duplicate) => Err("duplicate".into()),
        Err(EdgeInsertError::TooDeep(d)) => Err(format!("too_deep:{d}")),
        Err(EdgeInsertError::Cycle) => Err("cycle".into()),
        Err(EdgeInsertError::Contended) => Err("contended".into()),
        Err(EdgeInsertError::Db(e)) if db::is_deadlock(&e) => Err("deadlock".into()),
        Err(EdgeInsertError::Db(e)) => panic!("insert_edge: {e}"),
    }
}

async fn placement(pool: &PgPool, s: Uuid, o: Uuid) -> (Uuid, i32) {
    let row = db::fetch_edge(pool, s, P, o)
        .await
        .expect("fetch_edge")
        .unwrap_or_else(|| panic!("edge {s} -> {o} is missing"));
    (row.root, row.depth)
}

/// Every edge's (root, depth) is exactly what its parent implies. For a graph
/// where no subject carries more than one outgoing edge this is the whole
/// invariant, and it is checked in SQL so the assertion cannot drift from the
/// placement rule it mirrors.
async fn assert_consistent(pool: &PgPool) {
    let wrong: i64 = sqlx::query_scalar(
        "SELECT COUNT(*)
           FROM edges e
           LEFT JOIN LATERAL (
                SELECT root, depth FROM edges p
                 WHERE p.subject = e.object AND p.predicate = e.predicate
                 ORDER BY depth ASC, created_at ASC LIMIT 1) p ON TRUE
          WHERE NOT (
                (p.root IS NULL AND e.root = e.object AND e.depth = 1)
             OR (p.root IS NOT NULL AND e.root = p.root AND e.depth = p.depth + 1))",
    )
    .fetch_one(pool)
    .await
    .expect("consistency query");
    assert_eq!(wrong, 0, "{wrong} edge(s) carry a root or depth their parent does not imply");
}

// ── Placement ─────────────────────────────────────────────────────────────────

#[tokio::test]
async fn parent_before_child_places_as_before() {
    let Some(db) = testdb::fresh().await else { return };
    let (a, b, c) = (db.node("work").await, db.node("work").await, db.node("work").await);

    assert_eq!(insert(&db.pool, b, a).await.unwrap(), (a, 1));
    assert_eq!(insert(&db.pool, c, b).await.unwrap(), (a, 2));
    assert_consistent(&db.pool).await;
    db.finish().await;
}

/// The case the queues cannot promise away: twelve drains, no ordering between
/// them, so a reply can be registered before the work it replies to.
#[tokio::test]
async fn child_before_parent_is_rerooted_when_the_parent_lands() {
    let Some(db) = testdb::fresh().await else { return };
    let (z, a, b, c) = (
        db.node("work").await,
        db.node("work").await,
        db.node("work").await,
        db.node("work").await,
    );

    // c replies to b; b is not yet known to reply to anything.
    assert_eq!(insert(&db.pool, c, b).await.unwrap(), (b, 1));
    // b's own reply arrives: b -> a. c's edge must now sit under a.
    assert_eq!(insert(&db.pool, b, a).await.unwrap(), (a, 1));
    assert_eq!(placement(&db.pool, c, b).await, (a, 2));
    // And the root itself turns out to be a reply: everything shifts again.
    assert_eq!(insert(&db.pool, a, z).await.unwrap(), (z, 1));
    assert_eq!(placement(&db.pool, b, a).await, (z, 2));
    assert_eq!(placement(&db.pool, c, b).await, (z, 3));
    assert_consistent(&db.pool).await;

    // Reads see the whole thread under one root, bounded by depth.
    let thread: Vec<(Uuid, i32)> = sqlx::query_as(
        "SELECT subject, depth FROM edges WHERE root = $1 AND depth <= 2 ORDER BY depth",
    )
    .bind(z)
    .fetch_all(&db.pool)
    .await
    .unwrap();
    assert_eq!(thread, vec![(a, 1), (b, 2)]);
    db.finish().await;
}

#[tokio::test]
async fn a_chain_delivered_in_reverse_ends_up_identical_to_one_delivered_in_order() {
    let Some(db) = testdb::fresh().await else { return };
    let mut nodes = Vec::new();
    for _ in 0..12 {
        nodes.push(db.node("work").await);
    }
    // nodes[i+1] replies to nodes[i]; deliver the deepest reply first.
    for i in (0..nodes.len() - 1).rev() {
        insert(&db.pool, nodes[i + 1], nodes[i]).await.unwrap();
    }
    for i in 0..nodes.len() - 1 {
        assert_eq!(
            placement(&db.pool, nodes[i + 1], nodes[i]).await,
            (nodes[0], i as i32 + 1),
            "edge {} was not re-rooted correctly",
            i + 1
        );
    }
    assert_consistent(&db.pool).await;
    db.finish().await;
}

/// A subject with two outgoing edges keeps the parent chosen when its children
/// were placed, and re-rooting never touches a child that already hangs from a
/// chain.
#[tokio::test]
async fn a_second_outgoing_edge_does_not_move_children_placed_under_the_first() {
    let Some(db) = testdb::fresh().await else { return };
    let (a1, a2, b, c) = (
        db.node("work").await,
        db.node("work").await,
        db.node("work").await,
        db.node("work").await,
    );
    assert_eq!(insert(&db.pool, b, a1).await.unwrap(), (a1, 1));
    assert_eq!(insert(&db.pool, c, b).await.unwrap(), (a1, 2));
    // b also quotes-replies a2: a second outgoing edge, a second tree.
    assert_eq!(insert(&db.pool, b, a2).await.unwrap(), (a2, 1));
    assert_eq!(placement(&db.pool, c, b).await, (a1, 2), "c still hangs from a1");
    db.finish().await;
}

// ── Ceiling and cycles ────────────────────────────────────────────────────────

#[tokio::test]
async fn the_depth_ceiling_holds_over_a_rerooted_tree() {
    let Some(db) = testdb::fresh().await else { return };
    // A chain that already reaches MAX_DEPTH, built parent-first.
    let mut nodes = vec![db.node("work").await];
    for i in 0..MAX_DEPTH {
        let next = db.node("work").await;
        assert_eq!(insert(&db.pool, next, nodes[i as usize]).await.unwrap().1, i + 1);
        nodes.push(next);
    }
    let bottom = nodes[0];
    let deeper = db.node("work").await;
    // One more at the bottom would sit at MAX_DEPTH + 1.
    assert_eq!(
        insert(&db.pool, deeper, nodes[MAX_DEPTH as usize]).await,
        Err(format!("too_deep:{}", MAX_DEPTH + 1))
    );
    // Hanging the root from something else pushes the whole tree one deeper,
    // and that is refused for exactly the same reason.
    let above = db.node("work").await;
    assert_eq!(
        insert(&db.pool, bottom, above).await,
        Err(format!("too_deep:{}", MAX_DEPTH + 1))
    );
    // Nothing moved.
    assert_eq!(placement(&db.pool, nodes[MAX_DEPTH as usize], nodes[MAX_DEPTH as usize - 1]).await.1, MAX_DEPTH);
    assert_consistent(&db.pool).await;
    db.finish().await;
}

#[tokio::test]
async fn closing_a_chain_into_a_loop_is_refused() {
    let Some(db) = testdb::fresh().await else { return };
    let (a, b, c) = (db.node("work").await, db.node("work").await, db.node("work").await);
    insert(&db.pool, b, a).await.unwrap();
    assert_eq!(insert(&db.pool, a, b).await, Err("cycle".into()));
    insert(&db.pool, c, b).await.unwrap();
    assert_eq!(insert(&db.pool, a, c).await, Err("cycle".into()));
    assert_consistent(&db.pool).await;
    db.finish().await;
}

#[tokio::test]
async fn the_same_triple_twice_is_a_duplicate() {
    let Some(db) = testdb::fresh().await else { return };
    let (a, b) = (db.node("work").await, db.node("work").await);
    insert(&db.pool, b, a).await.unwrap();
    assert_eq!(insert(&db.pool, b, a).await, Err("duplicate".into()));
    db.finish().await;
}

// ── Deletion ──────────────────────────────────────────────────────────────────

#[tokio::test]
async fn deleting_refuses_to_orphan_and_frees_the_leaf() {
    let Some(db) = testdb::fresh().await else { return };
    let (a, b, c) = (db.node("work").await, db.node("work").await, db.node("work").await);
    insert(&db.pool, b, a).await.unwrap();
    insert(&db.pool, c, b).await.unwrap();

    let mut tx = db.pool.begin().await.unwrap();
    match db::delete_edge(&mut tx, b, P, a).await {
        Err(EdgeDeleteError::HasDescendants(1)) => {}
        Err(EdgeDeleteError::HasDescendants(n)) => panic!("counted {n} descendants, want 1"),
        Err(EdgeDeleteError::NotFound) => panic!("edge reported missing"),
        Err(EdgeDeleteError::Contended) => panic!("contended with nothing"),
        Err(EdgeDeleteError::Db(e)) => panic!("{e}"),
        Ok(_) => panic!("deleted an edge with a child beneath it"),
    }
    drop(tx);

    let mut tx = db.pool.begin().await.unwrap();
    let row = db::delete_edge(&mut tx, c, P, b).await.ok().expect("leaf delete");
    tx.commit().await.unwrap();
    assert_eq!((row.root, row.depth), (a, 2));

    let mut tx = db.pool.begin().await.unwrap();
    match db::delete_edge(&mut tx, c, P, b).await {
        Err(EdgeDeleteError::NotFound) => {}
        _ => panic!("second delete must report not found"),
    }
    drop(tx);
    db.finish().await;
}

// ── Concurrency ───────────────────────────────────────────────────────────────

/// Many writers, one chain, every edge in a random order at once. Whatever
/// interleaving Postgres picks, the tree that results is the one an in-order
/// delivery would have produced.
#[tokio::test]
async fn concurrent_out_of_order_inserts_converge_on_one_correct_tree() {
    let Some(db) = testdb::fresh().await else { return };
    const N: usize = 20;
    let mut nodes = Vec::new();
    for _ in 0..N {
        nodes.push(db.node("work").await);
    }
    let mut order: Vec<usize> = (0..N - 1).collect();
    {
        use rand::seq::SliceRandom;
        order.shuffle(&mut rand::thread_rng());
    }

    let mut tasks = Vec::new();
    for i in order {
        let pool = db.pool.clone();
        let (s, o) = (nodes[i + 1], nodes[i]);
        tasks.push(tokio::spawn(async move {
            // A drain retries transient refusals; so does this.
            loop {
                match insert(&pool, s, o).await {
                    Ok(_) => return,
                    Err(e) if e == "contended" || e == "deadlock" => continue,
                    Err(e) => panic!("edge {}: {e}", i + 1),
                }
            }
        }));
    }
    for t in tasks {
        t.await.expect("writer task");
    }

    for i in 0..N - 1 {
        assert_eq!(
            placement(&db.pool, nodes[i + 1], nodes[i]).await,
            (nodes[0], i as i32 + 1),
            "edge {} settled at the wrong place",
            i + 1
        );
    }
    assert_consistent(&db.pool).await;
    db.finish().await;
}

/// Two writers racing to attach the same tree under two different parents
/// (the subject gaining two outgoing edges at once) must leave the children
/// under exactly one of them, consistently.
#[tokio::test]
async fn concurrent_parents_for_one_subject_leave_children_under_exactly_one() {
    let Some(db) = testdb::fresh().await else { return };
    let (a1, a2, b, c, d) = (
        db.node("work").await,
        db.node("work").await,
        db.node("work").await,
        db.node("work").await,
        db.node("work").await,
    );
    insert(&db.pool, c, b).await.unwrap();
    insert(&db.pool, d, c).await.unwrap();

    let (p1, p2) = (db.pool.clone(), db.pool.clone());
    let t1 = tokio::spawn(async move { insert(&p1, b, a1).await.unwrap() });
    let t2 = tokio::spawn(async move { insert(&p2, b, a2).await.unwrap() });
    t1.await.unwrap();
    t2.await.unwrap();

    let (root_c, depth_c) = placement(&db.pool, c, b).await;
    let (root_d, depth_d) = placement(&db.pool, d, c).await;
    assert!(root_c == a1 || root_c == a2, "c hangs from neither parent");
    assert_eq!(root_d, root_c);
    assert_eq!((depth_c, depth_d), (2, 3));
    db.finish().await;
}

/// A viral post: many replies landing on one root at once, and, halfway
/// through, the root itself turning out to be a reply. Leaves must not wait on
/// each other, the re-root must carry every leaf that landed before it, and
/// every leaf that lands after it must place itself under the new root.
#[tokio::test]
async fn a_hot_root_takes_concurrent_replies_and_survives_being_rerooted_under_load() {
    let Some(db) = testdb::fresh().await else { return };
    const REPLIES: usize = 120;
    let root = db.node("work").await;
    let above = db.node("work").await;
    let mut replies = Vec::new();
    for _ in 0..REPLIES {
        replies.push(db.node("work").await);
    }

    let mut tasks = Vec::new();
    for (i, reply) in replies.iter().copied().enumerate() {
        let pool = db.pool.clone();
        tasks.push(tokio::spawn(async move {
            loop {
                match insert(&pool, reply, root).await {
                    Ok(_) => return,
                    Err(e) if e == "contended" || e == "deadlock" => continue,
                    Err(e) => panic!("reply {i}: {e}"),
                }
            }
        }));
        if i == REPLIES / 2 {
            let pool = db.pool.clone();
            tasks.push(tokio::spawn(async move {
                loop {
                    match insert(&pool, root, above).await {
                        Ok(_) => return,
                        Err(e) if e == "contended" || e == "deadlock" => continue,
                        Err(e) => panic!("re-root: {e}"),
                    }
                }
            }));
        }
    }
    for t in tasks {
        t.await.expect("writer task");
    }

    for reply in replies {
        assert_eq!(placement(&db.pool, reply, root).await, (above, 2));
    }
    assert_eq!(placement(&db.pool, root, above).await, (above, 1));
    assert_consistent(&db.pool).await;
    db.finish().await;
}

// ── The resolution cache ──────────────────────────────────────────────────────

/// A resolution served from memory stops being served the instant the
/// revocation commits, because Postgres says so — not because a timer ran out.
#[tokio::test]
async fn a_cached_resolution_is_dropped_the_moment_its_name_changes() {
    let Some(db) = testdb::fresh().await else { return };
    let cache = std::sync::Arc::new(crate::cache::ResolveCache::new(4096));
    tokio::spawn(crate::cache::listen(db.pool.clone(), cache.clone()));
    let state = crate::AppState {
        pool: db.pool.clone(),
        internal_api_key: "test".into(),
        cache: cache.clone(),
    };
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
    while !cache.hearing() {
        assert!(std::time::Instant::now() < deadline, "listener never connected");
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
    }

    let id = db.named("identity", "handle", "handle:cached").await;
    // The bind itself is announced, and the announcement may land after the
    // first read filled the cache — a correct eviction of a correct entry. So
    // read until the answer sticks; it must within a moment.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(3);
    loop {
        let first = crate::resolve_one(&state, "handle:cached").await.ok().flatten().expect("resolves");
        assert_eq!(first.node_id, id);
        if matches!(cache.get("HANDLE:CACHED"), crate::cache::Lookup::Hit(Some(_))) {
            break;
        }
        assert!(std::time::Instant::now() < deadline, "the answer never stayed cached");
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert_eq!(cache.len(), 1, "the answer was cached, case-folded");

    // Revoke. The trigger announces on commit; the listener evicts.
    db.revoke("handle:cached", 0).await;
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(3);
    loop {
        if let Ok(None) = crate::resolve_one(&state, "handle:cached").await {
            break;
        }
        assert!(std::time::Instant::now() < deadline, "revoked name still resolved after 3s");
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }

    // A miss is cached too, and a new binding announces its way in.
    assert!(matches!(cache.get("handle:cached"), crate::cache::Lookup::Hit(None)));
    let again = db.named("identity", "handle", "handle:cached").await;
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(3);
    loop {
        if let Ok(Some(r)) = crate::resolve_one(&state, "handle:cached").await {
            assert_eq!(r.node_id, again);
            break;
        }
        assert!(std::time::Instant::now() < deadline, "rebound name still missing after 3s");
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }

    // A node's status changing announces every name bound to it.
    sqlx::query("UPDATE nodes SET status = 'tombstoned' WHERE node_id = $1")
        .bind(again)
        .execute(&db.pool)
        .await
        .unwrap();
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(3);
    loop {
        if let Ok(Some(r)) = crate::resolve_one(&state, "handle:cached").await {
            if r.status == "tombstoned" {
                break;
            }
        }
        assert!(std::time::Instant::now() < deadline, "tombstoned node still read as active after 3s");
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert!(crate::resolve_active(&state, "handle:cached").await.unwrap().is_none());
    db.finish().await;
}

// ── /v1/apply ─────────────────────────────────────────────────────────────────

fn state_for(db: &testdb::TestDb) -> crate::AppState {
    crate::AppState {
        pool: db.pool.clone(),
        internal_api_key: "test".into(),
        cache: std::sync::Arc::new(crate::cache::ResolveCache::new(0)),
    }
}

fn op(name: &str, payload: serde_json::Value) -> crate::models::ApplyOp {
    crate::models::ApplyOp {
        op: name.into(),
        payload,
    }
}

async fn apply(db: &testdb::TestDb, brain: &str, ops: Vec<crate::models::ApplyOp>) -> Vec<(String, Option<String>)> {
    crate::apply_ops(&state_for(db), brain, &ops)
        .await
        .into_iter()
        .map(|r| (r.outcome.to_string(), r.code))
        .collect()
}

fn outcomes(results: &[(String, Option<String>)]) -> Vec<&str> {
    results.iter().map(|(o, _)| o.as_str()).collect()
}

/// The verdicts a drain acts on, in the situations a drain meets: a fresh
/// registration, its replay, a dependency not yet delivered, a name held by
/// the wrong node, a refusal that will never clear — and the rule that the
/// batch stops at the first row that must hold its place.
#[tokio::test]
async fn apply_answers_one_verdict_per_op_and_holds_order() {
    let Some(db) = testdb::fresh().await else { return };
    use serde_json::json;

    // A brain registers an identity and a work, then replays both.
    let node = |kind: &str, name: &str| {
        op("node", json!({"kind": kind, "name": name, "namespace": name.split(':').next().unwrap()}))
    };
    let r = apply(&db, "elohim-veni", vec![
        node("identity", "pial:alice"),
        node("identity", "pial:alice"),
        node("work", "uuid:w1"),
    ]).await;
    assert_eq!(outcomes(&r), ["applied", "already", "applied"], "{r:?}");

    // A work replying to a work that has not been registered yet holds the
    // batch; the rows behind it are untouched.
    let r = apply(&db, "feed-engine", vec![
        op("edge", json!({"subject_name": "uuid:w1", "predicate": "replies_to", "object_name": "uuid:w0"})),
        node("work", "uuid:w2"),
    ]).await;
    assert_eq!(outcomes(&r), ["retry", "skipped"], "{r:?}");
    assert_eq!(r[0].1.as_deref(), Some("object_unresolved"));

    // Deliver the parent, then the same edge lands, and replays as already.
    let r = apply(&db, "feed-engine", vec![
        node("work", "uuid:w0"),
        op("edge", json!({"subject_name": "uuid:w1", "predicate": "replies_to", "object_name": "uuid:w0"})),
        op("edge", json!({"subject_name": "uuid:w1", "predicate": "replies_to", "object_name": "uuid:w0"})),
        // A loop is refused for good and does NOT stop the batch.
        op("edge", json!({"subject_name": "uuid:w0", "predicate": "replies_to", "object_name": "uuid:w1"})),
        node("work", "uuid:w2"),
    ]).await;
    assert_eq!(outcomes(&r), ["applied", "applied", "already", "refused", "applied"], "{r:?}");
    assert_eq!(r[3].1.as_deref(), Some("edge_cycle"));

    // A handle bound by the registrar, replayed, then claimed for someone else.
    let bind = |handle: &str, to: &str| {
        op("name", json!({"node_name": to, "name": handle, "namespace": "handle"}))
    };
    let r = apply(&db, "registry-brain", vec![
        bind("handle:alice", "pial:alice"),
        bind("handle:alice", "pial:alice"),
    ]).await;
    assert_eq!(outcomes(&r), ["applied", "already"], "{r:?}");
    apply(&db, "elohim-veni", vec![node("identity", "pial:mallory")]).await;
    let r = apply(&db, "registry-brain", vec![
        bind("handle:alice", "pial:mallory"),
        bind("handle:other", "pial:mallory"),
    ]).await;
    assert_eq!(outcomes(&r), ["retry", "skipped"], "{r:?}");
    assert_eq!(r[0].1.as_deref(), Some("name_held_elsewhere"));

    // A bind whose node is not registered yet is a dependency, not a refusal.
    let r = apply(&db, "registry-brain", vec![bind("handle:ghost", "pial:nobody")]).await;
    assert_eq!(outcomes(&r), ["retry"]);
    assert_eq!(r[0].1.as_deref(), Some("node_unresolved"));

    // Revoke, replay the revoke, then try to mint the never-reissued name again.
    let r = apply(&db, "elohim-veni", vec![
        op("revoke_name", json!({"name": "pial:alice"})),
        op("revoke_name", json!({"name": "pial:alice"})),
        op("revoke_name", json!({"name": "pial:never-existed"})),
        node("identity", "pial:alice"),
        node("identity", "pial:bob"),
    ]).await;
    assert_eq!(outcomes(&r), ["applied", "already", "already", "refused", "applied"], "{r:?}");
    assert_eq!(r[3].1.as_deref(), Some("name_never_reissued"));

    // Associations: assert, replay, retract, replay the retraction, and a
    // retraction of a pair that cannot exist.
    let follow = |o: &str, s: &str, t: &str| op(o, json!({"assoc": "follows", "subject_name": s, "object_name": t}));
    let r = apply(&db, "feed-engine", vec![
        follow("assoc", "pial:bob", "pial:mallory"),
        follow("assoc", "pial:bob", "pial:mallory"),
        follow("unassoc", "pial:bob", "pial:mallory"),
        follow("unassoc", "pial:bob", "pial:mallory"),
        follow("unassoc", "pial:bob", "pial:nobody"),
        follow("assoc", "pial:bob", "pial:nobody"),
        follow("assoc", "pial:mallory", "pial:bob"),
    ]).await;
    assert_eq!(outcomes(&r), ["applied", "already", "applied", "already", "already", "retry", "skipped"], "{r:?}");

    // The wrong brain, a malformed payload, and an op from the future.
    let r = apply(&db, "verity", vec![
        op("node", json!({"kind": "work"})),
        op("teleport", json!({})),
        follow("assoc", "pial:bob", "pial:mallory"),
    ]).await;
    assert_eq!(outcomes(&r), ["refused", "retry", "skipped"], "{r:?}");
    assert_eq!(r[0].1.as_deref(), Some("malformed_payload"));
    assert_eq!(r[1].1.as_deref(), Some("unknown_op"));
    let r = apply(&db, "verity", vec![follow("assoc", "pial:bob", "pial:mallory")]).await;
    assert_eq!(outcomes(&r), ["retry"]);
    assert_eq!(r[0].1.as_deref(), Some("assoc_forbidden"));
    db.finish().await;
}

// ── may_bind_name ─────────────────────────────────────────────────────────────

async fn verdict(pool: &PgPool, name: &str, ns: &str, reclaimer: Option<Uuid>) -> BindVerdict {
    let mut tx = pool.begin().await.unwrap();
    let v = db::may_bind_name(&mut tx, name, ns, reclaimer).await.unwrap();
    tx.rollback().await.unwrap();
    v
}

#[tokio::test]
async fn a_fresh_name_is_bindable_in_every_namespace() {
    let Some(db) = testdb::fresh().await else { return };
    for ns in ["pial", "handle", "number", "addr", "no_such_namespace"] {
        assert_eq!(verdict(&db.pool, &format!("{ns}:fresh"), ns, None).await, BindVerdict::Bindable, "{ns}");
    }
    db.finish().await;
}

#[tokio::test]
async fn a_revoked_name_follows_its_namespace_rule() {
    let Some(db) = testdb::fresh().await else { return };
    db.named("identity", "pial", "pial:one").await;
    db.revoke("pial:one", 0).await;
    assert_eq!(verdict(&db.pool, "pial:one", "pial", None).await, BindVerdict::NeverReissued);
    // Case-folded: the same name in another spelling is the same name.
    assert_eq!(verdict(&db.pool, "PIAL:ONE", "pial", None).await, BindVerdict::NeverReissued);

    db.named("identity", "handle", "handle:bob").await;
    db.revoke("handle:bob", 0).await;
    assert_eq!(verdict(&db.pool, "handle:bob", "handle", None).await, BindVerdict::Bindable);

    // A namespace nobody has claimed decides against reuse rather than for it.
    let mut tx = db.pool.begin().await.unwrap();
    sqlx::query("INSERT INTO nodes (kind, owner) VALUES ('work', 'test')").execute(&mut *tx).await.unwrap();
    tx.commit().await.unwrap();
    db.named("work", "mystery", "mystery:x").await;
    db.revoke("mystery:x", 0).await;
    assert_eq!(verdict(&db.pool, "mystery:x", "mystery", None).await, BindVerdict::NeverReissued);
    db.finish().await;
}

#[tokio::test]
async fn a_number_is_held_for_the_quarantine_and_then_comes_back() {
    let Some(db) = testdb::fresh().await else { return };
    let year = 365 * 24 * 60 * 60;
    db.named("number", "number", "number:000000000018").await;
    db.revoke("number:000000000018", year - 3600).await;
    assert_eq!(verdict(&db.pool, "number:000000000018", "number", None).await, BindVerdict::InQuarantine);

    db.named("number", "number", "number:000000000026").await;
    db.revoke("number:000000000026", year + 3600).await;
    assert_eq!(verdict(&db.pool, "number:000000000026", "number", None).await, BindVerdict::Bindable);
    db.finish().await;
}

#[tokio::test]
async fn the_previous_holder_reclaims_through_the_quarantine_but_a_stranger_in_the_history_blocks_it() {
    let Some(db) = testdb::fresh().await else { return };
    let owner = db.node("identity").await;
    let stranger = db.node("identity").await;

    // Number held by owner, retired yesterday.
    let n1 = db.named("number", "number", "number:000000000034").await;
    insert_resolves_to(&db.pool, n1, owner).await;
    db.revoke("number:000000000034", 86_400).await;
    assert_eq!(verdict(&db.pool, "number:000000000034", "number", None).await, BindVerdict::InQuarantine);
    assert_eq!(verdict(&db.pool, "number:000000000034", "number", Some(stranger)).await, BindVerdict::InQuarantine);
    assert_eq!(verdict(&db.pool, "number:000000000034", "number", Some(owner)).await, BindVerdict::Bindable);

    // The same Number once belonged to somebody else: the waiver cannot be
    // assembled out of a stranger's history.
    let n2 = db.named("number", "number", "number:000000000034").await;
    insert_resolves_to(&db.pool, n2, stranger).await;
    db.revoke("number:000000000034", 3_600).await;
    assert_eq!(verdict(&db.pool, "number:000000000034", "number", Some(owner)).await, BindVerdict::InQuarantine);
    assert_eq!(verdict(&db.pool, "number:000000000034", "number", Some(stranger)).await, BindVerdict::InQuarantine);
    db.finish().await;
}

async fn insert_resolves_to(pool: &PgPool, number_node: Uuid, identity: Uuid) {
    let mut tx = pool.begin().await.unwrap();
    db::insert_edge(&mut tx, number_node, "resolves_to", identity)
        .await
        .ok()
        .expect("resolves_to edge");
    tx.commit().await.unwrap();
}

#[tokio::test]
async fn name_was_held_by_answers_only_about_the_caller() {
    let Some(db) = testdb::fresh().await else { return };
    let owner = db.node("identity").await;
    let other = db.node("identity").await;
    let n = db.named("number", "number", "number:000000000042").await;
    insert_resolves_to(&db.pool, n, owner).await;
    assert!(!db::name_was_held_by(&db.pool, "number:000000000042", owner).await.unwrap(), "still live");
    db.revoke("number:000000000042", 0).await;
    assert!(db::name_was_held_by(&db.pool, "number:000000000042", owner).await.unwrap());
    assert!(!db::name_was_held_by(&db.pool, "number:000000000042", other).await.unwrap());
    db.finish().await;
}

// ── Associations ──────────────────────────────────────────────────────────────

#[tokio::test]
async fn assoc_range_pages_newest_first_without_gaps_or_repeats() {
    let Some(db) = testdb::fresh().await else { return };
    let me = db.named("identity", "pial", "pial:me").await;
    let mut followed = Vec::new();
    for i in 0..7 {
        let id = db.named("identity", "pial", &format!("pial:f{i}")).await;
        db::insert_assoc(&db.pool, me, "follows", id).await.unwrap();
        // Distinct timestamps, so the ordering under test is time and not luck.
        sqlx::query("UPDATE assocs SET created_at = NOW() - ($1 * INTERVAL '1 minute') WHERE object = $2")
            .bind(10 - i as i64)
            .bind(id)
            .execute(&db.pool)
            .await
            .unwrap();
        followed.push(id);
    }
    // One follower, to prove the sides are separate.
    let fan = db.named("identity", "pial", "pial:fan").await;
    db::insert_assoc(&db.pool, fan, "follows", me).await.unwrap();

    let mut seen = Vec::new();
    let mut after = None;
    loop {
        let page = db::assoc_range(&db.pool, "follows", me, AssocSide::Out, after, 3).await.unwrap();
        if page.is_empty() {
            break;
        }
        assert!(page.len() <= 3);
        let last = page.last().unwrap();
        after = Some((last.created_at, last.node_id));
        seen.extend(page.into_iter().map(|r| (r.node_id, r.name)));
    }
    // Newest first: f6 was followed most recently.
    let expect: Vec<(Uuid, Option<String>)> = followed
        .iter()
        .enumerate()
        .rev()
        .map(|(i, id)| (*id, Some(format!("pial:f{i}"))))
        .collect();
    assert_eq!(seen, expect);
    assert_eq!(seen.iter().map(|(id, _)| *id).collect::<HashSet<_>>().len(), 7, "no repeats");

    let inbound = db::assoc_range(&db.pool, "follows", me, AssocSide::In, None, 10).await.unwrap();
    assert_eq!(inbound.len(), 1);
    assert_eq!(inbound[0].node_id, fan);
    assert_eq!(db::assoc_count(&db.pool, "follows", me).await.unwrap(), (7, 1));
    assert_eq!(db::assoc_count(&db.pool, "follows", fan).await.unwrap(), (1, 0));
    db.finish().await;
}

#[tokio::test]
async fn assoc_range_keyset_splits_rows_that_share_a_timestamp() {
    let Some(db) = testdb::fresh().await else { return };
    let me = db.named("identity", "pial", "pial:me").await;
    let stamp = Utc::now() - Duration::minutes(5);
    let mut ids = Vec::new();
    for i in 0..5 {
        let id = db.named("identity", "pial", &format!("pial:s{i}")).await;
        sqlx::query("INSERT INTO assocs (subject, assoc, object, created_at) VALUES ($1, 'follows', $2, $3)")
            .bind(me)
            .bind(id)
            .bind(stamp)
            .execute(&db.pool)
            .await
            .unwrap();
        ids.push(id);
    }
    let mut seen = HashSet::new();
    let mut after = None;
    loop {
        let page = db::assoc_range(&db.pool, "follows", me, AssocSide::Out, after, 2).await.unwrap();
        if page.is_empty() {
            break;
        }
        let last = page.last().unwrap();
        after = Some((last.created_at, last.node_id));
        for r in page {
            assert!(seen.insert(r.node_id), "row repeated across pages");
        }
    }
    assert_eq!(seen.len(), 5);
    db.finish().await;
}
