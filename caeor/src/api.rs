//! Caeor's HTTP surface. Every route but /health and /metrics requires the
//! internal service key (auth.rs).
//!
//!   POST   /v1/assets                     ingest an upload
//!   POST   /v1/assets/clip                make a clip from a Frequency replay
//!   GET    /v1/assets/resolve?path=       what a served path is
//!   GET    /v1/assets/{id}                an asset's facts
//!   POST   /v1/assets/{id}/withdraw       withdraw (body {reason})
//!   POST   /v1/assets/{id}/recheck        ask Abraxas about a held asset now
//!   DELETE /v1/assets/{id}                the same withdrawal (body {reason}, optional)
//!   GET    /v1/assets/{id}/download       the downloadable file, streamed

use std::path::PathBuf;
use std::sync::Arc;

use axum::body::Body;
use axum::extract::{Path, Query, Request, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Json;
use chrono::Utc;
use image::ImageFormat;
use serde::{Deserialize, Serialize};
use serde_json::json;
use sqlx::PgPool;
use tokio::sync::{Notify, Semaphore};
use uuid::Uuid;

use crate::assets::{self, Kind, NewAsset, NewDerivative, UPLOAD_KINDS};
use crate::config::Config;
use crate::ids::{mark_handle, mark_text};
use crate::imaging;
use crate::ingest::{self, Incoming};
use crate::jobs::{self, ClipSpec, JobRequest, MarkSpec};
use crate::integrity::{self, AllowTo, Gate};
use crate::layout::{ancestor_dirs, Layout, Location};
use crate::mark;
use crate::withdraw;

// ── Errors ───────────────────────────────────────────────────────────────────

#[derive(Debug)]
pub struct ApiError {
    status: StatusCode,
    body: serde_json::Value,
}

impl ApiError {
    pub fn new(status: StatusCode, msg: impl Into<String>) -> Self {
        Self { status, body: json!({ "error": msg.into() }) }
    }
    pub fn bad_request(msg: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, msg)
    }
    pub fn not_found(msg: impl Into<String>) -> Self {
        Self::new(StatusCode::NOT_FOUND, msg)
    }
    pub fn internal(msg: impl Into<String>) -> Self {
        let msg = msg.into();
        tracing::error!(error = %msg, "request failed");
        Self::new(StatusCode::INTERNAL_SERVER_ERROR, msg)
    }
    pub fn with_body(status: StatusCode, body: serde_json::Value) -> Self {
        Self { status, body }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (self.status, Json(self.body)).into_response()
    }
}

impl From<sqlx::Error> for ApiError {
    fn from(e: sqlx::Error) -> Self {
        ApiError::internal(format!("database: {e}"))
    }
}

impl From<anyhow::Error> for ApiError {
    fn from(e: anyhow::Error) -> Self {
        ApiError::internal(format!("{e:#}"))
    }
}

type ApiResult<T> = Result<T, ApiError>;

// ── State ────────────────────────────────────────────────────────────────────

#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    pub layout: Layout,
    pub cfg: Arc<Config>,
    pub jobs_wake: Arc<Notify>,
    /// Image mark passes and forensic encodes running at once.
    pub encode_slots: Arc<Semaphore>,
    /// The integrity authority every upload is put to.
    pub abraxas: integrity::Abraxas,
    /// Wakes the integrity reconciler when a held asset's check is brought forward.
    pub integrity_wake: Arc<Notify>,
}

// ── Limits ───────────────────────────────────────────────────────────────────

const IMAGE_MAX: u64 = 40 * 1024 * 1024;
const AUDIO_MAX: u64 = 500 * 1024 * 1024;
const ENCRYPTED_MAX: u64 = 10 * 1024 * 1024 * 1024;
const VIDEO_MAX: u64 = 10 * 1024 * 1024 * 1024;
const VIDEO_EXT: &[&str] = &["mp4", "mov", "webm", "mkv", "m4v"];
/// Audio container extension → (ffmpeg muxer, content type).
const AUDIO_EXT: &[(&str, &str, &str)] = &[
    ("mp3", "mp3", "audio/mpeg"),
    ("m4a", "ipod", "audio/mp4"),
    ("aac", "adts", "audio/aac"),
    ("ogg", "ogg", "audio/ogg"),
    ("opus", "opus", "audio/ogg"),
    ("oga", "ogg", "audio/ogg"),
    ("webm", "webm", "audio/webm"),
    ("wav", "wav", "audio/wav"),
    ("flac", "flac", "audio/flac"),
];
const MAX_CLIP_SECONDS: u32 = 140;
/// Open clip jobs allowed before caeor says it is busy.
const CLIPS_QUEUED: i64 = 20;

// ── Health ───────────────────────────────────────────────────────────────────

