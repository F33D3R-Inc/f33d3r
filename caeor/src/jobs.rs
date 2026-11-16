//! Video work: the durable job table and the reconciler that drives
//! transcoding.
//!
//! Transcoding is caeor's worker and nobody else's. It keeps only an in-memory
//! view of the jobs it is running; the truth is `upload_jobs` here. The
//! reconciler hands queued jobs to transcoding (PUT /v1/jobs/{id}, idempotent),
//! polls dispatched ones (GET), and finalizes them when transcoding reports a
//! result. If transcoding restarts it answers 404 for a job it lost, and the
//! reconciler hands the same job to it again: the source is in the private
//! store, so nothing is lost. If caeor restarts, the reconciler picks up every
//! open row where it left off.
//!
//! The wire types below are mirrored in transcoding/src/job.rs. Change both or
//! neither.

use std::sync::Arc;
use std::time::Duration;

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use sqlx::PgPool;
use tokio::sync::Notify;
use uuid::Uuid;

use crate::assets::{self, NewDerivative};
use crate::layout::{served_url, Layout, Location, Store};

// ── Wire: caeor → transcoding ────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ClipSpec {
    /// A replay file name (no directories) in transcoding's REPLAY_DIR.
    pub replay_file: String,
    pub start_seconds: u32,
    pub length_seconds: u32,
    pub title: String,
    /// The Frequency's host, credited in the audiogram's words.
    pub host_handle: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MarkSpec {
    /// Already filter-safe (ids::mark_handle). Burned as `@handle • f33d3r.com • short_id`.
    pub handle: String,
    pub short_id: String,
    /// True when the source already carries the moving mark (a legacy
    /// watermarked download being re-laddered). Renditions then inherit the
    /// mark instead of getting a second one drawn over it. The poster's static
    /// mark is drawn either way.
    pub source_marked: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct JobRequest {
    pub job_id: Uuid,
    pub asset_id: Uuid,
    /// The file encoded from. For a clip, transcoding draws it from the
    /// replay first when it does not exist yet.
    pub source: Location,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub clip: Option<ClipSpec>,
    pub mark: MarkSpec,
    /// Directory the marked HLS ladder is written to (master.m3u8 + one
    /// directory per rendition). Replaced whole, atomically, on success.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub hls: Option<Location>,
    /// The poster frame, with a static mark.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub poster: Option<Location>,
    /// The single-file marked MP4 (the download file).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub download: Option<Location>,
}

// ── Wire: transcoding → caeor ────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RenditionOut {
    pub name: String,
    pub width: u32,
    pub height: u32,
    pub bandwidth: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct JobOutput {
    pub source_width: u32,
    pub source_height: u32,
    pub duration_seconds: f64,
    #[serde(default)]
    pub renditions: Vec<RenditionOut>,
    pub hls_written: bool,
    pub poster_written: bool,
    pub download_written: bool,
    #[serde(default)]
    pub download_bytes: Option<u64>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WorkerJob {
    pub job_id: Uuid,
    /// queued | running | ready | failed
    pub state: String,
    #[serde(default)]
    pub error: Option<String>,
    #[serde(default)]
    pub output: Option<JobOutput>,
}

// ── Enqueueing ───────────────────────────────────────────────────────────────

/// Answered retries (transcoding said "failed") before a job fails for good.
const MAX_ATTEMPTS: i32 = 3;
const POLL_EVERY: Duration = Duration::from_secs(3);
const TICK: Duration = Duration::from_secs(2);
/// Session advisory lock: one reconciler per database, however many replicas.
const RECONCILER_LOCK: i64 = 0x_6361_656f_726a_6f62; // "caeorjob"

pub async fn enqueue(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    purpose: &str,
    req: &JobRequest,
) -> sqlx::Result<()> {
    sqlx::query(
        "INSERT INTO upload_jobs (id, asset_id, purpose, state, request)
         VALUES ($1, $2, $3, 'queued', $4)",
    )
    .bind(req.job_id)
    .bind(req.asset_id)
    .bind(purpose)
    .bind(sqlx::types::Json(req))
    .execute(&mut **tx)
    .await?;
    Ok(())
}

// ── The reconciler ───────────────────────────────────────────────────────────

#[derive(Clone)]
pub struct Reconciler {
    pub pool: PgPool,
    pub layout: Layout,
    pub http: reqwest::Client,
    pub transcoding_url: String,
    pub service_key: String,
    pub wake: Arc<Notify>,
}

#[derive(sqlx::FromRow)]
struct OpenJob {
    id: Uuid,
    asset_id: Uuid,
    purpose: String,
    state: String,
    request: sqlx::types::Json<JobRequest>,
    attempts: i32,
}

impl Reconciler {
    pub fn spawn(self) {
        tokio::spawn(async move {
            loop {
                if let Err(e) = self.tick().await {
                    tracing::error!(error = %format!("{e:#}"), "job reconciler tick failed");
                }
                tokio::select! {
                    _ = tokio::time::sleep(TICK) => {}
                    _ = self.wake.notified() => {}
                }
            }
        });
    }

    async fn tick(&self) -> Result<()> {
        let mut lock_conn = self.pool.acquire().await.context("reconciler connection")?;
        let locked: bool = sqlx::query_scalar("SELECT pg_try_advisory_lock($1)")
            .bind(RECONCILER_LOCK)
            .fetch_one(&mut *lock_conn)
            .await
            .context("taking the reconciler lock")?;
        if !locked {
            return Ok(());
        }
        let outcome = self.tick_locked().await;
        let released: bool = sqlx::query_scalar("SELECT pg_advisory_unlock($1)")
            .bind(RECONCILER_LOCK)
            .fetch_one(&mut *lock_conn)
            .await
            .context("releasing the reconciler lock")?;
        if !released {
            tracing::warn!("reconciler lock was not held at release");
        }
        outcome
    }

    async fn tick_locked(&self) -> Result<()> {
        let jobs: Vec<OpenJob> = sqlx::query_as(
            "SELECT id, asset_id, purpose, state, request, attempts FROM upload_jobs
              WHERE state IN ('queued','dispatched','cancelling') AND next_attempt_at <= NOW()
                -- An encode is not started for an asset the integrity authority
                -- has not answered for yet: banned bytes are never laddered.
                -- One under review is encoded, so its release is immediate.
                AND NOT (state = 'queued' AND EXISTS (
                    SELECT 1 FROM assets a
                     WHERE a.id = upload_jobs.asset_id AND a.status = 'held'
                       AND a.integrity_verdict IN ('pending','unreachable')))
              ORDER BY next_attempt_at, created_at
              LIMIT 64",
        )
        .fetch_all(&self.pool)
        .await
        .context("reading open jobs")?;

        for job in jobs {
            let id = job.id;
            let res = match job.state.as_str() {
                "queued" => self.dispatch(&job).await,
                "dispatched" => self.poll(&job).await,
                "cancelling" => self.cancel(&job).await,
                _ => Ok(()),
            };
            if let Err(e) = res {
                // The worker could not be reached or answered nonsense: an
                // outage, not a verdict on the job. Hold it, do not count it.
                let msg = format!("{e:#}");
                tracing::warn!(job_id = %id, error = %msg, "job step failed; will retry");
                sqlx::query(
                    "UPDATE upload_jobs SET last_error = $2,
                            next_attempt_at = NOW() + INTERVAL '10 seconds', updated_at = NOW()
                      WHERE id = $1",
                )
                .bind(id)
                .bind(&msg)
                .execute(&self.pool)
                .await
                .context("recording a job step failure")?;
            }
        }
        Ok(())
    }

    fn url(&self, id: Uuid) -> String {
        format!("{}/v1/jobs/{id}", self.transcoding_url)
    }

    async fn dispatch(&self, job: &OpenJob) -> Result<()> {
        let resp = self
            .http
            .put(self.url(job.id))
            .header("X-Service-Key", &self.service_key)
            .json(&job.request.0)
            .send()
            .await
            .context("sending job to transcoding")?;
        let status = resp.status();
        if status.is_client_error() {
            // Transcoding refused the request itself: it will refuse it again.
            let body = resp.text().await.unwrap_or_else(|e| format!("<unreadable body: {e}>"));
            return self
                .fail(job, &format!("transcoding refused the job ({status}): {body}"))
                .await;
        }
        if !status.is_success() {
            bail!("transcoding answered {status} to dispatch");
        }
        sqlx::query(
            "UPDATE upload_jobs SET state = 'dispatched', dispatched_at = NOW(), last_error = NULL,
                    next_attempt_at = NOW() + $2 * INTERVAL '1 second', updated_at = NOW()
              WHERE id = $1 AND state = 'queued'",
        )
        .bind(job.id)
        .bind(POLL_EVERY.as_secs() as f64)
        .execute(&self.pool)
        .await
        .context("marking job dispatched")?;
        tracing::info!(job_id = %job.id, asset_id = %job.asset_id, purpose = %job.purpose, "job dispatched");
        Ok(())
    }

    async fn fetch(&self, id: Uuid) -> Result<Option<WorkerJob>> {
        let resp = self
            .http
            .get(self.url(id))
            .header("X-Service-Key", &self.service_key)
            .send()
            .await
            .context("polling transcoding")?;
        if resp.status() == reqwest::StatusCode::NOT_FOUND {
            return Ok(None);
        }
        if !resp.status().is_success() {
            bail!("transcoding answered {} to a poll", resp.status());
        }
        Ok(Some(resp.json().await.context("decoding transcoding's job")?))
    }

    async fn forget(&self, id: Uuid) {
        let res = self
            .http
            .delete(self.url(id))
            .header("X-Service-Key", &self.service_key)
            .send()
            .await;
        match res {
            Ok(r) if r.status().is_success() || r.status() == reqwest::StatusCode::NOT_FOUND => {}
            Ok(r) => tracing::warn!(job_id = %id, status = %r.status(),
                "transcoding did not forget a finished job; its retention sweep will"),
            Err(e) => tracing::warn!(job_id = %id, error = %e,
                "could not tell transcoding to forget a finished job; its retention sweep will"),
        }
    }

    async fn poll(&self, job: &OpenJob) -> Result<()> {
        let Some(w) = self.fetch(job.id).await? else {
            // Transcoding restarted and lost it. Hand it over again.
            tracing::warn!(job_id = %job.id, "transcoding has no record of a dispatched job; re-dispatching");
            sqlx::query(
                "UPDATE upload_jobs SET state = 'queued', attempts = attempts + 1,
                        last_error = 'lost by transcoding (restart); re-dispatched',
                        next_attempt_at = NOW(), updated_at = NOW()
                  WHERE id = $1 AND state = 'dispatched'",
            )
            .bind(job.id)
            .execute(&self.pool)
            .await?;
            if job.attempts + 1 >= MAX_ATTEMPTS * 2 {
                return self.fail(job, "transcoding lost this job on every attempt").await;
            }
            return Ok(());
        };
        match w.state.as_str() {
            "queued" | "running" => {
                sqlx::query(
                    "UPDATE upload_jobs SET next_attempt_at = NOW() + $2 * INTERVAL '1 second',
                            updated_at = NOW() WHERE id = $1",
                )
                .bind(job.id)
                .bind(POLL_EVERY.as_secs() as f64)
                .execute(&self.pool)
                .await?;
                Ok(())
            }
            "ready" => {
                let out = w.output.context("transcoding reported ready with no output")?;
                self.finalize(job, &out).await?;
                self.forget(job.id).await;
                Ok(())
            }
            "failed" => {
                let err = w.error.unwrap_or_else(|| "transcoding reported failure".into());
                self.forget(job.id).await;
                if job.attempts + 1 >= MAX_ATTEMPTS {
                    self.fail(job, &err).await
                } else {
                    sqlx::query(
                        "UPDATE upload_jobs SET state = 'queued', attempts = attempts + 1,
                                last_error = $2,
                                next_attempt_at = NOW() + (30 * (attempts + 1)) * INTERVAL '1 second',
                                updated_at = NOW()
                          WHERE id = $1",
                    )
                    .bind(job.id)
                    .bind(&err)
                    .execute(&self.pool)
                    .await?;
                    Ok(())
                }
            }
            other => bail!("transcoding reported unknown job state {other:?}"),
        }
    }

    /// A withdrawn asset's job: wait for transcoding to stop, then remove
    /// whatever it wrote. Withdrawal already removed what existed before.
    async fn cancel(&self, job: &OpenJob) -> Result<()> {
        if let Some(w) = self.fetch(job.id).await? {
            if matches!(w.state.as_str(), "queued" | "running") {
                sqlx::query(
                    "UPDATE upload_jobs SET next_attempt_at = NOW() + $2 * INTERVAL '1 second',
                            updated_at = NOW() WHERE id = $1",
                )
                .bind(job.id)
                .bind(POLL_EVERY.as_secs() as f64)
                .execute(&self.pool)
                .await?;
                return Ok(());
            }
        }
        let req = &job.request.0;
        for loc in [&req.hls, &req.poster, &req.download].into_iter().flatten() {
            crate::withdraw::remove_location(&self.layout, loc).await?;
        }
        self.forget(job.id).await;
        sqlx::query(
            "UPDATE upload_jobs SET state = 'cancelled', finished_at = NOW(), updated_at = NOW()
              WHERE id = $1 AND state = 'cancelling'",
        )
        .bind(job.id)
        .execute(&self.pool)
        .await?;
        tracing::info!(job_id = %job.id, asset_id = %job.asset_id, "cancelled job's output removed");
        Ok(())
    }

    async fn fail(&self, job: &OpenJob, err: &str) -> Result<()> {
        let mut tx = self.pool.begin().await?;
        sqlx::query(
            "UPDATE upload_jobs SET state = 'failed', last_error = $2, finished_at = NOW(),
                    updated_at = NOW()
              WHERE id = $1 AND state IN ('queued','dispatched')",
        )
        .bind(job.id)
        .bind(err)
        .execute(&mut *tx)
        .await?;
        // A failed remark leaves the asset as it was (ready, its old files
        // still recorded as unmarked). A failed first encode fails the asset,
        // held or not: there is nothing to release it to.
        if job.purpose != "remark" {
            sqlx::query(
                "UPDATE assets SET status = 'failed', failure = $2, integrity_next_check_at = NULL,
                        updated_at = NOW()
                  WHERE id = $1 AND status IN ('processing','held')",
            )
            .bind(job.asset_id)
            .bind(err)
            .execute(&mut *tx)
            .await?;
        }
        tx.commit().await?;
        tracing::error!(job_id = %job.id, asset_id = %job.asset_id, error = %err, "job failed");
        Ok(())
    }

    async fn finalize(&self, job: &OpenJob, out: &JobOutput) -> Result<()> {
        let req = &job.request.0;
        let mut tx = self.pool.begin().await?;

        // The asset may have been withdrawn while transcoding ran: then the
        // job is cancelling, and its output is removed, not published.
        let state: String = sqlx::query_scalar("SELECT state FROM upload_jobs WHERE id = $1 FOR UPDATE")
            .bind(job.id)
            .fetch_one(&mut *tx)
            .await?;
        if state != "dispatched" {
            tx.rollback().await?;
            return Ok(());
        }

        let marked = true; // every rendition is marked: drawn here or inherited from a marked source
        if let (Some(hls), true) = (&req.hls, out.hls_written) {
            let base = served_url(&hls.rel);
            let mut master = NewDerivative::served_file(
                "hls_master",
                &Location::served(format!("{}/master.m3u8", hls.rel)),
                marked,
                "application/vnd.apple.mpegurl",
            )
            .dims(out.source_width, out.source_height);
            master.serves_prefix = Some(format!("{base}/"));
            assets::upsert_derivative(&mut tx, req.asset_id, &master).await?;

            let names: Vec<String> = out.renditions.iter().map(|r| r.name.clone()).collect();
            for r in &out.renditions {
                let mut v = NewDerivative::served_file(
                    "hls_variant",
                    &Location::served(format!("{}/{}/index.m3u8", hls.rel, r.name)),
                    marked,
                    "application/vnd.apple.mpegurl",
                )
                .dims(r.width, r.height);
                v.variant = r.name.clone();
                v.bandwidth = Some(r.bandwidth as i32);
                v.serves_prefix = Some(format!("{base}/{}/", r.name));
                assets::upsert_derivative(&mut tx, req.asset_id, &v).await?;
            }
            // Renditions the new ladder does not have were replaced with the
            // directory; their rows go with them.
            sqlx::query(
                "DELETE FROM derivatives WHERE asset_id = $1 AND role = 'hls_variant'
                   AND NOT (variant = ANY($2))",
            )
            .bind(req.asset_id)
            .bind(&names)
            .execute(&mut *tx)
            .await?;
        }
        if let (Some(p), true) = (&req.poster, out.poster_written) {
            let d = NewDerivative::served_file("poster", p, true, "image/jpeg");
            assets::upsert_derivative(&mut tx, req.asset_id, &d).await?;
        }
        if let (Some(dl), true) = (&req.download, out.download_written) {
            let mut d = match dl.store {
                Store::Private => NewDerivative::private_file("download", dl, true, "video/mp4"),
                Store::Served => NewDerivative::served_file("download", dl, true, "video/mp4"),
            };
            d.byte_size = out.download_bytes.map(|b| b as i64);
            // A legacy single-file video is re-marked in place: its served
            // file IS its download, and it is recorded under its own role.
            let role_taken: Option<String> = sqlx::query_scalar(
                "SELECT role FROM derivatives WHERE store = 'served' AND path = $1 AND asset_id = $2",
            )
            .bind(&d.path)
            .bind(req.asset_id)
            .fetch_optional(&mut *tx)
            .await?;
            match role_taken {
                Some(role) if role != "download" => {
                    sqlx::query(
                        "UPDATE derivatives SET watermarked = TRUE, byte_size = $3, updated_at = NOW()
                          WHERE asset_id = $1 AND role = $2 AND variant = ''",
                    )
                    .bind(req.asset_id)
                    .bind(&role)
                    .bind(d.byte_size)
                    .execute(&mut *tx)
                    .await?;
                }
                _ => assets::upsert_derivative(&mut tx, req.asset_id, &d).await?,
            }
        }

        sqlx::query(
            "UPDATE assets SET status = CASE WHEN status = 'processing' THEN 'ready' ELSE status END,
                    width = $2, height = $3, duration_seconds = $4, updated_at = NOW()
              WHERE id = $1",
        )
        .bind(req.asset_id)
        .bind(out.source_width as i32)
        .bind(out.source_height as i32)
        .bind(out.duration_seconds)
        .execute(&mut *tx)
        .await?;
        sqlx::query(
            "UPDATE upload_jobs SET state = 'ready', result = $2, last_error = NULL,
                    finished_at = NOW(), updated_at = NOW()
              WHERE id = $1",
        )
        .bind(job.id)
        .bind(sqlx::types::Json(out))
        .execute(&mut *tx)
        .await?;
        tx.commit().await?;
        tracing::info!(job_id = %job.id, asset_id = %req.asset_id, purpose = %job.purpose, "job finalized");
        Ok(())
    }
}
