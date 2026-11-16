//! Writes this brain's notifications to its outbox, from which they reach Herald.

pub use notification_outbox::Event;

/// Queues one notification on a pool or inside the caller's transaction.
pub async fn enqueue<'e, E>(exec: E, ev: &Event) -> Result<(), sqlx::Error>
where
    E: sqlx::Executor<'e, Database = sqlx::Postgres>,
{
    sqlx::query(notification_outbox::INSERT_SQL)
        .bind(&ev.event_id)
        .bind(&ev.recipient_pial)
        .bind(ev.to_json())
        .execute(exec)
        .await?;
    Ok(())
}

/// A notification fixed now and sent later, when the charge it announces
/// settles. Stored on the charge row as `{event_id, recipient_pial, body}`.
pub fn to_stored(ev: &Event) -> serde_json::Value {
    serde_json::json!({
        "event_id": ev.event_id,
        "recipient_pial": ev.recipient_pial,
        "body": ev.to_json(),
    })
}

/// Queues a stored notification inside the caller's transaction.
pub async fn enqueue_stored<'e, E>(exec: E, stored: &serde_json::Value) -> Result<(), sqlx::Error>
where
    E: sqlx::Executor<'e, Database = sqlx::Postgres>,
{
    let field = |k: &str| stored.get(k).and_then(|v| v.as_str()).unwrap_or_default().to_string();
    sqlx::query(notification_outbox::INSERT_SQL)
        .bind(field("event_id"))
        .bind(field("recipient_pial"))
        .bind(field("body"))
        .execute(exec)
        .await?;
    Ok(())
}
