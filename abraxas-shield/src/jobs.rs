//! Durable jobs: the work that must happen even when the worker, the database
//! or this process is down at the moment it is first attempted.
//!
//!   asset_recheck  a /v1/check the worker could not answer. The asset stays
//!                  at verdict `pending` until this completes; it is never
//!                  allowed by default.
//!   subject_scan   a work or vision accepted from content.events.
//!   ml_review      the worker's deep pass over one review item.
//!
//! A job is claimed by moving its next_attempt_at forward by a lease, so a
//! process that dies mid-job releases it when the lease runs out. A job that
//! keeps failing is retried with exponential backoff up to MAX_ATTEMPTS
//! (about a day of trying) and then ESCALATED: it becomes an admin_review
//! item a person must decide. Nothing waits forever and nothing is dropped.

use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;
use sqlx::{PgPool, Postgres, Transaction};

use crate::api::AppState;

const LEASE: &str = "30 minutes";
const MAX_ATTEMPTS: i32 = 40;
const IDLE: Duration = Duration::from_secs(2);

#[derive(Debug)]
pub enum JobError {
    /// Try again later: the worker or a dependency is not answering yet.
    Retry(String),
    /// No retry can succeed. A person decides.
    Escalate(String),
}

impl From<sqlx::Error> for JobError {
    fn from(e: sqlx::Error) -> Self {
        JobError::Retry(format!("database: {e}"))
    }
}

/// Queues a job inside the caller's transaction. A job already pending for
/// the same (kind, ref) is pulled forward instead of duplicated.
pub async fn enqueue(tx: &mut Transaction<'_, Postgres>, kind: &str, r#ref: &str, payload: Value) -> sqlx::Result<()> {
    sqlx::query(
        "INSERT INTO jobs (kind, ref, payload) VALUES ($1, $2, $3)
         ON CONFLICT (kind, ref) WHERE state = 'pending'
         DO UPDATE SET next_attempt_at = LEAST(jobs.next_attempt_at, NOW()), payload = EXCLUDED.payload",
    )
    .bind(kind)
    .bind(r#ref)
    .bind(payload)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

pub async fn backlog(pool: &PgPool) -> sqlx::Result<Vec<(String, i64)>> {
    sqlx::query_as("SELECT kind, COUNT(*) FROM jobs WHERE state = 'pending' GROUP BY kind ORDER BY kind")
        .fetch_all(pool)
        .await
}

fn backoff(attempts: i32) -> String {
    let secs = (30u64 << (attempts.clamp(1, 10) - 1) as u32).min(3600);
    format!("{secs} seconds")
}

pub fn spawn(state: AppState) {
    for worker in 0..state.cfg.job_concurrency {
        let st = state.clone();
        tokio::spawn(async move {
            loop {
                match run_one(&st).await {
                    Ok(true) => {}
                    Ok(false) => {
                        tokio::select! {
                            _ = st.jobs_wake.notified() => {}
                            _ = tokio::time::sleep(IDLE) => {}
                        }
                    }
                    Err(e) => {
                        tracing::error!(worker, error = %e, "job runner: database error; backing off");
                        tokio::time::sleep(Duration::from_secs(5)).await;
                    }
                }
            }
        });
    }
}

/// Claims and runs one due job. Ok(false) when nothing is due.
async fn run_one(st: &AppState) -> sqlx::Result<bool> {
    let claimed: Option<(i64, String, String, Value, i32)> = sqlx::query_as(&format!(
        "UPDATE jobs SET attempts = attempts + 1, next_attempt_at = NOW() + INTERVAL '{LEASE}'
          WHERE id = (SELECT id FROM jobs
                       WHERE state = 'pending' AND next_attempt_at <= NOW()
                       ORDER BY next_attempt_at, id
                       FOR UPDATE SKIP LOCKED LIMIT 1)
          RETURNING id, kind, ref, payload, attempts"
    ))
    .fetch_optional(&st.pool)
    .await?;
    let Some((id, kind, r#ref, payload, attempts)) = claimed else {
        return Ok(false);
    };

    let outcome = match kind.as_str() {
        "asset_recheck" => crate::check::run_recheck(st, &r#ref, &payload).await,
        "subject_scan" => crate::scan::run_subject_scan(st, &r#ref).await,
        "ml_review" => crate::review::run_ml_review(st, &r#ref).await,
        other => Err(JobError::Escalate(format!("unknown job kind {other:?}"))),
    };

    match outcome {
        Ok(()) => {
            sqlx::query("UPDATE jobs SET state = 'done', finished_at = NOW(), last_error = NULL WHERE id = $1")
                .bind(id)
                .execute(&st.pool)
                .await?;
            metrics::counter!("abraxas_jobs_total", "kind" => kind, "outcome" => "done").increment(1);
        }
        Err(JobError::Retry(why)) if attempts < MAX_ATTEMPTS => {
            tracing::warn!(job = id, kind = %kind, r#ref = %r#ref, attempts, error = %why, "job will be retried");
            sqlx::query(&format!(
                "UPDATE jobs SET last_error = $2, next_attempt_at = NOW() + INTERVAL '{}' WHERE id = $1",
                backoff(attempts)
            ))
            .bind(id)
            .bind(&why)
            .execute(&st.pool)
            .await?;
            metrics::counter!("abraxas_jobs_total", "kind" => kind, "outcome" => "retry").increment(1);
        }
        Err(JobError::Retry(why)) | Err(JobError::Escalate(why)) => {
            tracing::error!(job = id, kind = %kind, r#ref = %r#ref, attempts, error = %why,
                "job cannot complete; escalating to admin review");
            if let Err(e) = escalate(st, id, &kind, &r#ref, &why).await {
                // The escalation itself did not land: keep the job pending so
                // the next claim tries again. Never mark it finished.
                tracing::error!(job = id, error = %e, "escalation failed; job stays pending");
                sqlx::query("UPDATE jobs SET last_error = $2, next_attempt_at = NOW() + INTERVAL '5 minutes' WHERE id = $1")
                    .bind(id)
                    .bind(format!("escalation failed: {e}; cause: {why}"))
                    .execute(&st.pool)
                    .await?;
            } else {
                metrics::counter!("abraxas_jobs_total", "kind" => kind, "outcome" => "escalated").increment(1);
            }
        }
    }
    Ok(true)
}

/// Turns a job no machine can finish into an admin_review item, atomically
/// with closing the job.
async fn escalate(st: &AppState, id: i64, kind: &str, r#ref: &str, why: &str) -> sqlx::Result<()> {
    let mut tx = st.pool.begin().await?;
    match kind {
        "asset_recheck" => crate::review::open_unjudged(&mut tx, "asset", r#ref, why).await?,
        "subject_scan" => {
            let (sk, sid) = r#ref.split_once(':').unwrap_or(("work", r#ref));
            crate::review::open_unjudged(&mut tx, sk, sid, why).await?
        }
        "ml_review" => crate::review::promote_to_admin(&mut tx, r#ref, why).await?,
        _ => {}
    }
    sqlx::query("UPDATE jobs SET state = 'escalated', finished_at = NOW(), last_error = $2 WHERE id = $1")
        .bind(id)
        .bind(why)
        .execute(&mut *tx)
        .await?;
    tx.commit().await
}

/// Wakes the runners after a commit that queued work.
pub fn wake(notify: &Arc<tokio::sync::Notify>) {
    notify.notify_waiters();
}
