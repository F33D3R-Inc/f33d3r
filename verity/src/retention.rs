//! Retention, run inside verity on an interval.
//!
//! The rules are the shared KYC contract's:
//!
//! * **Failed attempts** — the PII (document facts, consent IP and user agent)
//!   is purged 30 days after the attempt. The row stays with its decision,
//!   reasons, evidence types and scores, and so does its audit trail.
//! * **Passing age-only attempts** — nothing to purge: legal name and DOB are
//!   the age record, and no image was ever stored.
//! * **Creator ID copies** — kept while any content recorded against the
//!   submission is live, and for seven years after the last of it is removed
//!   (18 U.S.C. 2257). A copy that never had content recorded against it runs
//!   its seven years from when it was stored. Account deletion alone never
//!   purges a copy: only this clock does.
//!
//! Every purge writes a `purge` audit row in the same transaction as the purge.

use std::time::Duration;

use serde_json::json;
use sqlx::PgPool;
use tracing::{error, info, warn};
use uuid::Uuid;

use crate::db::action;

const DEFAULT_INTERVAL: Duration = Duration::from_secs(3600);

/// Copies examined per cycle. A cycle that finds more simply leaves the rest
/// for the next one.
const ID_COPY_BATCH: i64 = 500;

/// The interval from `VERITY_RETENTION_INTERVAL_SECS`, defaulting to an hour.
/// A value that does not parse is reported and the default used.
pub fn interval_from_env() -> Duration {
    match std::env::var("VERITY_RETENTION_INTERVAL_SECS") {
        Ok(raw) => match raw.trim().parse::<u64>() {
            Ok(secs) if secs > 0 => Duration::from_secs(secs),
            _ => {
                warn!(
                    value = %raw,
                    default_secs = DEFAULT_INTERVAL.as_secs(),
                    "VERITY_RETENTION_INTERVAL_SECS is not a positive integer; using the default"
                );
                DEFAULT_INTERVAL
            }
        },
        Err(_) => DEFAULT_INTERVAL,
    }
}

pub fn spawn(pool: PgPool, interval: Duration) {
    info!(interval_secs = interval.as_secs(), "retention job started");
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(interval);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            ticker.tick().await;
            match purge_failed_attempts(&pool).await {
                Ok(n) if n > 0 => info!(count = n, "purged PII from failed attempts"),
                Ok(_) => {}
                Err(e) => error!(error = %e, "retention: failed-attempt purge failed"),
            }
            match purge_expired_id_copies(&pool).await {
                Ok(n) if n > 0 => info!(count = n, "purged expired 2257 ID copies"),
                Ok(_) => {}
                Err(e) => error!(error = %e, "retention: ID copy purge failed"),
            }
        }
    });
}

/// Clears a failed attempt's PII after 30 days and audits each row, as one
/// statement — the purge and its audit rows cannot separate.
async fn purge_failed_attempts(pool: &PgPool) -> Result<u64, sqlx::Error> {
    let done = sqlx::query(
        "WITH purged AS (
             UPDATE kyc_submissions
                SET doc_hmac = NULL, doc_type = NULL, issuing_country = NULL,
                    issuing_region = NULL, doc_expiry = NULL,
                    consent_ip = NULL, consent_user_agent = NULL,
                    pii_purged_at = NOW()
              WHERE decision = 'fail'
                AND pii_purged_at IS NULL
                AND created_at < NOW() - INTERVAL '30 days'
          RETURNING id, attempt_id, pial_id
         )
         INSERT INTO verity_audit (pial_id, action, attempt_id, submission_id, metadata)
         SELECT pial_id, $1, attempt_id, id,
                jsonb_build_object('what', 'failed_attempt_pii', 'rule', 'fail_30_days')
           FROM purged",
    )
    .bind(action::PURGE)
    .execute(pool)
    .await?;
    Ok(done.rows_affected())
}

/// The condition under which a copy may go, evaluated against the database as
/// it stands: no live content on its submission, and seven years since the
/// later of the copy being stored and the last removal.
const COPY_EXPIRED: &str = "NOT EXISTS (SELECT 1 FROM records_2257 r
                                         WHERE r.submission_id = c.submission_id
                                           AND r.removed_at IS NULL)
                            AND GREATEST(c.created_at,
                                         COALESCE((SELECT MAX(r.removed_at) FROM records_2257 r
                                                    WHERE r.submission_id = c.submission_id),
                                                  c.created_at))
                                < NOW() - INTERVAL '7 years'";

/// Deletes each expired ID copy in its own transaction. The copy row is locked
/// first and the condition re-checked under the lock: a 2257 record being
/// written holds a share lock on the copy, so a copy cannot be purged out from
/// under content that is being recorded against it at that moment.
async fn purge_expired_id_copies(pool: &PgPool) -> Result<u64, sqlx::Error> {
    let candidates: Vec<(Uuid,)> = sqlx::query_as(&format!(
        "SELECT c.submission_id FROM kyc_id_copies c WHERE {COPY_EXPIRED} LIMIT $1"
    ))
    .bind(ID_COPY_BATCH)
    .fetch_all(pool)
    .await?;

    let mut purged = 0u64;
    for (submission_id,) in candidates {
        let mut tx = pool.begin().await?;

        let locked: Option<(Uuid,)> = sqlx::query_as(
            "SELECT pial_id FROM kyc_id_copies WHERE submission_id = $1 FOR UPDATE",
        )
        .bind(submission_id)
        .fetch_optional(&mut *tx)
        .await?;
        if locked.is_none() {
            continue;
        }

        let deleted: Option<(Uuid, String)> = sqlx::query_as(&format!(
            "DELETE FROM kyc_id_copies c
              WHERE c.submission_id = $1 AND {COPY_EXPIRED}
          RETURNING c.pial_id, c.key_id"
        ))
        .bind(submission_id)
        .fetch_optional(&mut *tx)
        .await?;

        let Some((pial_id, key_id)) = deleted else {
            // Content was recorded against it while this cycle ran; it stays.
            continue;
        };

        let attempt_id: Uuid =
            sqlx::query_scalar("SELECT attempt_id FROM kyc_submissions WHERE id = $1")
                .bind(submission_id)
                .fetch_one(&mut *tx)
                .await?;

        crate::db::audit(
            &mut *tx,
            pial_id,
            action::PURGE,
            Some(attempt_id),
            Some(submission_id),
            json!({ "what": "id_copy", "rule": "2257_7_years_after_last_removal", "key_id": key_id }),
        )
        .await?;

        tx.commit().await?;
        purged += 1;
    }
    Ok(purged)
}