pub async fn health(State(st): State<AppState>) -> Response {
    let db = crate::migrate::schema_version(&st.pool).await;
    let backlog = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL",
    )
    .fetch_one(&st.pool)
    .await;
    let open_jobs = sqlx::query_scalar::<_, i64>(
        "SELECT COUNT(*) FROM upload_jobs WHERE state IN ('queued','dispatched','cancelling')",
    )
    .fetch_one(&st.pool)
    .await;
    match (db, backlog, open_jobs) {
        (Ok(v), Ok(b), Ok(j)) => Json(json!({
            "status": "ok", "brain": "caeor", "version": "3.0",
            "schema_version": v, "manhattan_backlog": b, "open_jobs": j,
        }))
        .into_response(),
        (db, b, j) => {
            let err = [db.err().map(|e| format!("{e:#}")), b.err().map(|e| e.to_string()), j.err().map(|e| e.to_string())]
                .into_iter()
                .flatten()
                .collect::<Vec<_>>()
                .join("; ");
            (StatusCode::SERVICE_UNAVAILABLE, Json(json!({ "status": "degraded", "brain": "caeor", "error": err })))
                .into_response()
        }
    }
}

// ── POST /v1/assets ──────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct UploadQuery {
    pub kind: String,
    pub uploader_pial: Uuid,
    #[serde(default)]
    pub uploader_account: Option<Uuid>,
    #[serde(default)]
    pub uploader_handle: String,
    /// A raw body's file name; only its extension is used.
    #[serde(default)]
    pub filename: Option<String>,
    /// Create the asset even when these exact bytes are already credited to
    /// another asset. The original keeps the credit (raw_hashes is not moved).
    #[serde(default)]
    pub allow_duplicate: bool,
}

#[derive(Serialize)]
struct DuplicateResponse {
    status: &'static str,
    duplicate_of: assets::DuplicateOf,
}

/// The answer to an upload the integrity authority refused. Nothing more is
/// said: the category is for the record, not for the uploader.
fn banned() -> ApiError {
    ApiError::with_body(StatusCode::UNPROCESSABLE_ENTITY, json!({ "error": "banned" }))
}

/// Puts a freshly inserted upload to the integrity authority. A banned asset
/// is already withdrawn when this returns Err(banned()).
async fn gate_upload(st: &AppState, id: Uuid, allow_to: AllowTo, not_before: Option<std::time::Duration>) -> ApiResult<Gate> {
    match integrity::gate(&st.pool, &st.layout, &st.abraxas, id, allow_to, not_before).await? {
        Gate::Banned => Err(banned()),
        g => Ok(g),
    }
}

fn duplicate(d: assets::DuplicateOf) -> ApiError {
    ApiError::with_body(
        StatusCode::CONFLICT,
        serde_json::to_value(DuplicateResponse { status: "duplicate", duplicate_of: d })
            .unwrap_or_else(|e| json!({ "error": e.to_string() })),
    )
}

pub async fn upload(
    State(st): State<AppState>,
    Query(q): Query<UploadQuery>,
    req: Request,
) -> ApiResult<Response> {
    let kind = Kind::parse(&q.kind)
        .filter(|k| UPLOAD_KINDS.contains(&k.as_str()))
        .ok_or_else(|| ApiError::bad_request(format!("kind must be one of {}", UPLOAD_KINDS.join(", "))))?;
    if q.uploader_pial.is_nil() {
        return Err(ApiError::bad_request("uploader_pial is required"));
    }
    let handle = mark_handle(&q.uploader_handle);
    if kind.is_visibly_marked() && handle.is_empty() {
        return Err(ApiError::bad_request(
            "uploader_handle is required: it is burned into the watermark",
        ));
    }
    let limit = match kind {
        Kind::Image | Kind::Avatar | Kind::Header => IMAGE_MAX,
        Kind::Voice | Kind::Audio => AUDIO_MAX,
        Kind::Encrypted => ENCRYPTED_MAX,
        Kind::Video | Kind::Clip => VIDEO_MAX,
    };
    let incoming = ingest::receive(&st.layout, req, q.filename.clone(), limit).await?;

    if kind.is_credited_by_hash() && !q.allow_duplicate {
        if let Some(d) = assets::find_by_raw_hash(&st.pool, &incoming.sha256).await? {
            // Bytes the integrity authority refused stay refused: they are
            // never handed back as somebody's original.
            if d.is_banned() {
                return Err(banned());
            }
            return Err(duplicate(d));
        }
    }

    let asset = NewAsset {
        id: Uuid::new_v4(),
        uploader_pial: q.uploader_pial,
        uploader_account: q.uploader_account,
        uploader_handle: handle,
        kind,
        // Every upload starts held: nothing of it is servable until the
        // integrity authority has answered.
        status: "held",
        origin: "upload",
        raw_sha256: Some(incoming.sha256.clone()),
        byte_size: Some(incoming.size as i64),
        width: None,
        height: None,
        created_at: None,
        withdrawn_at: None,
        withdrawn_reason: None,
        held_for: Some(integrity::INSERT_GRACE),
    };

    let id = match kind {
        Kind::Image | Kind::Avatar | Kind::Header => ingest_image(&st, asset, incoming, q.allow_duplicate).await?,
        Kind::Video => ingest_video(&st, asset, incoming, q.allow_duplicate).await?,
        Kind::Voice | Kind::Audio => ingest_audio(&st, asset, incoming, q.allow_duplicate).await?,
        Kind::Encrypted => ingest_encrypted(&st, asset, incoming).await?,
        Kind::Clip => return Err(ApiError::bad_request("clips are made with POST /v1/assets/clip")),
    };
    let view = assets::load_view(&st.pool, id)
        .await?
        .ok_or_else(|| ApiError::internal("asset vanished after insert"))?;
    Ok((StatusCode::CREATED, Json(view)).into_response())
}

