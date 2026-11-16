//! Transcoding is caeor's worker. Caeor is its only caller and keeps the
//! durable record of every job (caeor's upload_jobs); this service keeps an
//! in-memory view of what it is running, nothing more. If it restarts, caeor
//! finds a job it dispatched missing (404) and dispatches it again; the source
//! is in the private store, so nothing is lost.
//!
//!   PUT    /v1/jobs/{job_id}  start a job (idempotent: an existing live or
//!                             finished job is returned as it is; a failed one
//!                             is started again)
//!   GET    /v1/jobs/{job_id}  the job's state, and its output when ready
//!   DELETE /v1/jobs/{job_id}  caeor has recorded the outcome; forget it

use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Json;
use chrono::{DateTime, Utc};
use dashmap::mapref::entry::Entry;
use dashmap::DashMap;
use serde::Serialize;
use tokio::sync::Semaphore;
use tracing::{error, info};
use uuid::Uuid;

use crate::job::{JobOutput, JobRequest, Roots};
use crate::transcode;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum JobState {
    Queued,
    Running,
    Ready,
    Failed,
}

#[derive(Debug, Clone, Serialize)]
pub struct Job {
    pub job_id: Uuid,
    pub asset_id: Uuid,
    pub state: JobState,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub error: Option<String>,
    pub output: Option<JobOutput>,
}

/// How long a finished job is kept if caeor never says it has recorded it.
const JOB_RETENTION_HOURS: i64 = 24;

#[derive(Clone)]
pub struct AppState {
    pub roots: Arc<Roots>,
    pub jobs: Arc<DashMap<Uuid, Job>>,
    /// Encodes running at once. Each is real CPU (or NVENC session) work on a
    /// shared box; the rest wait their turn here, durably queued in caeor.
    pub slots: Arc<Semaphore>,
}

pub async fn health() -> Json<serde_json::Value> {
    Json(serde_json::json!({ "status": "ok", "brain": "transcoding", "version": "2.0" }))
}

/// Only caeor may start work here or read a job's output.
pub async fn require_service_key(req: axum::extract::Request, next: axum::middleware::Next) -> Response {
    let path = req.uri().path();
    if path == "/health" || path == "/metrics" {
        return next.run(req).await;
    }
    let key = std::env::var("INTERNAL_API_KEY").unwrap_or_default();
    let presented = req.headers().get("X-Service-Key").and_then(|v| v.to_str().ok()).unwrap_or("");
    let same = presented.len() == key.len()
        && presented.bytes().zip(key.bytes()).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0;
    if key.is_empty() || !same {
        return (StatusCode::UNAUTHORIZED, "service key required").into_response();
    }
    next.run(req).await
}

/// Forgets finished jobs caeor never acknowledged, so the map cannot grow for
/// the life of the process.
pub fn evict_finished_jobs(jobs: &DashMap<Uuid, Job>) {
    let cutoff = Utc::now() - chrono::Duration::hours(JOB_RETENTION_HOURS);
    jobs.retain(|_, j| j.updated_at > cutoff || !matches!(j.state, JobState::Ready | JobState::Failed));
}

pub async fn put_job(
    State(st): State<AppState>,
    Path(job_id): Path<Uuid>,
    Json(req): Json<JobRequest>,
) -> Response {
    if req.job_id != job_id {
        return (StatusCode::BAD_REQUEST, "job_id in the path and the body differ").into_response();
    }
    if let Err(e) = req.validate() {
        return (StatusCode::UNPROCESSABLE_ENTITY, format!("{e:#}")).into_response();
    }
    let now = Utc::now();
    let fresh = Job {
        job_id,
        asset_id: req.asset_id,
        state: JobState::Queued,
        created_at: now,
        updated_at: now,
        error: None,
        output: None,
    };
    // Atomic per key: two PUTs of one job cannot both start it.
    let start = match st.jobs.entry(job_id) {
        Entry::Vacant(v) => {
            v.insert(fresh);
            true
        }
        Entry::Occupied(mut o) if o.get().state == JobState::Failed => {
            o.insert(fresh);
            true
        }
        Entry::Occupied(_) => false,
    };
    if start {
        let st2 = st.clone();
        tokio::spawn(async move { run_job(st2, req).await });
    }
    match st.jobs.get(&job_id) {
        Some(j) => (StatusCode::ACCEPTED, Json(j.clone())).into_response(),
        None => (StatusCode::INTERNAL_SERVER_ERROR, "job vanished").into_response(),
    }
}

pub async fn get_job(State(st): State<AppState>, Path(job_id): Path<Uuid>) -> Response {
    match st.jobs.get(&job_id) {
        Some(j) => Json(j.clone()).into_response(),
        None => (StatusCode::NOT_FOUND, "job not found").into_response(),
    }
}

pub async fn delete_job(State(st): State<AppState>, Path(job_id): Path<Uuid>) -> Response {
    let finished = st
        .jobs
        .remove_if(&job_id, |_, j| matches!(j.state, JobState::Ready | JobState::Failed));
    match finished {
        Some(_) => StatusCode::NO_CONTENT.into_response(),
        None if st.jobs.contains_key(&job_id) => {
            (StatusCode::CONFLICT, "job is still running").into_response()
        }
        None => (StatusCode::NOT_FOUND, "job not found").into_response(),
    }
}

fn set(st: &AppState, id: Uuid, f: impl FnOnce(&mut Job)) {
    if let Some(mut j) = st.jobs.get_mut(&id) {
        f(&mut j);
        j.updated_at = Utc::now();
    }
}

async fn run_job(st: AppState, req: JobRequest) {
    let id = req.job_id;
    let slot = match st.slots.clone().acquire_owned().await {
        Ok(s) => s,
        Err(e) => {
            set(&st, id, |j| {
                j.state = JobState::Failed;
                j.error = Some(format!("job slots closed: {e}"));
            });
            return;
        }
    };
    set(&st, id, |j| j.state = JobState::Running);
    info!(job_id = %id, asset_id = %req.asset_id, "job start");
    let result = transcode::run(&st.roots, &req).await;
    drop(slot);
    match result {
        Ok(out) => {
            info!(job_id = %id, asset_id = %req.asset_id, "job ready");
            set(&st, id, |j| {
                j.state = JobState::Ready;
                j.output = Some(out);
            });
        }
        Err(e) => {
            let msg = format!("{e:#}");
            error!(job_id = %id, asset_id = %req.asset_id, error = %msg, "job failed");
            set(&st, id, |j| {
                j.state = JobState::Failed;
                j.error = Some(msg);
            });
        }
    }
}
