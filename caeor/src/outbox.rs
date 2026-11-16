//! Caeor's Manhattan outbox drain.
//!
//! The drain is the shared `manhattan-outbox` crate, as in every other brain
//! (see libs/manhattan-outbox/src/lib.rs). The rows are written by the asset
//! trigger in migrations/0002_manhattan_outbox.sql: each asset becomes a
//! `media` node, media:<asset_id>, authored_by its uploader's pial node.

use std::time::Duration;

use manhattan_client::Manhattan;

/// Starts the drain for the life of the process. If the database is not
/// reachable yet, keeps trying, loudly, rather than running without a drain.
pub fn spawn(database_url: String, manhattan: Manhattan) {
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            match manhattan_outbox::connect("caeor", &database_url).await {
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
