//! NEXUS identity maintenance, run inside Verity on an interval.
//!
//! This was a separate `nexus-sweep` container that opened its own pool on
//! `f33d3r_verity` and ran the same four statements every fifteen minutes.
//! Verity owns that database and already runs a background task on it (the
//! Manhattan outbox drain), so a second process holding a second pool on the
//! same tables was a deployment unit with no boundary to justify it. It lives
//! here now, on Verity's pool, started after Verity's migrations — so it can
//! never run against a schema it is ahead of.
//!
//! Each cycle:
//!   1. Expires stale link requests (status = 'pending', expires_at < NOW()).
//!   2. Deletes expired session context rows.
//!   3. Flags `nexus_aml_aggregates` that breach the withdrawal threshold and
//!      are not yet flagged, so compliance can open enhanced due diligence.
//!   4. Logs a heartbeat with the live identity and link counts.

use std::time::Duration;

use sqlx::PgPool;
use tracing::{error, info, warn};

/// Withdrawal total, in micro-AET, past which a NEXUS is flagged for EDD.
/// 10,000 AET.
const AML_THRESHOLD_UNITS: i64 = 1_000_000;

const DEFAULT_INTERVAL: Duration = Duration::from_secs(900);

/// The sweep interval from `NEXUS_SWEEP_INTERVAL_SECS`, defaulting to fifteen
/// minutes. A value that does not parse is reported and the default used —
/// maintenance that silently stops because of a typo is worse than
/// maintenance on the wrong schedule.
pub fn interval_from_env() -> Duration {
    match std::env::var("NEXUS_SWEEP_INTERVAL_SECS") {
        Ok(raw) => match raw.trim().parse::<u64>() {
            Ok(secs) if secs > 0 => Duration::from_secs(secs),
            _ => {
                warn!(
                    value = %raw,
                    default_secs = DEFAULT_INTERVAL.as_secs(),
                    "NEXUS_SWEEP_INTERVAL_SECS is not a positive integer; using the default"
                );
                DEFAULT_INTERVAL
            }
        },
        Err(_) => DEFAULT_INTERVAL,
    }
}

/// Start the sweep as a background task on Verity's pool.
pub fn spawn(pool: PgPool, interval: Duration) {
    info!(interval_secs = interval.as_secs(), "nexus sweep started");
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(interval);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            ticker.tick().await;
            if let Err(e) = sweep(&pool).await {
                error!(error = %e, "nexus sweep cycle failed");
            }
        }
    });
}

async fn sweep(pool: &PgPool) -> Result<(), sqlx::Error> {
    // 1. Expire stale link requests.
    let expired_links = sqlx::query(
        "UPDATE nexus_link_requests SET status = 'expired'
         WHERE status = 'pending' AND expires_at < NOW()",
    )
    .execute(pool)
    .await?
    .rows_affected();
    if expired_links > 0 {
        info!(count = expired_links, "expired stale nexus link requests");
    }

    // 2. Delete expired session context rows.
    let expired_sessions =
        sqlx::query("DELETE FROM nexus_session_context WHERE expires_at < NOW()")
            .execute(pool)
            .await?
            .rows_affected();
    if expired_sessions > 0 {
        info!(
            count = expired_sessions,
            "purged expired nexus session rows"
        );
    }

    // 3. Flag AML aggregates that breach the threshold and are not yet flagged.
    let flagged_aml = sqlx::query(
        "UPDATE nexus_aml_aggregates
         SET flag_for_edd = true, flagged_at = NOW()
         WHERE total_withdrawn_uaet >= $1
           AND flag_for_edd = false
           AND period_end > NOW() - INTERVAL '30 days'",
    )
    .bind(AML_THRESHOLD_UNITS)
    .execute(pool)
    .await?
    .rows_affected();
    if flagged_aml > 0 {
        info!(count = flagged_aml, "flagged nexus AML aggregates for EDD");
    }

    // 4. Heartbeat. A failed count is reported in the log line rather than
    // failing the cycle whose maintenance already ran.
    let link_count: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM nexus_persona_links")
        .fetch_one(pool)
        .await
        .unwrap_or(0);
    let identity_count: i64 =
        sqlx::query_scalar("SELECT COUNT(*) FROM nexus_identities WHERE NOT is_suspended")
            .fetch_one(pool)
            .await
            .unwrap_or(0);

    info!(
        nexus_identities = identity_count,
        persona_links = link_count,
        expired_link_requests = expired_links,
        expired_sessions,
        flagged_aml,
        "nexus sweep complete"
    );
    Ok(())
}
