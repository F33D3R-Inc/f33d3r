//! Database-backed tests. See testdb.rs for how they are gated.
//!
//! What is pinned here is what only the database can prove: the
//! compare-and-set, the partial unique indexes, the outbox row committing
//! with the change that caused it, and the idempotency store.

use serde_json::json;
use uuid::Uuid;

use crate::domain::{FrequencyRole, FrequencyState, RequestStatus, Visibility};
use crate::events::{Event, EventType};
use crate::repository::postgres::{self as pg, NewFrequency, Patch};
use crate::testdb;

const HOST: &str = "pial:c0ffee00-0000-4000-8000-000000000001";
const ALICE: &str = "pial:c0ffee00-0000-4000-8000-000000000002";
const BOB: &str = "pial:c0ffee00-0000-4000-8000-000000000003";

fn new_freq(host: &str) -> NewFrequency {
    NewFrequency {
        host_pial: host.into(),
        title: "Late Night Tech Talk".into(),
        description: String::new(),
        state: FrequencyState::Draft,
        visibility: Visibility::Public,
        language: "en".into(),
        adult_content: false,
        speaker_verity_min_tier: 0,
        scheduled_at: None,
        recording_enabled: false,
        max_speakers: 10,
        max_listeners: 1000,
    }
}

#[tokio::test]
async fn create_reads_back_and_registers_the_host_in_the_naming_outbox() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();

    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();
    assert_eq!(f.state, FrequencyState::Draft);
    assert_eq!(f.version, 1);
    assert_eq!(f.visibility, Visibility::Public);

    let again = pg::get_frequency(&mut conn, f.id).await.unwrap().unwrap();
    assert_eq!(again.id, f.id);
    assert_eq!(again.title, "Late Night Tech Talk");

    // The trigger queued the host's identity registration.
    let (n,): (i64,) = sqlx::query_as("SELECT COUNT(*) FROM manhattan_outbox WHERE dedup_key = $1")
        .bind(format!("node:{HOST}"))
        .fetch_one(&db.pool)
        .await
        .unwrap();
    assert_eq!(n, 1);

    drop(conn);
    db.finish().await;
}

/// THE concurrency pin (§51): two writers holding the same version, one wins.
#[tokio::test]
async fn compare_and_set_lets_exactly_one_writer_through() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();

    let first = pg::cas_update(
        &mut conn,
        f.id,
        f.version,
        &Patch::state(FrequencyState::Starting),
    )
    .await
    .unwrap();
    assert!(first.is_some());
    assert_eq!(first.as_ref().unwrap().version, 2);

    let second = pg::cas_update(
        &mut conn,
        f.id,
        f.version,
        &Patch::state(FrequencyState::Cancelled),
    )
    .await
    .unwrap();
    assert!(second.is_none(), "a stale version must not write");

    let now = pg::get_frequency(&mut conn, f.id).await.unwrap().unwrap();
    assert_eq!(now.state, FrequencyState::Starting);
    assert_eq!(now.version, 2);

    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn a_host_has_at_most_one_open_frequency() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();

    let a = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();
    let b = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();
    pg::cas_update(
        &mut conn,
        a.id,
        a.version,
        &Patch::state(FrequencyState::Live),
    )
    .await
    .unwrap();

    let err = pg::cas_update(
        &mut conn,
        b.id,
        b.version,
        &Patch::state(FrequencyState::Live),
    )
    .await
    .unwrap_err();
    assert!(
        err.to_string().contains("uq_frequencies_one_open_per_host"),
        "{err}"
    );

    let open = pg::open_for_host(&mut conn, HOST).await.unwrap().unwrap();
    assert_eq!(open.id, a.id);

    drop(conn);
    db.finish().await;
}