/// Removes files written for an asset whose insert did not happen.
async fn discard_written(st: &AppState, id: Uuid, private: &[Location]) {
    let dir = st.layout.media_dir.join(Layout::asset_rel(id));
    if let Err(e) = tokio::fs::remove_dir_all(&dir).await {
        if e.kind() != std::io::ErrorKind::NotFound {
            tracing::error!(path = %dir.display(), error = %e, "removing files of an asset that was not created");
        }
    }
    for loc in private {
        if let Err(e) = withdraw::remove_location(&st.layout, loc).await {
            tracing::error!(error = %format!("{e:#}"), "removing a private file of an asset that was not created");
        }
    }
}

/// Inserts the asset with its derivatives and (when credited) its raw hash,
/// in one transaction. A hash claimed concurrently by another upload turns
/// into the duplicate answer, unless duplicates were allowed.
async fn commit_asset(
    st: &AppState,
    asset: &NewAsset,
    derivatives: &[NewDerivative],
    job: Option<(&str, &JobRequest)>,
    allow_duplicate: bool,
) -> ApiResult<Result<(), assets::DuplicateOf>> {
    let mut tx = st.pool.begin().await?;
    let sid = assets::insert_asset(&mut tx, asset, None, false).await?;
    if sid.is_none() {
        return Err(ApiError::internal("asset id collision"));
    }
    if asset.kind.is_credited_by_hash() {
        if let Some(sha) = &asset.raw_sha256 {
            let first = assets::claim_raw_hash(&mut tx, sha, asset.id, asset.uploader_pial, None).await?;
            if !first && !allow_duplicate {
                tx.rollback().await?;
                let d = assets::find_by_raw_hash(&st.pool, sha)
                    .await?
                    .ok_or_else(|| ApiError::internal("raw hash conflict with no owner"))?;
                return Ok(Err(d));
            }
        }
    }
    for d in derivatives {
        assets::upsert_derivative(&mut tx, asset.id, d).await?;
    }
    if let Some((purpose, req)) = job {
        jobs::enqueue(&mut tx, purpose, req).await?;
    }
    tx.commit().await?;
    Ok(Ok(()))
}

/// Inserts the asset (status `processing`), its private source and, when the
/// kind earns credit, its raw hash, in one transaction; returns the short id
/// the database assigned. A hash already credited elsewhere is the duplicate
/// answer unless duplicates were allowed. `job` is built from the short id and
/// enqueued in the same transaction.
async fn open_asset(
    st: &AppState,
    asset: &NewAsset,
    source: &NewDerivative,
    allow_dup: bool,
    job: Option<(&str, &(dyn Fn(&str) -> JobRequest + Send + Sync))>,
) -> ApiResult<Result<String, assets::DuplicateOf>> {
    let mut tx = st.pool.begin().await?;
    let short_id = assets::insert_asset(&mut tx, asset, None, false)
        .await?
        .ok_or_else(|| ApiError::internal("asset id collision"))?;
    if asset.kind.is_credited_by_hash() {
        if let Some(sha) = &asset.raw_sha256 {
            let first = assets::claim_raw_hash(&mut tx, sha, asset.id, asset.uploader_pial, None).await?;
            if !first && !allow_dup {
                tx.rollback().await?;
                let d = assets::find_by_raw_hash(&st.pool, sha)
                    .await?
                    .ok_or_else(|| ApiError::internal("raw hash conflict with no owner"))?;
                return Ok(Err(d));
            }
        }
    }
    assets::upsert_derivative(&mut tx, asset.id, source).await?;
    if let Some((purpose, build)) = job {
        jobs::enqueue(&mut tx, purpose, &build(&short_id)).await?;
    }
    tx.commit().await?;
    Ok(Ok(short_id))
}

/// Records why an asset that exists could not be made ready.
async fn fail_asset(st: &AppState, id: Uuid, err: &ApiError) {
    let msg = err.body.get("error").and_then(|v| v.as_str()).unwrap_or("processing failed").to_string();
    if let Err(e) = sqlx::query(
        "UPDATE assets SET status = 'failed', failure = $2, integrity_next_check_at = NULL, updated_at = NOW()
          WHERE id = $1 AND status IN ('processing','held')",
    )
    .bind(id)
    .bind(&msg)
    .execute(&st.pool)
    .await
    {
        tracing::error!(asset_id = %id, error = %e, "recording an asset's failure");
    }
}

