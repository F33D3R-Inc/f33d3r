//! The drain. Compiled by `manhattan-outbox` (sqlx 0.8) and
//! `manhattan-outbox-sqlx07` (sqlx 0.7); see lib.rs.
use std::time::Duration;

use anyhow::{Context, Result};
use manhattan_client::{ApplyOp, ApplyResult, Manhattan, ManhattanError, Outcome};
use sqlx::postgres::PgPoolOptions;
use sqlx::PgPool;
use tracing::{error, info, warn};

pub const DRAIN_INTERVAL: Duration = Duration::from_secs(10);
/// Rows per apply. Under Manhattan's ceiling of 500 with room to spare.
pub const DRAIN_BATCH: i64 = 200;
/// Answered retries before a row is quarantined. Twelve, with doubling
/// backoff capped at an hour, is a little over an hour of Manhattan saying
/// "not yet" — long enough for a rolling deploy, and outages do not count.
pub const MAX_ATTEMPTS: i32 = 12;
/// The session advisory lock one draining process holds for a `drain_once`.
/// Every brain's outbox lives in that brain's own database, so one key serves
/// them all.
const DRAIN_LOCK_KEY: i64 = 0x_6d68_6f75_7462_6f78; // "mhoutbox"

struct OutboxRow {
    id: i64,
    op: String,
    payload: serde_json::Value,
    attempts: i32,
}

/// A brain's outbox, ready to drain.
#[derive(Clone)]
pub struct Outbox {
    pool: PgPool,
    brain: String,
}

/// Opens the drain's own connections to the brain's database.
pub async fn connect(brain: &str, database_url: &str) -> Result<Outbox> {
    let pool = PgPoolOptions::new()
        .max_connections(2)
        .acquire_timeout(Duration::from_secs(10))
        .connect(database_url)
        .await
        .with_context(|| format!("{brain}: opening the outbox drain's database pool"))?;
    Ok(Outbox {
        pool,
        brain: brain.to_string(),
    })
}

