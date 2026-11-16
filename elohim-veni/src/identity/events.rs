//! identity.events — what this brain tells the estate about a person.
//!
//! Each event is a full SNAPSHOT of the current state, not a delta: the
//! account (its PIAL's facts, role, status, capabilities and trust folded on),
//! the PIAL when it has no account, or the handle with its identity. A consumer
//! keeping a read projection applies the latest snapshot per account (or per
//! PIAL) and needs nothing else. Every record is keyed by pial_id, so all of
//! one identity's records land in one partition, in order.
//!
//! Rows are written to identity_outbox inside the caller's transaction. The
//! relay (relay.rs) adds `seq` — the outbox id, strictly increasing — when it
//! publishes, so a consumer can refuse a snapshot older than the one it holds
//! even across partitions (an account linked to another identity changes
//! partition, because it changes key).
//!
//! Payload:
//!   { event_id, seq, type, occurred_at, pial_id, account_id, handle,
//!     is_primary, account_created_at, deactivated_at, role, status, tier,
//!     caps, cap_states, facts, trust, tombstoned_at, detail }

use anyhow::{Context, Result};
use chrono::Utc;
use serde_json::{json, Value};
use sqlx::PgConnection;
use uuid::Uuid;

use super::view::{self, AccountRow, PialView};
use super::vocab;

/// Queues one record, keyed by its identity.
async fn enqueue(conn: &mut PgConnection, event_id: Uuid, pial_id: Uuid, event: Value) -> Result<()> {
    let key = pial_id.to_string();
    sqlx::query(
        "INSERT INTO identity_outbox (event_id, partition_key, record_key, event)
         VALUES ($1, $2, $3, $4)",
    )
    .bind(event_id)
    .bind(&key)
    .bind(&key)
    .bind(event)
    .execute(conn)
    .await
    .context("identity outbox: enqueue")?;
    Ok(())
}

fn snapshot(
    event_id: Uuid,
    event_type: &str,
    pial: &PialView,
    account: Option<&AccountRow>,
    detail: &Value,
) -> Value {
    let now = Utc::now();
    json!({
        "event_id": event_id,
        "type": event_type,
        "occurred_at": now,
        "pial_id": pial.pial_id,
        "account_id": account.map(|a| a.account_id),
        "handle": account.map(|a| a.handle.clone()),
        "is_primary": account.map(|a| a.is_primary),
        "account_created_at": account.map(|a| a.created_at),
        "deactivated_at": account.and_then(|a| a.deactivated_at),
        "purged_at": account.and_then(|a| a.purged_at),
        "role": pial.role,
        // The account's status when the record is about one, else the
        // identity's; pial_status is always the identity's.
        "status": match account {
            Some(a) => vocab::account_status(&pial.status, a.deactivated_at.is_some(), a.purged_at.is_some()).to_string(),
            None => pial.status.clone(),
        },
        "pial_status": pial.status,
        "tier": pial.tier,
        "caps": pial.granted(now),
        "cap_states": pial.cap_states_json(),
        "facts": pial.facts,
        "trust": pial.trust,
        "tombstoned_at": pial.tombstoned_at,
        "detail": detail,
    })
}

async fn load(conn: &mut PgConnection, pial_id: Uuid) -> Result<PialView> {
    view::load_pial(conn, pial_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("identity event for missing PIAL {pial_id}"))
}

/// A change to the identity itself (facts, role, status, capabilities, trust):
/// one snapshot per account it holds, or one for the PIAL when it holds none.
pub async fn emit_pial(
    conn: &mut PgConnection,
    pial_id: Uuid,
    event_type: &str,
    detail: Value,
) -> Result<()> {
    let pial = load(conn, pial_id).await?;
    if pial.accounts.is_empty() {
        let id = Uuid::new_v4();
        let ev = snapshot(id, event_type, &pial, None, &detail);
        return enqueue(conn, id, pial_id, ev).await;
    }
    for account in &pial.accounts {
        let id = Uuid::new_v4();
        let ev = snapshot(id, event_type, &pial, Some(account), &detail);
        enqueue(conn, id, pial_id, ev).await?;
    }
    Ok(())
}

/// A change to one account (created, deactivated, reactivated, linked, made
/// primary): a snapshot of that account.
pub async fn emit_account(
    conn: &mut PgConnection,
    account_id: Uuid,
    event_type: &str,
    detail: Value,
) -> Result<()> {
    let (account, pial) = view::load_account_view(conn, account_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("identity event for missing account {account_id}"))?;
    let id = Uuid::new_v4();
    let ev = snapshot(id, event_type, &pial, Some(&account), &detail);
    enqueue(conn, id, pial.pial_id, ev).await
}

/// A change in the handle registry: the handle, with the full current state of
/// the identity it points at (and of the account presenting it, if any).
pub async fn emit_handle(
    conn: &mut PgConnection,
    handle: &str,
    event_type: &str,
    detail: Value,
) -> Result<()> {
    let (pial_id, status, tier, account_id): (Uuid, String, String, Option<Uuid>) = sqlx::query_as(
        "SELECT h.pial_id, h.status, h.tier, a.id
           FROM handles h LEFT JOIN accounts a ON a.handle = h.handle
          WHERE h.handle = $1::citext",
    )
    .bind(handle)
    .fetch_one(&mut *conn)
    .await
    .with_context(|| format!("identity event for handle {handle}"))?;
    let pial = load(conn, pial_id).await?;
    let account = account_id.and_then(|id| pial.accounts.iter().find(|a| a.account_id == id));
    let id = Uuid::new_v4();
    let mut ev = snapshot(id, event_type, &pial, account, &detail);
    ev["handle"] = json!(handle);
    ev["handle_status"] = json!(status);
    ev["handle_tier"] = json!(tier);
    enqueue(conn, id, pial_id, ev).await
}