async fn ingest_image(st: &AppState, mut asset: NewAsset, incoming: Incoming, allow_dup: bool) -> ApiResult<Uuid> {
    // Decode before anything is recorded: bytes that are not an image are a
    // bad request, not a failed asset.
    let bytes = incoming.read_all().await?;
    let decoded = tokio::task::spawn_blocking(move || imaging::decode(&bytes))
        .await
        .map_err(|e| ApiError::internal(format!("decode task: {e}")))?;
    let (img, format) = decoded.map_err(|e| ApiError::new(StatusCode::UNPROCESSABLE_ENTITY, format!("{e:#}")))?;
    asset.width = Some(img.width() as i32);
    asset.height = Some(img.height() as i32);

    let ext = imaging::extension_for(format);
    let source = Layout::source(asset.id, ext);
    let size = incoming.size;
    incoming.persist(&st.layout, &source).await?;
    let source_row = NewDerivative::private_file("source_private", &source, false, &format!("image/{ext}")).size(size);

    let short_id = match open_asset(st, &asset, &source_row, allow_dup, None).await {
        Ok(Ok(sid)) => sid,
        Ok(Err(d)) => {
            discard_written(st, asset.id, &[source]).await;
            return Err(duplicate(d));
        }
        Err(e) => {
            discard_written(st, asset.id, &[source]).await;
            return Err(e);
        }
    };

    // The authority answers before a single watermarked copy is drawn, so
    // banned bytes are never rendered. An allow moves the asset to
    // processing; a hold keeps it held while it renders, and it is asked
    // about again the moment the render is done.
    gate_upload(st, asset.id, AllowTo::Processing, Some(integrity::INSERT_GRACE)).await?;

    let made: ApiResult<Vec<NewDerivative>> = async {
        let text = mark_text(&asset.uploader_handle, &short_id);
        let renditions = {
            let _slot = st.encode_slots.acquire().await.map_err(|e| ApiError::internal(e.to_string()))?;
            imaging::render(&st.layout.scratch_dir(), asset.kind, img, &text, &short_id).await?
        };
        let mut out = Vec::new();
        for r in renditions {
            let loc = Layout::served_file(asset.id, r.file);
            let abs = st.layout.abs(&loc)?;
            let (w, h) = (r.img.width(), r.img.height());
            let size = imaging::write_atomic(r.img, ImageFormat::WebP, &abs).await?;
            out.push(NewDerivative::served_file(r.role, &loc, r.watermarked, "image/webp").dims(w, h).size(size));
        }
        let mut tx = st.pool.begin().await?;
        for d in &out {
            assets::upsert_derivative(&mut tx, asset.id, d).await?;
        }
        sqlx::query(
            "UPDATE assets SET status = CASE WHEN status = 'processing' THEN 'ready' ELSE status END,
                    integrity_next_check_at = CASE WHEN status = 'held' THEN NOW() ELSE integrity_next_check_at END,
                    updated_at = NOW()
              WHERE id = $1",
        )
        .bind(asset.id)
        .execute(&mut *tx)
        .await?;
        tx.commit().await?;
        Ok(out)
    }
    .await;
    match made {
        Ok(_) => {
            st.integrity_wake.notify_one();
            Ok(asset.id)
        }
        Err(e) => {
            // The asset exists and keeps its credit and source; it failed.
            discard_written(st, asset.id, &[]).await;
            fail_asset(st, asset.id, &e).await;
            Err(e)
        }
    }
}

async fn ingest_video(st: &AppState, asset: NewAsset, incoming: Incoming, allow_dup: bool) -> ApiResult<Uuid> {
    let ext = incoming
        .extension()
        .filter(|e| VIDEO_EXT.contains(&e.as_str()))
        .unwrap_or_else(|| "mp4".into());
    let source = Layout::source(asset.id, &ext);
    let size = incoming.size;
    incoming.persist(&st.layout, &source).await?;
    let source_row = NewDerivative::private_file("source_private", &source, false, "video/*").size(size);

    let id = asset.id;
    let handle = asset.uploader_handle.clone();
    let src = source.clone();
    let build = move |short_id: &str| JobRequest {
        job_id: Uuid::new_v4(),
        asset_id: id,
        source: src.clone(),
        clip: None,
        mark: MarkSpec { handle: handle.clone(), short_id: short_id.to_string(), source_marked: false },
        hls: Some(Layout::hls_dir(id)),
        poster: Some(Layout::poster(id)),
        download: Some(Layout::download(id)),
    };
    match open_asset(st, &asset, &source_row, allow_dup, Some(("transcode", &build))).await {
        Ok(Ok(_)) => {
            // The encode is queued with the asset, and waits for the
            // authority's answer before it is dispatched (jobs.rs).
            let gate = gate_upload(st, asset.id, AllowTo::Release, None).await;
            st.jobs_wake.notify_one();
            gate?;
            Ok(asset.id)
        }
        Ok(Err(d)) => {
            discard_written(st, asset.id, &[source]).await;
            Err(duplicate(d))
        }
        Err(e) => {
            discard_written(st, asset.id, &[source]).await;
            Err(e)
        }
    }
}

