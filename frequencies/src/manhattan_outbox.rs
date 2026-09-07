//! Auralis's Manhattan outbox drain.
//!
//! The drain itself is the shared `manhattan-outbox` crate — one drain for
//! every Rust brain. See libs/manhattan-outbox/src/lib.rs for what it does and
//! why. This module is the seam: it hands the crate this brain's name and
//! database URL, and keeps the one read `/health` makes, which runs on this
//! brain's own pool.

use std::time::Duration;

use manhattan_client::Manhattan;
use sqlx::PgPool;

/// Starts the drain for the life of the process. The crate opens its own
/// connections from `database_url`; if the database is not there yet at boot
/// this keeps trying, loudly, rather than running the brain without a drain.
pub fn spawn(database_url: String, manhattan: Manhattan) {
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            match manhattan_outbox::connect("auralis", &database_url).await {
                Ok(outbox) => {
                    outbox.spawn(manhattan);
                    return;
                }
                Err(e) => {
                    tracing::error!(error = %e, "manhattan outbox drain cannot open its database pool; retrying");
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(Duration::from_secs(30));
                }
            }
        }
    });
}

/// Rows queued and not yet delivered, quarantined ones included. Read on
/// `/health` through this brain's own pool: a number that only grows is a
/// naming plane that has stopped accepting writes.
pub async fn backlog(pool: &PgPool) -> sqlx::Result<i64> {
    let (n,): (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL")
            .fetch_one(pool)
            .await?;
    Ok(n)
}
