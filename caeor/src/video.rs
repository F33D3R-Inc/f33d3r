//! Video upload + transcode endpoint. Accepts a multipart upload, persists
//! the source to MinIO `media-raw`, runs the HLS ladder transcode in the
//! background, and writes the output to `media-derived`.
//!
//! Returns immediately with a job_id. Status polled via GET /v1/video/job/:id.
//!
//! For 4K source on a CCX23 (no GPU) the full ladder takes ~real-time × 4–6.
//! Acceptable at T0 — moves to GPU node pool at T2 (D-015 trigger).

use std::path::PathBuf;
use std::sync::Arc;

use axum::{
    extract::{Multipart, Path, State},
    http::StatusCode,
    Json,
};
use chrono::{DateTime, Utc};
use dashmap::DashMap;
use once_cell::sync::Lazy;
use serde::Serialize;
use tokio::io::AsyncWriteExt;
use tracing::{error, info, warn};
use uuid::Uuid;

use crate::storage::Storage;
use crate::transcode::{self, TranscodeOutput};

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum JobStatus {
    Queued,
    Probing,
    Transcoding,
    Uploading,
    Ready,
    Failed,
}

#[derive(Debug, Clone, Serialize)]
pub struct Job {
    pub job_id: String,
    pub asset_id: String,
    pub uploader_pial: Option<String>,
    pub status: JobStatus,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub error: Option<String>,
    pub output: Option<TranscodeOutput>,
}

/// In-memory job registry. Replaces with Postgres-backed jobs in S0.5.
/// In-memory is fine at T0 because Caeor restarts are rare, and on restart
/// any in-flight job is presumed failed; client retries.
static JOBS: Lazy<DashMap<String, Job>> = Lazy::new(DashMap::new);

const MAX_UPLOAD_BYTES: u64 = 10 * 1024 * 1024 * 1024; // 10 GB; CCX23 disk is 160 GB
const ALLOWED_EXT: &[&str] = &["mp4", "mov", "webm", "mkv", "m4v"];

#[derive(Clone)]
pub struct VideoState {
    pub storage: Arc<Storage>,
}

#[derive(Debug, Serialize)]
pub struct UploadResponse {
    pub job_id: String,
    pub asset_id: String,
}

/// POST /v1/video/upload — multipart with field "file"
pub async fn upload(
    State(state): State<VideoState>,
    headers: axum::http::HeaderMap,
    mut multipart: Multipart,
) -> Result<Json<UploadResponse>, (StatusCode, String)> {
    let pial = headers
        .get("x-pial-identity")
        .and_then(|v| v.to_str().ok())
        .map(String::from);

    let mut tmp_path: Option<PathBuf> = None;
    let mut filename: Option<String> = None;

    while let Some(mut field) = multipart
        .next_field()
        .await
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("multipart: {e}")))?
    {
        let name = field.name().unwrap_or("").to_string();
        if name != "file" {
            continue;
        }

        let original = field
            .file_name()
            .ok_or((StatusCode::BAD_REQUEST, "missing filename".into()))?
            .to_string();

        let ext = original
            .rsplit('.')
            .next()
            .map(|s| s.to_lowercase())
            .unwrap_or_default();
        if !ALLOWED_EXT.contains(&ext.as_str()) {
            return Err((
                StatusCode::UNSUPPORTED_MEDIA_TYPE,
                format!("unsupported extension: {ext}"),
            ));
        }
        filename = Some(original);

        let tf = tempfile::Builder::new()
            .prefix("caeor-vsrc-")
            .suffix(&format!(".{ext}"))
            .tempfile()
            .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("tempfile: {e}")))?;
        let (file, path) = tf.keep().map_err(|e| {
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("tempfile keep: {e}"),
            )
        })?;

        let mut writer = tokio::io::BufWriter::new(tokio::fs::File::from_std(file));
        let mut total: u64 = 0;
        while let Some(chunk) = field
            .chunk()
            .await
            .map_err(|e| (StatusCode::BAD_REQUEST, format!("chunk: {e}")))?
        {
            total += chunk.len() as u64;
            if total > MAX_UPLOAD_BYTES {
                return Err((StatusCode::PAYLOAD_TOO_LARGE, "file too large".into()));
            }
            writer.write_all(&chunk)
                .await
                .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("write: {e}")))?;
        }
        writer.flush()
            .await
            .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("flush: {e}")))?;
        tmp_path = Some(path);
        break;
    }

    let source_path = tmp_path.ok_or((StatusCode::BAD_REQUEST, "no file field".into()))?;
    let original_name = filename.unwrap_or_else(|| "video".into());

    let asset_id = Uuid::new_v4().to_string();
    let job_id = Uuid::new_v4().to_string();

    // Persist source to media-raw (best-effort, async).
    let storage = state.storage.clone();
    let raw_key = format!("posts/{asset_id}/source-{original_name}");
    let now = Utc::now();

    JOBS.insert(
        job_id.clone(),
        Job {
            job_id: job_id.clone(),
            asset_id: asset_id.clone(),
            uploader_pial: pial.clone(),
            status: JobStatus::Queued,
            created_at: now,
            updated_at: now,
            error: None,
            output: None,
        },
    );

    let job_id_clone = job_id.clone();
    let asset_id_clone = asset_id.clone();
    tokio::spawn(async move {
        run_pipeline(storage, source_path, raw_key, job_id_clone, asset_id_clone).await;
    });

    Ok(Json(UploadResponse { job_id, asset_id }))
}