async fn ingest_audio(st: &AppState, asset: NewAsset, incoming: Incoming, allow_dup: bool) -> ApiResult<Uuid> {
    let ext = incoming.extension().unwrap_or_default();
    let (ext, muxer, ctype) = AUDIO_EXT
        .iter()
        .find(|(e, _, _)| *e == ext)
        .copied()
        .ok_or_else(|| {
            ApiError::bad_request(format!(
                "audio needs a file name with one of: {}",
                AUDIO_EXT.iter().map(|(e, _, _)| *e).collect::<Vec<_>>().join(", ")
            ))
        })?;
    // Served copy: the same stream with its metadata removed. An inaudible
    // audio watermark is a later phase; this is where it will be applied.
    let served = Layout::served_file(asset.id, &format!("audio.{ext}"));
    let served_abs = st.layout.abs(&served)?;
    if let Some(parent) = served_abs.parent() {
        tokio::fs::create_dir_all(parent).await.map_err(|e| ApiError::internal(e.to_string()))?;
    }
    if let Err(e) = mark::strip_audio_metadata(&incoming.abs, &served_abs, muxer).await {
        discard_written(st, asset.id, &[]).await;
        return Err(ApiError::new(StatusCode::UNPROCESSABLE_ENTITY, format!("{e:#}")));
    }
    let served_size = tokio::fs::metadata(&served_abs).await.map_err(|e| ApiError::internal(e.to_string()))?.len();
    let source = Layout::source(asset.id, ext);
    let size = incoming.size;
    if let Err(e) = incoming.persist(&st.layout, &source).await {
        discard_written(st, asset.id, &[]).await;
        return Err(e.into());
    }
    let derivatives = [
        NewDerivative::served_file("full", &served, false, ctype).size(served_size),
        NewDerivative::private_file("source_private", &source, false, ctype).size(size),
    ];
    match commit_asset(st, &asset, &derivatives, None, allow_dup).await {
        Ok(Ok(())) => {
            gate_upload(st, asset.id, AllowTo::Release, None).await?;
            Ok(asset.id)
        }
        Ok(Err(d)) => {
            discard_written(st, asset.id, &[source]).await;
            Err(duplicate(d))
        }
        Err(e) => {
            discard_written(st, asset.id, &[source]).await;
            Err(e)
        }
    }
}

async fn ingest_encrypted(st: &AppState, asset: NewAsset, incoming: Incoming) -> ApiResult<Uuid> {
    // Ciphertext: caeor cannot see the content, so it cannot mark it. The
    // creator's client marks before it encrypts. The served copy is the blob;
    // the private source is the same bytes, kept for the record.
    let served = Layout::served_file(asset.id, "blob.bin");
    let served_abs = st.layout.abs(&served)?;
    if let Some(parent) = served_abs.parent() {
        tokio::fs::create_dir_all(parent).await.map_err(|e| ApiError::internal(e.to_string()))?;
    }
    tokio::fs::copy(&incoming.abs, &served_abs)
        .await
        .map_err(|e| ApiError::internal(format!("copying ciphertext: {e}")))?;
    let source = Layout::source(asset.id, "bin");
    let size = incoming.size;
    if let Err(e) = incoming.persist(&st.layout, &source).await {
        discard_written(st, asset.id, &[]).await;
        return Err(e.into());
    }
    let derivatives = [
        NewDerivative::served_file("full", &served, false, "application/octet-stream").size(size),
        NewDerivative::private_file("source_private", &source, false, "application/octet-stream").size(size),
    ];
    match commit_asset(st, &asset, &derivatives, None, true).await {
        Ok(Ok(())) => {
            // Ciphertext: the authority can match its exact bytes and nothing
            // more; it is asked all the same, because every upload is.
            gate_upload(st, asset.id, AllowTo::Release, None).await?;
            Ok(asset.id)
        }
        Ok(Err(d)) => Err(duplicate(d)),
        Err(e) => {
            discard_written(st, asset.id, &[source]).await;
            Err(e)
        }
    }
}

// ── POST /v1/assets/clip ─────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct ClipRequest {
    pub replay_file: String,
    pub start_seconds: u32,
    pub length_seconds: u32,
    pub title: String,
    pub host_handle: String,
    pub clipper_pial: Uuid,
    #[serde(default)]
    pub clipper_account: Option<Uuid>,
    pub clipper_handle: String,
}

