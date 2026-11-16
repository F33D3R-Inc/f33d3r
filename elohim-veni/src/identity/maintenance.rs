//! Identity upkeep that is a function of time, not of a request.
//!
//!   * Capability expiry. A timed capability (a cooldown, a grant with an end)
//!     changes what an identity may do the moment it expires, with no write
//!     to say so. Each minute every newly expired row is announced: a ledger
//!     row and an identity.events snapshot carrying the caps now in force.
//!   * The age sweep. is_adult / is_minor are stored facts derived from the
//!     date of birth, and a derived fact about age goes stale on a birthday.
//!     feed-engine never re-derived them, so a 17-year-old stayed a minor
//!     forever. Every identity whose date of birth now makes it an adult is
//!     moved, with a ledger row and an identity.events snapshot.
//!   * Session reaping. Revoked and expired sessions are kept SESSION_EVIDENCE
//!     after they end (device and IP evidence), then deleted.

use std::time::Duration;

use anyhow::Result;
use serde_json::json;
use sqlx::PgPool;
use uuid::Uuid;

use super::{events, ledger};

const EVERY: Duration = Duration::from_secs(60);
/// The age sweep and session reaping run once per this many ticks.
const SLOW_EVERY_TICKS: u64 = 60;
const EXPIRY_BATCH: i64 = 500;
const LOCK_KEY: i64 = 7331_0013;

pub fn spawn(pool: PgPool) {
    tokio::spawn(async move {
        let mut tick: u64 = 0;
        loop {
            if let Err(e) = run_once(&pool, tick % SLOW_EVERY_TICKS == 0).await {
                tracing::error!(error = ?e, "identity maintenance failed; retrying next tick");
                metrics::counter!("identity_maintenance_failures_total").increment(1);
            }
            tick += 1;
            tokio::time::sleep(EVERY).await;
        }
    });
}

async fn run_once(pool: &PgPool, slow: bool) -> Result<()> {
    let mut conn = pool.acquire().await?;
    let locked: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
        .bind(LOCK_KEY)
        .fetch_one(&mut *conn)
        .await?;
    if !locked {
        return Ok(());
    }
    let mut outcome = announce_expiries(pool).await;
    if outcome.is_ok() && slow {
        outcome = sweep(pool).await;
    }
    sqlx::query("SELECT pg_advisory_unlock($1)")
        .bind(LOCK_KEY)
        .execute(&mut *conn)
        .await?;
    outcome
}

async fn announce_expiries(pool: &PgPool) -> Result<()> {
    let mut tx = pool.begin().await?;
    let expired: Vec<(Uuid, String, String)> = sqlx::query_as(
        "UPDATE pial_capabilities SET expiry_announced_at = NOW()
          WHERE (pial_id, capability) IN (
                SELECT pial_id, capability FROM pial_capabilities
                 WHERE expires_at IS NOT NULL AND expires_at <= NOW() AND expiry_announced_at IS NULL
                 ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED)
         RETURNING pial_id, capability, state",
    )
    .bind(EXPIRY_BATCH)
    .fetch_all(&mut *tx)
    .await?;
    let mut by_pial: std::collections::BTreeMap<Uuid, Vec<serde_json::Value>> = Default::default();
    for (pial_id, capability, state) in expired {
        by_pial.entry(pial_id).or_default().push(json!({ "capability": capability, "state": state }));
    }
    for (pial_id, caps) in &by_pial {
        let detail = json!({ "expired": caps });
        ledger::append(&mut tx, *pial_id, None, "capability_expired", detail.clone(), "expiry").await?;
        events::emit_pial(&mut tx, *pial_id, "pial.capability_expired", detail).await?;
    }
    tx.commit().await?;
    if !by_pial.is_empty() {
        tracing::info!(identities = by_pial.len(), "announced capability expiries");
    }
    Ok(())
}

async fn sweep(pool: &PgPool) -> Result<()> {
    let mut tx = pool.begin().await?;
    let came_of_age: Vec<Uuid> = sqlx::query_scalar(
        "UPDATE pial_states SET is_adult = TRUE, is_minor = FALSE, updated_at = NOW()
          WHERE is_minor AND dob IS NOT NULL AND date_part('year', age(dob)) >= 18
            AND status NOT IN ('tombstoned','purged')
         RETURNING pial_id",
    )
    .fetch_all(&mut *tx)
    .await?;
    for pial_id in &came_of_age {
        ledger::append(&mut tx, *pial_id, None, "came_of_age", json!({}), "age-sweep").await?;
        events::emit_pial(&mut tx, *pial_id, "pial.facts_changed", json!({ "changed": ["age"], "by": "age-sweep" })).await?;
    }
    let reaped = sqlx::query(
        "DELETE FROM sessions WHERE COALESCE(revoked_at, expires_at) < NOW() - INTERVAL '30 days'",
    )
    .execute(&mut *tx)
    .await?
    .rows_affected();
    tx.commit().await?;
    if !came_of_age.is_empty() || reaped > 0 {
        tracing::info!(came_of_age = came_of_age.len(), sessions_reaped = reaped, "identity maintenance");
    }
    Ok(())
}