/// The outbox contract: the event row commits with the change, or neither
/// does.
#[tokio::test]
async fn event_and_state_commit_together_or_not_at_all() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();
    drop(conn);

    // Rolled back: no state change, no event.
    {
        let mut tx = db.pool.begin().await.unwrap();
        pg::cas_update(
            &mut tx,
            f.id,
            f.version,
            &Patch::state(FrequencyState::Starting),
        )
        .await
        .unwrap();
        pg::insert_event(
            &mut tx,
            &Event::new(f.id, EventType::Started, "service", "t", json!({})),
        )
        .await
        .unwrap();
        tx.rollback().await.unwrap();
    }
    let mut conn = db.pool.acquire().await.unwrap();
    assert_eq!(
        pg::get_frequency(&mut conn, f.id)
            .await
            .unwrap()
            .unwrap()
            .state,
        FrequencyState::Draft
    );
    assert_eq!(pg::unpublished_events(&db.pool).await.unwrap(), 0);

    // Committed: both.
    {
        let mut tx = db.pool.begin().await.unwrap();
        pg::cas_update(
            &mut tx,
            f.id,
            f.version,
            &Patch::state(FrequencyState::Starting),
        )
        .await
        .unwrap();
        pg::insert_event(
            &mut tx,
            &Event::new(f.id, EventType::Started, "service", "t", json!({})),
        )
        .await
        .unwrap();
        tx.commit().await.unwrap();
    }
    assert_eq!(
        pg::get_frequency(&mut conn, f.id)
            .await
            .unwrap()
            .unwrap()
            .state,
        FrequencyState::Starting
    );
    assert_eq!(pg::unpublished_events(&db.pool).await.unwrap(), 1);
    let evs = pg::list_events(&db.pool, f.id, 10).await.unwrap();
    assert_eq!(evs.len(), 1);
    assert_eq!(evs[0].event_type, "frequency.started");
    assert_eq!(evs[0].schema_version, "1.0");

    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn joining_twice_is_one_row_and_leaving_reopens_on_rejoin() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();

    let (p1, fresh1) = pg::join(&mut conn, f.id, ALICE, FrequencyRole::Listener)
        .await
        .unwrap();
    let (p2, fresh2) = pg::join(&mut conn, f.id, ALICE, FrequencyRole::Listener)
        .await
        .unwrap();
    assert!(fresh1);
    assert!(!fresh2);
    assert_eq!(p1.id, p2.id);
    assert_eq!(pg::joined(&mut conn, f.id).await.unwrap().len(), 1);

    let left = pg::leave(&mut conn, f.id, ALICE).await.unwrap();
    assert!(left.is_some());
    assert!(pg::leave(&mut conn, f.id, ALICE).await.unwrap().is_none());

    let (p3, fresh3) = pg::join(&mut conn, f.id, ALICE, FrequencyRole::Listener)
        .await
        .unwrap();
    assert!(fresh3);
    assert_ne!(p3.id, p1.id, "a rejoin is a new visit");

    drop(conn);
    db.finish().await;
}