pub async fn clip(State(st): State<AppState>, Json(r): Json<ClipRequest>) -> ApiResult<Response> {
    let replay = std::path::Path::new(&r.replay_file)
        .file_name()
        .and_then(|n| n.to_str())
        .filter(|n| *n == r.replay_file && n.ends_with(".mp4"))
        .ok_or_else(|| ApiError::bad_request("replay_file must be a bare .mp4 file name"))?
        .to_string();
    if r.length_seconds == 0 || r.length_seconds > MAX_CLIP_SECONDS {
        return Err(ApiError::bad_request(format!("length_seconds must be 1..={MAX_CLIP_SECONDS}")));
    }
    if r.clipper_pial.is_nil() {
        return Err(ApiError::bad_request("clipper_pial is required"));
    }
    let handle = mark_handle(&r.clipper_handle);
    if handle.is_empty() {
        return Err(ApiError::bad_request("clipper_handle is required: it is burned into the watermark"));
    }
    let open: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM upload_jobs WHERE purpose = 'clip' AND state IN ('queued','dispatched')",
    )
    .fetch_one(&st.pool)
    .await?;
    if open >= CLIPS_QUEUED {
        return Err(ApiError::new(StatusCode::SERVICE_UNAVAILABLE, "too many clips waiting"));
    }

    let asset = NewAsset {
        id: Uuid::new_v4(),
        uploader_pial: r.clipper_pial,
        uploader_account: r.clipper_account,
        uploader_handle: handle.clone(),
        kind: Kind::Clip,
        status: "processing",
        origin: "clip",
        raw_sha256: None,
        byte_size: None,
        width: None,
        height: None,
        created_at: None,
        withdrawn_at: None,
        withdrawn_reason: None,
        // A clip is drawn from the platform's own replay, not uploaded; the
        // live lane's frame checks already judged what it shows.
        held_for: None,
    };
    let source = Layout::source(asset.id, "mp4");
    let id = asset.id;
    let src = source.clone();
    let spec = ClipSpec {
        replay_file: replay,
        start_seconds: r.start_seconds,
        length_seconds: r.length_seconds,
        title: r.title.chars().take(120).collect(),
        host_handle: mark_handle(&r.host_handle),
    };
    let build = move |short_id: &str| JobRequest {
        job_id: Uuid::new_v4(),
        asset_id: id,
        source: src.clone(),
        clip: Some(spec.clone()),
        mark: MarkSpec { handle: handle.clone(), short_id: short_id.to_string(), source_marked: false },
        hls: Some(Layout::hls_dir(id)),
        poster: Some(Layout::poster(id)),
        download: Some(Layout::download(id)),
    };
    // The source is drawn by transcoding; it is recorded now so the record
    // says where it will be.
    let source_row = NewDerivative::private_file("source_private", &source, false, "video/mp4");
    match open_asset(&st, &asset, &source_row, true, Some(("clip", &build))).await? {
        Ok(_) => {}
        Err(d) => return Err(duplicate(d)),
    }
    st.jobs_wake.notify_one();

    let view = assets::load_view(&st.pool, asset.id)
        .await?
        .ok_or_else(|| ApiError::internal("asset vanished after insert"))?;
    Ok((StatusCode::CREATED, Json(view)).into_response())
}

// ── GET /v1/assets/{id} ──────────────────────────────────────────────────────

pub async fn get_asset(State(st): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Response> {
    let view = assets::load_view(&st.pool, id)
        .await?
        .ok_or_else(|| ApiError::not_found("no such asset"))?;
    Ok(Json(view).into_response())
}

// ── GET /v1/assets/resolve ───────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct ResolveQuery {
    pub path: String,
}

#[derive(Serialize)]
struct ResolveResponse {
    asset_id: Uuid,
    short_id: String,
    uploader_pial: Uuid,
    uploader_account: Option<Uuid>,
    kind: String,
    status: String,
    withdrawn: bool,
    role: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    variant: String,
    watermarked: bool,
    content_type: String,
    /// The one answer the read path needs: may these bytes be served at all?
    /// False when the asset is not ready, is withdrawn, the derivative's bytes
    /// were removed, the derivative is the download file (reached only through
    /// the download endpoint), or a content derivative is not watermarked.
    servable: bool,
}

pub async fn resolve(State(st): State<AppState>, Query(q): Query<ResolveQuery>) -> ApiResult<Response> {
    let path = q.path.split(['?', '#']).next().unwrap_or("");
    if !path.starts_with('/') || path.contains("/../") || path.ends_with("/..") || path.contains("//") {
        return Err(ApiError::bad_request("path must be an absolute URL path"));
    }
    let r = assets::resolve_path(&st.pool, path, &ancestor_dirs(path))
        .await?
        .ok_or_else(|| ApiError::not_found("no asset serves this path"))?;
    let marked_kind = Kind::parse(&r.kind).map(Kind::is_visibly_marked).unwrap_or(true);
    let servable = r.status == "ready"
        && r.removed_at.is_none()
        && r.role != "download"
        && r.role != "source_private"
        && (r.watermarked || !marked_kind);
    let body = ResolveResponse {
        asset_id: r.asset_id,
        short_id: r.short_id,
        uploader_pial: r.uploader_pial,
        uploader_account: r.uploader_account,
        withdrawn: r.status == "withdrawn",
        kind: r.kind,
        status: r.status,
        role: r.role,
        variant: r.variant,
        watermarked: r.watermarked,
        content_type: r.content_type,
        servable,
    };
    Ok(Json(body).into_response())
}

// ── POST /v1/assets/{id}/recheck ─────────────────────────────────────────────

