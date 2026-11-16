//! POST /analyze, GET /health.
//!
//! Zior is the audio-analysis service: tempo, key, mood and a place on the
//! Jung axes, for music and voice. It is stateless — every answer is computed
//! from the file alone and kept by the caller — and it judges nothing: what a
//! file may be shown to is Abraxas's, and who owns it is Abraxas's too.

use axum::{extract::{Multipart, State}, http::StatusCode, Json};
use chrono::Utc;
use std::sync::Arc;
use std::time::Instant;
use tracing::{info, instrument, warn};

use crate::api::types::AnalyzeResponse;
use crate::audio::{decoder, features};
use crate::config::AppConfig;
use crate::jung::mapper;

pub struct ZiorState {
    pub config: Arc<AppConfig>,
    pub internal_api_key: String,
}

const NOTE_NAMES: [&str; 12] = ["C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"];

// ── POST /analyze ─────────────────────────────────────────────────────────────

#[instrument(skip_all)]
pub async fn analyze(
    State(state): State<Arc<ZiorState>>,
    mut multipart: Multipart,
) -> Result<Json<AnalyzeResponse>, StatusCode> {
    let start = Instant::now();
    let cfg = &state.config.audio;

    let mut audio_bytes: Option<Vec<u8>> = None;
    let mut file_ext: Option<String> = None;
    while let Some(field) = multipart.next_field().await.map_err(|e| {
        warn!("malformed multipart body: {e}");
        StatusCode::BAD_REQUEST
    })? {
        if field.name() != Some("audio") {
            continue;
        }
        if let Some(filename) = field.file_name() {
            file_ext = filename.rsplit('.').next().map(|e| e.to_lowercase());
        }
        let data = field.bytes().await.map_err(|e| {
            warn!("reading the audio field: {e}");
            StatusCode::BAD_REQUEST
        })?;
        if data.len() > cfg.max_upload_bytes {
            warn!(bytes = data.len(), limit = cfg.max_upload_bytes, "audio too large");
            return Err(StatusCode::PAYLOAD_TOO_LARGE);
        }
        audio_bytes = Some(data.to_vec());
    }
    let bytes = audio_bytes.ok_or(StatusCode::BAD_REQUEST)?;

    let decoded = decoder::decode_audio(&bytes, file_ext.as_deref()).map_err(|e| {
        warn!("decode error: {}", e);
        StatusCode::UNPROCESSABLE_ENTITY
    })?;
    if decoded.duration_secs < cfg.min_duration_secs || decoded.duration_secs > cfg.max_duration_secs {
        warn!(
            duration_secs = decoded.duration_secs,
            min = cfg.min_duration_secs,
            max = cfg.max_duration_secs,
            "audio length outside what Zior analyses"
        );
        return Err(StatusCode::UNPROCESSABLE_ENTITY);
    }

    let samples = if decoded.sample_rate != cfg.target_sample_rate {
        decoder::downsample(&decoded.samples, decoded.sample_rate, cfg.target_sample_rate)
    } else {
        decoded.samples.clone()
    };

    let feats = features::extract(&samples, cfg.target_sample_rate, cfg.fft_window_size, cfg.hop_size)
        .map_err(|e| {
            warn!("feature extraction error: {}", e);
            StatusCode::UNPROCESSABLE_ENTITY
        })?;

    let mapped = mapper::map_to_axes(&feats);
    let key = format!(
        "{} {}",
        NOTE_NAMES[feats.key as usize % NOTE_NAMES.len()],
        if feats.is_major { "major" } else { "minor" }
    );

    let processing_ms = start.elapsed().as_millis() as u64;
    let mood = mapped.mood.as_str().to_string();
    info!(bpm = feats.bpm_raw, key = %key, mood = %mood, processing_ms, "audio analysed");

    Ok(Json(AnalyzeResponse {
        processing_ms,
        duration_secs: decoded.duration_secs,
        topic_vector: mapped.psych_vector.0,
        bpm: feats.bpm_raw,
        key,
        mood,
        descriptor: mapped.descriptor,
        context_tags: mapped.context_tags,
    }))
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health() -> Json<serde_json::Value> {
    Json(serde_json::json!({
        "status":  "ok",
        "service": "zior-engine",
        "version": env!("CARGO_PKG_VERSION"),
        "ts":      Utc::now().to_rfc3339(),
    }))
}