impl Outbox {
    /// Runs the drain for the life of the process.
    ///
    /// With no `MANHATTAN_URL` this is loud once and then quiet: the outbox
    /// keeps accumulating and will be delivered whole when the naming plane
    /// is configured. Nothing is lost by running without it, but nothing is
    /// resolved either — which is why it says so rather than looking healthy.
    pub fn spawn(self, manhattan: Manhattan) {
        if !manhattan.configured() {
            warn!(
                brain = %self.brain,
                "MANHATTAN_URL is not set: naming-plane registrations are being queued in \
                 manhattan_outbox but not delivered"
            );
            return;
        }
        info!(
            brain = %self.brain,
            interval_secs = DRAIN_INTERVAL.as_secs(),
            batch = DRAIN_BATCH,
            "manhattan outbox drain started"
        );
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(DRAIN_INTERVAL);
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                tick.tick().await;
                match self.drain_once(&manhattan).await {
                    Ok(0) => {}
                    Ok(n) => info!(brain = %self.brain, delivered = n, "manhattan outbox drained"),
                    Err(e) => error!(brain = %self.brain, error = %e, "manhattan outbox drain failed"),
                }
            }
        });
    }

    /// Drains until the queue is short or a row holds the head, and returns
    /// how many rows landed. Under the drain lock: a second replica that finds
    /// it held delivers nothing this tick.
    pub async fn drain_once(&self, manhattan: &Manhattan) -> Result<usize> {
        if !manhattan.configured() {
            return Ok(0);
        }
        // The lock is session-scoped: it lives on this one connection for
        // exactly as long as this call.
        let mut conn = self.pool.acquire().await.context("acquiring the drain connection")?;
        let locked: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
            .bind(DRAIN_LOCK_KEY)
            .fetch_one(&mut *conn)
            .await
            .context("taking the drain lock")?;
        if !locked {
            return Ok(0);
        }

        let mut total = 0usize;
        let outcome = loop {
            match self.drain_batch(manhattan).await {
                Ok((n, full)) => {
                    total += n;
                    if !full {
                        break Ok(total);
                    }
                }
                Err(e) => break Err(e),
            }
        };
        self.report_quarantined().await;

        // Released explicitly. An advisory lock is session-scoped, so the only
        // way this can fail is a dead backend — which has released it already.
        match sqlx::query_scalar::<_, bool>("SELECT pg_advisory_unlock($1)")
            .bind(DRAIN_LOCK_KEY)
            .fetch_one(&mut *conn)
            .await
        {
            Ok(true) => {}
            Ok(false) => warn!(brain = %self.brain, "drain lock was not held at release"),
            Err(e) => error!(brain = %self.brain, error = %e, "releasing the drain lock"),
        }
        outcome
    }

    /// One batch: read, apply, act. `full` says the batch was a whole one and
    /// nothing in it held, so there may be more behind it.
    async fn drain_batch(&self, manhattan: &Manhattan) -> Result<(usize, bool)> {
        let pending: Vec<OutboxRow> = sqlx::query_as::<_, (i64, String, serde_json::Value, i32)>(
            "SELECT id, op, payload, attempts
               FROM manhattan_outbox
              WHERE delivered_at IS NULL
                AND blocked_at IS NULL
                AND next_attempt_at <= NOW()
              ORDER BY id
              LIMIT $1",
        )
        .bind(DRAIN_BATCH)
        .fetch_all(&self.pool)
        .await
        .context("reading outbox")?
        .into_iter()
        .map(|(id, op, payload, attempts)| OutboxRow {
            id,
            op,
            payload,
            attempts,
        })
        .collect();
        if pending.is_empty() {
            return Ok((0, false));
        }

        let ops: Vec<ApplyOp> = pending
            .iter()
            .map(|r| ApplyOp {
                op: r.op.clone(),
                payload: r.payload.clone(),
            })
            .collect();
        let results: Vec<ApplyResult> = match manhattan.apply(&ops).await {
            Ok(r) => r,
            Err(e) => {
                // No verdicts, so nothing is known about any row: the plane
                // could not be reached, predates the endpoint, or rejected the
                // request as a whole. None of that is a fact about the head
                // row, so it is held and the attempt is not counted. If the
                // request landed and only the answer was lost, replaying is
                // safe — every op answers "already" the second time.
                let msg = match e {
                    ManhattanError::NotFound => format!(
                        "applying a batch of {}: this Manhattan predates /v1/apply; the queue waits for one that has it",
                        pending.len()
                    ),
                    e => format!("applying a batch of {}: {e}", pending.len()),
                };
                self.hold(&pending[0], &msg).await;
                return Ok((0, false));
            }
        };

        let mut delivered = 0usize;
        // A retry verdict halts the plane's walk of the batch, so every row
        // after it comes back skipped. When that row is then quarantined here,
        // the skipped rows are free to go and the caller should send them at
        // once rather than wait a tick.
        let mut quarantined_a_hold = false;
        for (row, res) in pending.iter().zip(results) {
            let detail = format!(
                "{} ({}): {}",
                res.code.as_deref().unwrap_or("-"),
                res.outcome,
                res.error.as_deref().unwrap_or("")
            );
            let quarantined = match res.outcome {
                Outcome::Applied | Outcome::Already => {
                    self.mark_delivered(row.id).await?;
                    delivered += 1;
                    continue;
                }
                Outcome::Refused => self.refuse(row, &detail).await,
                Outcome::Retry => self.retry(row, &detail).await,
                Outcome::Error => {
                    self.hold(row, &format!("could not be applied on Manhattan's side: {detail}")).await;
                    false
                }
                // An earlier row held, and this one was left exactly as it was.
                Outcome::Skipped => return Ok((delivered, quarantined_a_hold)),
            };
            // Stop here unless the row has been taken out of the queue.
            if !quarantined {
                return Ok((delivered, false));
            }
            if res.outcome == Outcome::Retry {
                quarantined_a_hold = true;
            }
        }
        Ok((delivered, pending.len() as i64 == DRAIN_BATCH))
    }

    async fn mark_delivered(&self, id: i64) -> Result<()> {
        sqlx::query("UPDATE manhattan_outbox SET delivered_at = NOW(), last_error = NULL WHERE id = $1")
            .bind(id)
            .execute(&self.pool)
            .await
            .with_context(|| format!("marking outbox row {id} delivered"))?;
        Ok(())
    }

    /// The plane did not answer about this row. Record what happened and try
    /// again next tick, but leave `attempts` alone: the count measures how
    /// many times Manhattan has looked at this write and sent it back, and an
    /// outage is not that.
    async fn hold(&self, row: &OutboxRow, msg: &str) {
        warn!(
            brain = %self.brain,
            row = row.id,
            op = %row.op,
            attempts = row.attempts,
            retry_in_secs = DRAIN_INTERVAL.as_secs(),
            error = %msg,
            "manhattan outbox row held: the naming plane did not answer"
        );
        if let Err(e) = sqlx::query(
            "UPDATE manhattan_outbox
                SET last_error      = $2,
                    next_attempt_at = NOW() + make_interval(secs => $3::double precision)
              WHERE id = $1",
        )
        .bind(row.id)
        .bind(msg)
        .bind(DRAIN_INTERVAL.as_secs_f64())
        .execute(&self.pool)
        .await
        {
            error!(brain = %self.brain, row = row.id, error = %e, "recording manhattan outbox hold");
        }
    }

    /// Manhattan answered "not yet". Count it, back off, and quarantine once
    /// it has answered that `MAX_ATTEMPTS` times. Returns whether the row was
    /// quarantined, which is the signal that the batch may continue past it.
    async fn retry(&self, row: &OutboxRow, detail: &str) -> bool {
        let attempt = row.attempts + 1;
        let backoff_secs = (1i64 << row.attempts.clamp(0, 11)).min(3600);
        warn!(
            brain = %self.brain,
            row = row.id,
            op = %row.op,
            attempt,
            retry_in_secs = backoff_secs,
            error = %detail,
            "manhattan outbox row not delivered yet"
        );
        if let Err(e) = sqlx::query(
            "UPDATE manhattan_outbox
                SET attempts        = attempts + 1,
                    last_error      = $2,
                    next_attempt_at = NOW() + make_interval(secs => $3::double precision)
              WHERE id = $1",
        )
        .bind(row.id)
        .bind(detail)
        .bind(backoff_secs as f64)
        .execute(&self.pool)
        .await
        {
            error!(brain = %self.brain, row = row.id, error = %e, "recording manhattan outbox failure");
            // The count did not advance, so a quarantine decision would rest
            // on a number that was never written. Hold the head.
            return false;
        }
        if attempt < MAX_ATTEMPTS {
            return false;
        }
        self.quarantine(row, &format!("Manhattan sent this back {attempt} times: {detail}")).await
    }

    /// Manhattan answered that no retry this queue could schedule will land
    /// this row. Quarantined at once; the queue drains past it.
    async fn refuse(&self, row: &OutboxRow, detail: &str) -> bool {
        if let Err(e) = sqlx::query(
            "UPDATE manhattan_outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1",
        )
        .bind(row.id)
        .bind(detail)
        .execute(&self.pool)
        .await
        {
            error!(brain = %self.brain, row = row.id, error = %e, "recording manhattan outbox refusal");
            return false;
        }
        self.quarantine(row, &format!("Manhattan refused this for good: {detail}")).await
    }

    /// Never dropped, never silent. The row keeps its payload and its error;
    /// it is out of the queue's way and says so on every tick until someone
    /// clears `blocked_at`.
    async fn quarantine(&self, row: &OutboxRow, reason: &str) -> bool {
        match sqlx::query(
            "UPDATE manhattan_outbox SET blocked_at = NOW(), blocked_reason = $2
              WHERE id = $1 AND blocked_at IS NULL",
        )
        .bind(row.id)
        .bind(reason)
        .execute(&self.pool)
        .await
        {
            Ok(_) => {
                error!(
                    brain = %self.brain,
                    row = row.id,
                    op = %row.op,
                    reason = %reason,
                    "INCIDENT: manhattan outbox row quarantined — this naming-plane write did not \
                     happen; the queue now drains past it"
                );
                true
            }
            Err(e) => {
                // Not quarantined, so it keeps the head. The safe answer: the
                // queue stalls loudly rather than running past a row still in it.
                error!(brain = %self.brain, row = row.id, error = %e, "quarantining manhattan outbox row");
                false
            }
        }
    }

    // ── What an operator asks ─────────────────────────────────────────────────

    /// Rows queued and not yet delivered, quarantined ones included. A number
    /// that only grows is a plane that has stopped accepting writes.
    pub async fn backlog(&self) -> Result<i64> {
        sqlx::query_scalar("SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL")
            .fetch_one(&self.pool)
            .await
            .context("counting the manhattan outbox backlog")
    }

    /// How many rows the drain has given up on, and a sample of them.
    pub async fn quarantined(&self) -> Result<(i64, Vec<String>)> {
        let count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM manhattan_outbox WHERE blocked_at IS NOT NULL AND delivered_at IS NULL",
        )
        .fetch_one(&self.pool)
        .await
        .context("counting quarantined manhattan outbox rows")?;
        if count == 0 {
            return Ok((0, Vec::new()));
        }
        let rows: Vec<(i64, String, Option<String>)> = sqlx::query_as(
            "SELECT id, op, blocked_reason FROM manhattan_outbox
              WHERE blocked_at IS NOT NULL AND delivered_at IS NULL
              ORDER BY id LIMIT 5",
        )
        .fetch_all(&self.pool)
        .await
        .context("reading quarantined manhattan outbox rows")?;
        Ok((
            count,
            rows.into_iter()
                .map(|(id, op, reason)| {
                    format!("{id} ({op}): {}", reason.unwrap_or_else(|| "no reason recorded".into()))
                })
                .collect(),
        ))
    }

    /// Says what is quarantined, on every tick where anything is.
    async fn report_quarantined(&self) {
        match self.quarantined().await {
            Ok((0, _)) => {}
            Ok((count, sample)) => error!(
                brain = %self.brain,
                quarantined = count,
                sample = %sample.join(" | "),
                "INCIDENT: manhattan outbox rows are quarantined — these naming-plane writes did not \
                 happen and will not until an operator clears blocked_at"
            ),
            Err(e) => error!(brain = %self.brain, error = %e, "reading quarantined manhattan outbox rows"),
        }
    }
}