/// GET /v1/video/job/:job_id
pub async fn job_status(
    Path(job_id): Path<String>,
) -> Result<Json<Job>, (StatusCode, String)> {
    JOBS.get(&job_id)
        .map(|j| Json(j.clone()))
        .ok_or((StatusCode::NOT_FOUND, "job not found".into()))
}

fn set_status(job_id: &str, status: JobStatus) {
    if let Some(mut j) = JOBS.get_mut(job_id) {
        j.status = status;
        j.updated_at = Utc::now();
    }
}

fn set_failed(job_id: &str, msg: String) {
    if let Some(mut j) = JOBS.get_mut(job_id) {
        j.status = JobStatus::Failed;
        j.error = Some(msg);
        j.updated_at = Utc::now();
    }
}

fn set_output(job_id: &str, out: TranscodeOutput) {
    if let Some(mut j) = JOBS.get_mut(job_id) {
        j.output = Some(out);
        j.status = JobStatus::Ready;
        j.updated_at = Utc::now();
    }
}

async fn run_pipeline(
    storage: Arc<Storage>,
    source_path: PathBuf,
    raw_key: String,
    job_id: String,
    asset_id: String,
) {
    set_status(&job_id, JobStatus::Probing);
    info!(%job_id, %asset_id, "video pipeline start");

    // Best-effort source archive to media-raw — non-fatal.
    if let Ok(bytes) = tokio::fs::read(&source_path).await {
        let bucket = storage.bucket_raw.clone();
        if let Err(e) = storage
            .put(&bucket, &raw_key, bytes::Bytes::from(bytes), "video/mp4")
            .await
        {
            warn!(%job_id, ?e, "source archive to media-raw failed (non-fatal)");
        }
    }

    set_status(&job_id, JobStatus::Transcoding);
    let result = transcode::run(&storage, &asset_id, &source_path).await;

    // Always cleanup tmp source
    let _ = tokio::fs::remove_file(&source_path).await;

    match result {
        Ok(out) => {
            set_output(&job_id, out);
            info!(%job_id, %asset_id, "video pipeline ok");
        }
        Err(e) => {
            error!(%job_id, %asset_id, error=%e, "video pipeline failed");
            set_failed(&job_id, format!("{e:#}"));
        }
    }
}
