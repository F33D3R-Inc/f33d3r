//! Video upload + async transcode endpoint.
//!
//! POST /v1/video/upload  — accepts multipart "file" field, returns {job_id, asset_id}
//! GET  /v1/video/job/:id — poll job status; output contains HLS URLs when ready

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
use tracing::{error, info};
use uuid::Uuid;

use crate::transcode::{self, TranscodeOutput};

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum JobStatus {
    Queued,
    Transcoding,
    Ready,
    Failed,
}

#[derive(Debug, Clone, Serialize)]
pub struct Job {
    pub job_id: String,
    pub asset_id: String,
    pub status: JobStatus,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub error: Option<String>,
    pub output: Option<TranscodeOutput>,
}

/// In-memory job store. On restart, in-flight jobs are presumed failed; clients retry.
static JOBS: Lazy<DashMap<String, Job>> = Lazy::new(DashMap::new);

const MAX_UPLOAD_BYTES: u64 = 10 * 1024 * 1024 * 1024;
const ALLOWED_EXT: &[&str] = &["mp4", "mov", "webm", "mkv", "m4v"];

#[derive(Clone)]
pub struct AppState {
    pub media_dir: Arc<String>,
}

#[derive(Debug, Serialize)]
pub struct UploadResponse {
    pub job_id: String,
    pub asset_id: String,
}

pub async fn health() -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "status": "ok",
        "brain": "transcoding",
        "version": "1.0"
    }))
}

/// POST /v1/video/upload
///
/// Multipart fields:
///   file            — required; the video file
///   creator_handle  — optional; burned into every frame as attribution chit
///   creator_pial    — optional; stored for logging / future use
pub async fn upload(
    State(state): State<AppState>,
    mut multipart: Multipart,
) -> Result<Json<UploadResponse>, (StatusCode, String)> {
    let mut tmp_path: Option<PathBuf> = None;
    let mut creator_handle = String::new();

    while let Some(mut field) = multipart
        .next_field()
        .await
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("multipart: {e}")))?
    {
        match field.name().unwrap_or("") {
            "creator_handle" => {
                creator_handle = field
                    .text()
                    .await
                    .map_err(|e| (StatusCode::BAD_REQUEST, format!("creator_handle: {e}")))?;
            }
            "creator_pial" => {
                // Accepted for logging / future use; not used in transcode path.
                let _ = field
                    .text()
                    .await
                    .map_err(|e| (StatusCode::BAD_REQUEST, format!("creator_pial: {e}")))?;
            }
            "file" => {
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

                let tf = tempfile::Builder::new()
                    .prefix("transcode-vsrc-")
                    .suffix(&format!(".{ext}"))
                    .tempfile()
                    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("tempfile: {e}")))?;
                let (file, path) = tf.keep().map_err(|e| {
                    (StatusCode::INTERNAL_SERVER_ERROR, format!("tempfile keep: {e}"))
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
                        return Err((
                            StatusCode::PAYLOAD_TOO_LARGE,
                            "file too large (10 GB max)".into(),
                        ));
                    }
                    writer
                        .write_all(&chunk)
                        .await
                        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("write: {e}")))?;
                }
                writer
                    .flush()
                    .await
                    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("flush: {e}")))?;
                tmp_path = Some(path);
            }
            _ => {
                // Unknown field — drain and ignore.
            }
        }
    }

    let source_path = tmp_path.ok_or((StatusCode::BAD_REQUEST, "no file field".into()))?;
    let asset_id = Uuid::new_v4().to_string();
    let job_id = Uuid::new_v4().to_string();
    let now = Utc::now();

    JOBS.insert(
        job_id.clone(),
        Job {
            job_id: job_id.clone(),
            asset_id: asset_id.clone(),
            status: JobStatus::Queued,
            created_at: now,
            updated_at: now,
            error: None,
            output: None,
        },
    );

    let jid = job_id.clone();
    let aid = asset_id.clone();
    let media_dir = (*state.media_dir).clone();
    tokio::spawn(async move {
        run_pipeline(media_dir, source_path, jid, aid, creator_handle).await;
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
    media_dir: String,
    source_path: PathBuf,
    job_id: String,
    asset_id: String,
    creator_handle: String,
) {
    set_status(&job_id, JobStatus::Transcoding);
    info!(%job_id, %asset_id, creator_handle=%creator_handle, "transcode pipeline start");

    let result = transcode::run(&media_dir, &asset_id, &source_path, &creator_handle).await;
    let _ = tokio::fs::remove_file(&source_path).await;

    match result {
        Ok(out) => {
            set_output(&job_id, out);
            info!(%job_id, %asset_id, "transcode pipeline complete");
        }
        Err(e) => {
            error!(%job_id, %asset_id, error=%e, "transcode pipeline failed");
            set_failed(&job_id, format!("{e:#}"));
        }
    }
}
