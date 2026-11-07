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