/// Two moderators approve the same request: one approval, one no-op.
#[tokio::test]
async fn a_request_resolves_once() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();
    pg::join(&mut conn, f.id, ALICE, FrequencyRole::Listener)
        .await
        .unwrap();

    let r = pg::create_request(&mut conn, f.id, ALICE, "I run a node")
        .await
        .unwrap()
        .unwrap();
    assert!(pg::create_request(&mut conn, f.id, ALICE, "again")
        .await
        .unwrap()
        .is_none());

    let a = pg::resolve_request(&mut conn, r.id, RequestStatus::Approved, Some(HOST))
        .await
        .unwrap();
    let b = pg::resolve_request(&mut conn, r.id, RequestStatus::Declined, Some(BOB))
        .await
        .unwrap();
    assert!(a.is_some());
    assert!(b.is_none());
    assert_eq!(
        pg::get_request(&mut conn, r.id)
            .await
            .unwrap()
            .unwrap()
            .status,
        RequestStatus::Approved
    );

    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn upvotes_count_once_per_person_and_order_the_queue() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();

    let first = pg::create_request(&mut conn, f.id, ALICE, "a")
        .await
        .unwrap()
        .unwrap();
    let second = pg::create_request(&mut conn, f.id, BOB, "b")
        .await
        .unwrap()
        .unwrap();
    assert!(pg::upvote_request(&mut conn, second.id, HOST)
        .await
        .unwrap());
    assert!(!pg::upvote_request(&mut conn, second.id, HOST)
        .await
        .unwrap());

    let queue = pg::pending_requests(&mut conn, f.id).await.unwrap();
    assert_eq!(
        queue.iter().map(|r| r.id).collect::<Vec<_>>(),
        vec![second.id, first.id]
    );
    assert_eq!(queue[0].upvotes, 1);

    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn granted_roles_survive_and_revoke() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();

    assert_eq!(
        pg::granted_role(&mut conn, f.id, ALICE).await.unwrap(),
        None
    );
    pg::grant_role(&mut conn, f.id, ALICE, FrequencyRole::CoHost, HOST)
        .await
        .unwrap();
    assert_eq!(
        pg::granted_role(&mut conn, f.id, ALICE).await.unwrap(),
        Some(FrequencyRole::CoHost)
    );
    assert_eq!(
        pg::cohosts(&mut conn, f.id).await.unwrap(),
        vec![ALICE.to_string()]
    );

    // Re-granting replaces rather than duplicating.
    pg::grant_role(&mut conn, f.id, ALICE, FrequencyRole::Speaker, HOST)
        .await
        .unwrap();
    assert_eq!(
        pg::granted_role(&mut conn, f.id, ALICE).await.unwrap(),
        Some(FrequencyRole::Speaker)
    );
    assert!(pg::cohosts(&mut conn, f.id).await.unwrap().is_empty());

    assert!(pg::revoke_role(&mut conn, f.id, ALICE, HOST).await.unwrap());
    assert_eq!(
        pg::granted_role(&mut conn, f.id, ALICE).await.unwrap(),
        None
    );
    assert!(!pg::revoke_role(&mut conn, f.id, ALICE, HOST).await.unwrap());

    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn blocks_are_durable_and_idempotent() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();

    assert!(!pg::is_blocked(&mut conn, f.id, ALICE).await.unwrap());
    assert!(pg::block(&mut conn, f.id, ALICE, HOST, "spam")
        .await
        .unwrap());
    assert!(!pg::block(&mut conn, f.id, ALICE, HOST, "spam")
        .await
        .unwrap());
    assert!(pg::is_blocked(&mut conn, f.id, ALICE).await.unwrap());
    assert!(pg::unblock(&mut conn, f.id, ALICE).await.unwrap());
    assert!(!pg::is_blocked(&mut conn, f.id, ALICE).await.unwrap());

    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn idempotency_store_replays_the_first_answer() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();

    assert!(pg::idempotent_get(&mut conn, HOST, "k1")
        .await
        .unwrap()
        .is_none());
    pg::idempotent_put(&mut conn, HOST, "k1", 201, &json!({ "a": 1 }))
        .await
        .unwrap();
    pg::idempotent_put(&mut conn, HOST, "k1", 500, &json!({ "a": 2 }))
        .await
        .unwrap();
    let (status, body) = pg::idempotent_get(&mut conn, HOST, "k1")
        .await
        .unwrap()
        .unwrap();
    assert_eq!(status, 201);
    assert_eq!(body, json!({ "a": 1 }));
    // Keys are per actor.
    assert!(pg::idempotent_get(&mut conn, ALICE, "k1")
        .await
        .unwrap()
        .is_none());

    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn the_schema_refuses_a_handle_as_an_identity() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let err = pg::insert_frequency(&mut conn, &new_freq("handle:someone"))
        .await
        .unwrap_err();
    assert!(err.to_string().contains("host_pial"), "{err}");
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();
    let err = pg::join(&mut conn, f.id, "someone", FrequencyRole::Listener)
        .await
        .unwrap_err();
    assert!(err.to_string().contains("pial"), "{err}");
    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn ending_departs_everyone_and_expires_the_queue() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();
    pg::join(&mut conn, f.id, HOST, FrequencyRole::Host)
        .await
        .unwrap();
    pg::join(&mut conn, f.id, ALICE, FrequencyRole::Listener)
        .await
        .unwrap();
    pg::join(&mut conn, f.id, BOB, FrequencyRole::Listener)
        .await
        .unwrap();
    pg::create_request(&mut conn, f.id, ALICE, "")
        .await
        .unwrap();

    assert_eq!(pg::leave_all(&mut conn, f.id).await.unwrap(), 3);
    assert_eq!(pg::expire_requests(&mut conn, f.id).await.unwrap(), 1);
    assert!(pg::joined(&mut conn, f.id).await.unwrap().is_empty());
    assert!(pg::pending_requests(&mut conn, f.id)
        .await
        .unwrap()
        .is_empty());

    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn unknown_frequency_is_none() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    assert!(pg::get_frequency(&mut conn, Uuid::new_v4())
        .await
        .unwrap()
        .is_none());
    drop(conn);
    db.finish().await;
}

#[tokio::test]
async fn current_participation_is_the_live_frequency_a_person_is_joined_to() {
    let Some(db) = testdb::fresh().await else {
        return;
    };
    let mut conn = db.pool.acquire().await.unwrap();
    let f = pg::insert_frequency(&mut conn, &new_freq(HOST))
        .await
        .unwrap();
    assert!(pg::current_for_participant(&mut conn, ALICE)
        .await
        .unwrap()
        .is_none());

    pg::join(&mut conn, f.id, ALICE, FrequencyRole::Listener)
        .await
        .unwrap();
    // Joined, but the Frequency is a draft: nothing to dock.
    assert!(pg::current_for_participant(&mut conn, ALICE)
        .await
        .unwrap()
        .is_none());

    pg::cas_update(
        &mut conn,
        f.id,
        f.version,
        &Patch::state(FrequencyState::Live),
    )
    .await
    .unwrap();
    assert_eq!(
        pg::current_for_participant(&mut conn, ALICE)
            .await
            .unwrap()
            .unwrap()
            .id,
        f.id
    );

    pg::leave(&mut conn, f.id, ALICE).await.unwrap();
    assert!(pg::current_for_participant(&mut conn, ALICE)
        .await
        .unwrap()
        .is_none());

    drop(conn);
    db.finish().await;
}
