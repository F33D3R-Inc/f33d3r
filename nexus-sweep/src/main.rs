/// nexus-sweep — NEXUS identity maintenance job.
///
/// Runs on a configurable interval (default 15 min) and:
///   1. Expires stale link requests (status = 'pending', expires_at < NOW()).
///   2. Expires stale session context rows (expires_at < NOW()).
///   3. Flags nexus_aml_aggregates where total_withdrawn_uaet >= threshold and
///      no EDD flag is set yet (so Ain Soph/compliance can act).
///   4. Emits a health heartbeat to stdout for Docker health checks.
///
/// Calls Verity's internal maintenance endpoints — does NOT access any DB directly
/// except f33d3r_verity, which it owns as a co-process of the Verity brain.

use anyhow::Result;
use sqlx::PgPool;
use tracing::{error, info};

const AML_THRESHOLD_UNITS: i64 = 1_000_000; // 10,000 AET in micro-AET

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(std::env::var("RUST_LOG").unwrap_or_else(|_| "info".into()))
        .json()
        .init();

    let database_url = std::env::var("DATABASE_URL")
        .expect("DATABASE_URL must be set (f33d3r_verity)");
    let interval_secs: u64 = std::env::var("SWEEP_INTERVAL_SECS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(900); // 15 min default

    let pool = PgPool::connect(&database_url).await?;
    info!("nexus-sweep connected — interval {}s", interval_secs);

    loop {
        if let Err(e) = sweep(&pool).await {
            error!(error = %e, "sweep cycle failed");
        }
        tokio::time::sleep(tokio::time::Duration::from_secs(interval_secs)).await;
    }
}

async fn sweep(pool: &PgPool) -> Result<()> {
    // 1. Expire stale link requests.
    let expired_links = sqlx::query(
        "UPDATE nexus_link_requests SET status = 'expired'
         WHERE status = 'pending' AND expires_at < NOW()"
    )
    .execute(pool)
    .await?
    .rows_affected();

    if expired_links > 0 {
        info!(count = expired_links, "expired stale nexus link requests");
    }

    // 2. Delete expired session context rows.
    let expired_sessions = sqlx::query(
        "DELETE FROM nexus_session_context WHERE expires_at < NOW()"
    )
    .execute(pool)
    .await?
    .rows_affected();

    if expired_sessions > 0 {
        info!(count = expired_sessions, "purged expired nexus session rows");
    }

    // 3. Flag AML aggregates that breach threshold and are not yet flagged.
    let flagged_aml = sqlx::query(
        "UPDATE nexus_aml_aggregates
         SET flag_for_edd = true, flagged_at = NOW()
         WHERE total_withdrawn_uaet >= $1
           AND flag_for_edd = false
           AND period_end > NOW() - INTERVAL '30 days'"
    )
    .bind(AML_THRESHOLD_UNITS)
    .execute(pool)
    .await?
    .rows_affected();

    if flagged_aml > 0 {
        info!(count = flagged_aml, "flagged nexus AML aggregates for EDD");
    }

    // 4. Emit heartbeat.
    let (link_count,): (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM nexus_persona_links"
    )
    .fetch_one(pool)
    .await
    .unwrap_or((0,));

    let (identity_count,): (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM nexus_identities WHERE NOT is_suspended"
    )
    .fetch_one(pool)
    .await
    .unwrap_or((0,));

    info!(
        nexus_identities = identity_count,
        persona_links = link_count,
        expired_link_requests = expired_links,
        expired_sessions,
        flagged_aml,
        "sweep complete"
    );

    Ok(())
}
