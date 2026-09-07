//! verity's Manhattan outbox drain.
//!
//! The drain itself is the shared `manhattan-outbox` crate — one drain for
//! every Rust brain, in place of the ten copies that used to live in files
//! like this one and drifted apart. See libs/manhattan-outbox/src/lib.rs for
//! what it does and why. This module is the seam: it hands the crate this
//! brain's name and database URL, and keeps the one read `/health` makes,
//! which runs on this brain's own pool because the crate's pool is not the
//! same sqlx as this brain's.

use std::time::Duration;

use manhattan_client::Manhattan;


/// Starts the drain for the life of the process. The crate opens its own
/// connections from `database_url`; if the database is not there yet at boot
/// this keeps trying, loudly, rather than running the brain without a drain.
pub fn spawn(database_url: String, manhattan: Manhattan) {
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            match manhattan_outbox::connect("verity", &database_url).await {
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
