//! The identity ledger: an append-only record of everything that happened to
//! an identity (ported from feed-engine LogPIALEvent). Unlike its predecessor,
//! a failed append is an error the caller's transaction fails on — an identity
//! change with no ledger row does not commit.

use anyhow::{Context, Result};
use serde_json::Value;
use sqlx::PgConnection;
use uuid::Uuid;

pub async fn append(
    conn: &mut PgConnection,
    pial_id: Uuid,
    account_id: Option<Uuid>,
    event_type: &str,
    payload: Value,
    source: &str,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO pial_event_ledger (pial_id, account_id, event_type, payload, source)
         VALUES ($1, $2, $3, $4, $5)",
    )
    .bind(pial_id)
    .bind(account_id)
    .bind(event_type)
    .bind(payload)
    .bind(source)
    .execute(conn)
    .await
    .with_context(|| format!("ledger: recording {event_type} for PIAL {pial_id}"))?;
    Ok(())
}
