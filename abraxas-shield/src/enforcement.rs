//! Account actions are elohim-veni's. Abraxas decides that a report warrants a
//! ban; elohim-veni carries it out (status, every capability, every session).
//!
//! The decision and the command to ban are written in one transaction
//! (identity_commands). This drain delivers commands in order to
//! `POST {ELOHIM_VENI_URL}/v1/pials/{pial}/ban` with X-Internal-Key and
//! `{reason, by}`, and marks a command only once elohim-veni has accepted it.
//!
//!   2xx                       delivered (a repeat ban is re-asserted, not doubled)
//!   409 pial_tombstoned       delivered: the identity no longer exists, so
//!                             there is nothing left to ban; recorded as such
//!   other 4xx                 quarantined (blocked_at) and reported on every
//!                             tick until a person clears it — a ban that did
//!                             not happen is never silent
//!   5xx / unreachable         retried with backoff, the queue held behind it

use std::time::Duration;

use serde_json::json;
use sqlx::{PgPool, Postgres, Transaction};
use uuid::Uuid;

const LOCK_KEY: i64 = 7331_0022;

pub async fn enqueue_ban(tx: &mut Transaction<'_, Postgres>, pial: Uuid, reason: &str, by: &str, source_ref: &str) -> sqlx::Result<()> {
    sqlx::query(
        "INSERT INTO identity_commands (pial, command, reason, requested_by, source_ref)
         VALUES ($1, 'ban', $2, $3, $4) ON CONFLICT (source_ref) DO NOTHING",
    )
    .bind(pial)
    .bind(reason)
    .bind(by)
    .bind(source_ref)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

pub async fn backlog(pool: &PgPool) -> sqlx::Result<(i64, i64)> {
    sqlx::query_as(
        "SELECT COUNT(*) FILTER (WHERE blocked_at IS NULL), COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)
           FROM identity_commands WHERE delivered_at IS NULL",
    )
    .fetch_one(pool)
    .await
}

pub fn spawn(pool: PgPool, elohim_url: String, key: String) {
    tokio::spawn(async move {
        let mut backoff = Duration::from_secs(1);
        loop {
            if let Err(e) = drain(&pool, &elohim_url, &key).await {
                tracing::error!(error = %e, "identity command drain stopped; restarting");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        }
    });
}

async fn drain(pool: &PgPool, base: &str, key: &str) -> anyhow::Result<()> {
    let http = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(20))
        .build()?;
    let mut lock_conn = pool.acquire().await?.detach();
    loop {
        let held: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
            .bind(LOCK_KEY)
            .fetch_one(&mut lock_conn)
            .await?;
        if held {
            break;
        }
        tokio::time::sleep(Duration::from_secs(5)).await;
    }
    let mut last_report = std::time::Instant::now() - Duration::from_secs(120);
    loop {
        sqlx::query("SELECT 1").execute(&mut lock_conn).await?;
        if last_report.elapsed() >= Duration::from_secs(60) {
            last_report = std::time::Instant::now();
            let (pending, blocked) = backlog(pool).await?;
            metrics::gauge!("abraxas_identity_commands_pending").set(pending as f64);
            metrics::gauge!("abraxas_identity_commands_blocked").set(blocked as f64);
            if blocked > 0 {
                tracing::error!(blocked, "INCIDENT: bans elohim-veni refused — SELECT id, pial, blocked_reason FROM identity_commands WHERE blocked_at IS NOT NULL AND delivered_at IS NULL");
            }
        }
        let next: Option<(i64, Uuid, String, String)> = sqlx::query_as(
            "SELECT id, pial, reason, requested_by FROM identity_commands
              WHERE delivered_at IS NULL AND blocked_at IS NULL AND next_attempt_at <= NOW()
              ORDER BY id LIMIT 1",
        )
        .fetch_optional(pool)
        .await?;
        let Some((id, pial, reason, by)) = next else {
            tokio::time::sleep(Duration::from_secs(1)).await;
            continue;
        };
        let sent = http
            .post(format!("{base}/v1/pials/{pial}/ban"))
            .header("X-Internal-Key", key)
            .json(&json!({ "reason": reason, "by": by }))
            .send()
            .await;
        match sent {
            Ok(r) if r.status().is_success() => {
                sqlx::query("UPDATE identity_commands SET delivered_at = NOW(), attempts = attempts + 1 WHERE id = $1")
                    .bind(id)
                    .execute(pool)
                    .await?;
                tracing::info!(pial = %pial, "elohim-veni enforced the ban");
            }
            Ok(r) => {
                let status = r.status();
                let body: String = r.text().await.unwrap_or_default().chars().take(400).collect();
                if status.as_u16() == 409 && body.contains("pial_tombstoned") {
                    sqlx::query("UPDATE identity_commands SET delivered_at = NOW(), attempts = attempts + 1, last_error = $2 WHERE id = $1")
                        .bind(id)
                        .bind(format!("identity tombstoned; nothing to ban: {body}"))
                        .execute(pool)
                        .await?;
                } else if status.is_client_error() {
                    sqlx::query(
                        "UPDATE identity_commands SET attempts = attempts + 1, last_error = $2,
                                blocked_at = NOW(), blocked_reason = $2 WHERE id = $1",
                    )
                    .bind(id)
                    .bind(format!("status {status}: {body}"))
                    .execute(pool)
                    .await?;
                    tracing::error!(pial = %pial, status = %status, body = %body, "elohim-veni refused the ban; quarantined");
                } else {
                    retry(pool, id, &format!("status {status}: {body}")).await?;
                }
            }
            Err(e) => retry(pool, id, &e.to_string()).await?,
        }
    }
}

async fn retry(pool: &PgPool, id: i64, why: &str) -> sqlx::Result<()> {
    tracing::warn!(command = id, error = %why, "elohim-veni unavailable; ban held and retried");
    sqlx::query(
        "UPDATE identity_commands
            SET attempts = attempts + 1, last_error = $2,
                next_attempt_at = NOW() + LEAST(INTERVAL '5 minutes', INTERVAL '2 seconds' * POWER(2, LEAST(attempts, 8)))
          WHERE id = $1",
    )
    .bind(id)
    .bind(why)
    .execute(pool)
    .await?;
    Ok(())
}