/// Brings a held asset's next integrity check forward. It decides nothing:
/// the answer is Abraxas's, fetched by the reconciler.
pub async fn recheck(State(st): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Response> {
    if assets::get_asset(&st.pool, id).await?.is_none() {
        return Err(ApiError::not_found("no such asset"));
    }
    let moved = integrity::recheck_now(&st.pool, id).await?;
    if moved {
        st.integrity_wake.notify_one();
    }
    let view = assets::load_view(&st.pool, id).await?;
    Ok(Json(json!({ "rechecking": moved, "asset": view })).into_response())
}

// ── Withdraw ─────────────────────────────────────────────────────────────────

#[derive(Deserialize, Default)]
pub struct WithdrawBody {
    #[serde(default)]
    pub reason: String,
}

async fn do_withdraw(st: &AppState, id: Uuid, reason: &str) -> ApiResult<Response> {
    let reason = reason.trim();
    if reason.is_empty() || reason.len() > 500 {
        return Err(ApiError::bad_request("reason is required (1-500 characters)"));
    }
    match withdraw::withdraw(&st.pool, &st.layout, id, reason).await? {
        withdraw::Outcome::NotFound => Err(ApiError::not_found("no such asset")),
        withdraw::Outcome::Done(failures) => {
            let view = assets::load_view(&st.pool, id).await?;
            if failures.is_empty() {
                Ok(Json(json!({ "withdrawn": true, "complete": true, "asset": view })).into_response())
            } else {
                Err(ApiError::with_body(
                    StatusCode::INTERNAL_SERVER_ERROR,
                    json!({
                        "error": "the asset is withdrawn and served nowhere, but some bytes could not be removed; retry the withdrawal",
                        "withdrawn": true,
                        "complete": false,
                        "failures": failures,
                        "asset": view,
                    }),
                ))
            }
        }
    }
}

pub async fn withdraw_post(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    Json(b): Json<WithdrawBody>,
) -> ApiResult<Response> {
    do_withdraw(&st, id, &b.reason).await
}

pub async fn withdraw_delete(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    body: axum::body::Bytes,
) -> ApiResult<Response> {
    let b: WithdrawBody = if body.is_empty() {
        WithdrawBody { reason: "deleted".into() }
    } else {
        serde_json::from_slice(&body).map_err(|e| ApiError::bad_request(format!("body: {e}")))?
    };
    do_withdraw(&st, id, &b.reason).await
}

// ── GET /v1/assets/{id}/download ─────────────────────────────────────────────

#[derive(Deserialize)]
pub struct DownloadQuery {
    #[serde(default)]
    pub viewer_handle: String,
    #[serde(default)]
    pub forensic: bool,
}

/// Streams an open file whose directory entry may already be gone. Unlinking
/// a temp file right after opening it means no exit path can leave it behind.
async fn stream_file(file: tokio::fs::File, len: u64, ctype: &str, filename: &str) -> ApiResult<Response> {
    let stream = futures_util::stream::unfold(file, |mut f| async move {
        use tokio::io::AsyncReadExt;
        let mut buf = vec![0u8; 256 * 1024];
        match f.read(&mut buf).await {
            Ok(0) => None,
            Ok(n) => {
                buf.truncate(n);
                Some((Ok::<_, std::io::Error>(axum::body::Bytes::from(buf)), f))
            }
            Err(e) => Some((Err(e), f)),
        }
    });
    let mut resp = Body::from_stream(stream).into_response();
    let h = resp.headers_mut();
    h.insert(header::CONTENT_TYPE, HeaderValue::from_str(ctype).map_err(|e| ApiError::internal(e.to_string()))?);
    h.insert(header::CONTENT_LENGTH, HeaderValue::from(len));
    h.insert(
        header::CONTENT_DISPOSITION,
        HeaderValue::from_str(&format!("attachment; filename=\"{filename}\""))
            .map_err(|e| ApiError::internal(e.to_string()))?,
    );
    h.insert(header::CACHE_CONTROL, HeaderValue::from_static("private, no-store"));
    Ok(resp)
}

async fn open_temp(path: &std::path::Path) -> ApiResult<(tokio::fs::File, u64)> {
    let file = tokio::fs::File::open(path).await.map_err(|e| ApiError::internal(format!("opening {}: {e}", path.display())))?;
    let len = file.metadata().await.map_err(|e| ApiError::internal(e.to_string()))?.len();
    tokio::fs::remove_file(path)
        .await
        .map_err(|e| ApiError::internal(format!("unlinking {}: {e}", path.display())))?;
    Ok((file, len))
}

pub async fn download(
    State(st): State<AppState>,
    Path(id): Path<Uuid>,
    Query(q): Query<DownloadQuery>,
) -> ApiResult<Response> {
    let a = assets::get_asset(&st.pool, id).await?.ok_or_else(|| ApiError::not_found("no such asset"))?;
    if a.is_withdrawn() {
        return Err(ApiError::new(StatusCode::GONE, "asset withdrawn"));
    }
    if a.status != "ready" {
        return Err(ApiError::new(StatusCode::CONFLICT, format!("asset is {}", a.status)));
    }
    let viewer = mark_handle(&q.viewer_handle);
    if q.forensic && viewer.is_empty() {
        return Err(ApiError::bad_request("forensic=true needs viewer_handle"));
    }
    let ds = assets::get_derivatives(&st.pool, id).await?;
    let base_name = format!("{}-{}", if a.uploader_handle.is_empty() { "f33d3r" } else { &a.uploader_handle }, a.short_id);

    let find_abs = |role: &str| -> ApiResult<(PathBuf, String)> {
        let d = ds
            .iter()
            .find(|d| d.role == role && d.removed_at.is_none())
            .ok_or_else(|| ApiError::not_found(format!("asset has no {role} file")))?;
        if a.kind().is_visibly_marked() && !d.watermarked {
            // Imported media not yet re-marked: a clean copy is never handed out.
            return Err(ApiError::new(
                StatusCode::CONFLICT,
                format!("the {role} file is not watermarked yet (run `caeor remark`)"),
            ));
        }
        let abs = match d.location() {
            Some(loc) => st.layout.abs(&loc)?,
            None => st
                .layout
                .served_url_to_abs(&d.path)
                .ok_or_else(|| ApiError::internal(format!("{} is outside caeor's stores", d.path)))?,
        };
        Ok((abs, d.content_type.clone()))
    };

    match a.kind() {
        Kind::Video | Kind::Clip => {
            let (src, _) = find_abs("download").or_else(|_| find_abs("full"))?;
            if !q.forensic {
                let file = tokio::fs::File::open(&src).await.map_err(|e| ApiError::internal(format!("opening download: {e}")))?;
                let len = file.metadata().await.map_err(|e| ApiError::internal(e.to_string()))?.len();
                return stream_file(file, len, "video/mp4", &format!("{base_name}.mp4")).await;
            }
            let tmp = st.layout.scratch_dir().join(format!("forensic-{}.mp4", Uuid::new_v4()));
            let made = {
                let _slot = st.encode_slots.acquire().await.map_err(|e| ApiError::internal(e.to_string()))?;
                mark::forensic_video(&st.layout.scratch_dir(), &src, &viewer, &tmp).await
            };
            if let Err(e) = made {
                if let Err(e2) = tokio::fs::remove_file(&tmp).await {
                    if e2.kind() != std::io::ErrorKind::NotFound {
                        tracing::error!(path = %tmp.display(), error = %e2, "removing failed forensic output");
                    }
                }
                return Err(e.into());
            }
            tracing::info!(asset_id = %id, viewer = %viewer, at = %Utc::now(), "forensic download issued");
            let (file, len) = open_temp(&tmp).await?;
            stream_file(file, len, "video/mp4", &format!("{base_name}.mp4")).await
        }
        Kind::Image => {
            let (src, ctype) = find_abs("full")?;
            if !q.forensic {
                let file = tokio::fs::File::open(&src).await.map_err(|e| ApiError::internal(format!("opening full: {e}")))?;
                let len = file.metadata().await.map_err(|e| ApiError::internal(e.to_string()))?.len();
                let ext = src.extension().and_then(|e| e.to_str()).unwrap_or("webp").to_string();
                return stream_file(file, len, &ctype, &format!("{base_name}.{ext}")).await;
            }
            let bytes = tokio::fs::read(&src).await.map_err(|e| ApiError::internal(format!("reading full: {e}")))?;
            let (img, _) = tokio::task::spawn_blocking(move || imaging::decode(&bytes))
                .await
                .map_err(|e| ApiError::internal(e.to_string()))??;
            let marked = {
                let _slot = st.encode_slots.acquire().await.map_err(|e| ApiError::internal(e.to_string()))?;
                mark::forensic_image(&st.layout.scratch_dir(), &img, &viewer).await?
            };
            let tmp = st.layout.scratch_dir().join(format!("forensic-{}.webp", Uuid::new_v4()));
            imaging::write_atomic(marked, ImageFormat::WebP, &tmp).await?;
            tracing::info!(asset_id = %id, viewer = %viewer, "forensic image download issued");
            let (file, len) = open_temp(&tmp).await?;
            stream_file(file, len, "image/webp", &format!("{base_name}.webp")).await
        }
        Kind::Voice | Kind::Audio => {
            if q.forensic {
                return Err(ApiError::new(
                    StatusCode::UNPROCESSABLE_ENTITY,
                    "no forensic layer exists for audio yet; the caller must decide whether to offer this download",
                ));
            }
            let (src, ctype) = find_abs("full")?;
            let file = tokio::fs::File::open(&src).await.map_err(|e| ApiError::internal(format!("opening audio: {e}")))?;
            let len = file.metadata().await.map_err(|e| ApiError::internal(e.to_string()))?.len();
            let ext = src.extension().and_then(|e| e.to_str()).unwrap_or("bin").to_string();
            stream_file(file, len, &ctype, &format!("{base_name}.{ext}")).await
        }
        Kind::Avatar | Kind::Header | Kind::Encrypted => Err(ApiError::new(
            StatusCode::UNPROCESSABLE_ENTITY,
            format!("{} assets are not downloadable", a.kind),
        )),
    }
}
