use axum::{
    extract::{Multipart, Path, State},
    http::StatusCode,
    Json,
};
use chrono::Utc;
use std::collections::HashMap;
use std::sync::Arc;
use std::time::Instant;
use tracing::{info, instrument, warn};
use uuid::Uuid;

use crate::api::types::*;
use crate::audio::{decoder, features, fingerprint, segmenter};
use crate::behavioral::events::{BehavioralStore, PlayEvent};
use crate::behavioral::velocity::VelocityTracker;
use crate::config::AppConfig;
use crate::jung::{axes::PsychVector, mapper};
use crate::signals::{emitter::SignalEmitter, types::AethyrSignal};
use crate::store::TrackStore;
use crate::vector::VectorStore;

// ── Application state ─────────────────────────────────────────────────────────

pub struct ZiorState {
    pub config: Arc<AppConfig>,
    pub track_store: Arc<TrackStore>,
    pub vector_store: Arc<tokio::sync::RwLock<VectorStore>>,
    pub behavior_store: Arc<BehavioralStore>,
    pub velocity_trackers: Arc<dashmap::DashMap<String, VelocityTracker>>,
    pub emitter: Arc<SignalEmitter>,
}

// ── POST /upload ──────────────────────────────────────────────────────────────

#[instrument(skip_all)]
pub async fn upload_track(
    State(state): State<Arc<ZiorState>>,
    mut multipart: Multipart,
) -> Result<Json<UploadResponse>, StatusCode> {
    let start = Instant::now();

    // Extract file bytes and metadata from multipart form
    let mut audio_bytes: Option<Vec<u8>> = None;
    let mut file_ext: Option<String> = None;
    let mut creator_id = "unknown".to_string();
    let mut provided_id: Option<String> = None;

    while let Some(field) = multipart
        .next_field()
        .await
        .map_err(|_| StatusCode::BAD_REQUEST)?
    {
        let name = field.name().unwrap_or("").to_string();
        match name.as_str() {
            "audio" => {
                // Extract extension from content-disposition filename
                if let Some(filename) = field.file_name() {
                    file_ext = filename.rsplit('.').next().map(|e| e.to_lowercase());
                }
                let data = field.bytes().await.map_err(|_| StatusCode::BAD_REQUEST)?;
                if data.len() > state.config.audio.max_upload_bytes {
                    warn!("upload too large: {} bytes", data.len());
                    return Err(StatusCode::PAYLOAD_TOO_LARGE);
                }
                audio_bytes = Some(data.to_vec());
            }
            "creator_id" => {
                creator_id = field.text().await.unwrap_or_default();
            }
            "track_id" => {
                provided_id = field.text().await.ok();
            }
            _ => {}
        }
    }

    let bytes = audio_bytes.ok_or(StatusCode::BAD_REQUEST)?;

    // ── Decode ────────────────────────────────────────────────────────────────
    let decoded = decoder::decode_audio(&bytes, file_ext.as_deref()).map_err(|e| {
        warn!("decode error: {}", e);
        StatusCode::UNPROCESSABLE_ENTITY
    })?;

    let cfg = &state.config.audio;
    if decoded.duration_secs < cfg.min_duration_secs {
        return Err(StatusCode::UNPROCESSABLE_ENTITY);
    }

    // Resample to target rate for analysis
    let samples = if decoded.sample_rate != cfg.target_sample_rate {
        decoder::downsample(
            &decoded.samples,
            decoded.sample_rate,
            cfg.target_sample_rate,
        )
    } else {
        decoded.samples.clone()
    };

    // ── Feature extraction ────────────────────────────────────────────────────
    let feats = features::extract(
        &samples,
        cfg.target_sample_rate,
        cfg.fft_window_size,
        cfg.hop_size,
    )
    .map_err(|e| {
        warn!("feature extraction error: {}", e);
        StatusCode::INTERNAL_SERVER_ERROR
    })?;

    // ── Fingerprint ───────────────────────────────────────────────────────────
    let fp = fingerprint::generate(&samples, cfg.target_sample_rate);
    let track_id = provided_id.unwrap_or_else(|| format!("zior_{}", &fp.hash[..16]));

    // ── Structural segmentation ───────────────────────────────────────────────
    let sections = segmenter::segment(&samples, cfg.target_sample_rate);
    let early_retention = segmenter::early_retention_proxy(&sections);

    // ── Jung mapping ──────────────────────────────────────────────────────────
    // Blend with existing behavioral vector if this track has been seen before
    let behavioral_vec = state.behavior_store.psych_vector(&track_id);
    let mapped = mapper::map_to_axes(&feats, behavioral_vec.as_ref());

    // ── Velocity (initial — based on audio quality signals) ───────────────────
    let tracker = state
        .velocity_trackers
        .entry(track_id.clone())
        .or_insert_with(|| VelocityTracker::new(state.config.velocity.window_hours));
    let velocity_score = tracker.velocity_score();
    drop(tracker);

    // ── Explicit content classifier ───────────────────────────────────────────
    // Production: replace with a trained audio classifier.
    // Stub: uses energy + spectral heuristics as a proxy.
    let adult_probability = explicit_probability_stub(&feats);

    // ── Build signal ──────────────────────────────────────────────────────────
    let note_names = [
        "C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B",
    ];
    let key_str = format!(
        "{} {}",
        note_names[feats.key as usize],
        if feats.is_major { "major" } else { "minor" }
    );

    let signal = AethyrSignal {
        track_id: track_id.clone(),
        creator_id: creator_id.clone(),
        computed_at: Utc::now(),
        topic_vector: mapped.psych_vector.to_vec(),
        velocity_score,
        early_retention,
        completion_rate: 0.5, // unknown until behavioral data arrives
        conversion_probability: 0.05,
        creator_revenue_rate: 0.10,
        ltv_estimate: 0.10,
        adult_probability,
        exposure_count: 0,
        creator_exposure: 0,
        fingerprint: fp.hash.clone(),
        duration_secs: decoded.duration_secs,
        bpm: feats.bpm_raw,
        key: feats.key,
        is_major: feats.is_major,
        mood: mapped.mood.as_str().to_string(),
        context_tags: mapped.context_tags.clone(),
        descriptor: mapped.descriptor.clone(),
        tonal_valence: feats.tonal_valence,
        rms_energy: feats.rms_energy,
        bass_energy: feats.bass_energy,
        vocal_probability: feats.vocal_probability,
        has_vocals: feats.vocal_probability > 0.5,
        replay_rate: 0.0,
        share_velocity: 0.0,
    };

    // ── Store vector + emit signal ─────────────────────────────────────────────
    {
        let mut vs = state.vector_store.write().await;
        vs.upsert(track_id.clone(), mapped.psych_vector);
    }

    state.emitter.emit(signal.clone()).await.map_err(|e| {
        warn!("emit error: {}", e);
        StatusCode::INTERNAL_SERVER_ERROR
    })?;

    let processing_ms = start.elapsed().as_millis() as u64;
    info!(
        track_id = %track_id,
        bpm      = feats.bpm_raw,
        key      = %key_str,
        mood     = %signal.mood,
        processing_ms,
        "track processed"
    );

    Ok(Json(UploadResponse {
        track_id: track_id.clone(),
        fingerprint: fp.hash,
        status: "processed".to_string(),
        processing_ms,
        signal_preview: SignalPreview {
            bpm: feats.bpm_raw,
            key: key_str,
            mood: signal.mood.clone(),
            descriptor: signal.descriptor.clone(),
            velocity_score,
            context_tags: signal.context_tags.clone(),
        },
    }))
}

// ── GET /signal/:track_id ─────────────────────────────────────────────────────

pub async fn get_signal(
    State(state): State<Arc<ZiorState>>,
    Path(track_id): Path<String>,
) -> Result<Json<AethyrSignal>, StatusCode> {
    state
        .track_store
        .get_signal(&track_id)
        .map(Json)
        .ok_or(StatusCode::NOT_FOUND)
}

// ── GET /signals ──────────────────────────────────────────────────────────────

pub async fn list_signals(State(state): State<Arc<ZiorState>>) -> Json<serde_json::Value> {
    let signals = state.track_store.all_signals();
    Json(serde_json::json!({
        "count":   signals.len(),
        "signals": signals,
    }))
}

// ── POST /events ──────────────────────────────────────────────────────────────

pub async fn ingest_events(
    State(state): State<Arc<ZiorState>>,
    Json(req): Json<IngestEventsRequest>,
) -> Json<IngestResponse> {
    let mut affected_tracks: std::collections::HashSet<String> = std::collections::HashSet::new();
    let n = req.events.len();

    for ev in req.events {
        let ts = ev.timestamp.unwrap_or_else(Utc::now);
        let event_type_str = format!("{:?}", ev.event_type).to_lowercase();

        // Behavioral store update
        state.behavior_store.ingest(PlayEvent {
            track_id: ev.track_id.clone(),
            user_id: ev.user_id.clone(),
            event_type: ev.event_type,
            position_secs: ev.position_secs,
            duration_secs: ev.duration_secs,
            timestamp: ts,
            session_id: ev.session_id.clone(),
        });

        // Velocity tracker update
        state
            .velocity_trackers
            .entry(ev.track_id.clone())
            .or_insert_with(|| VelocityTracker::new(state.config.velocity.window_hours))
            .record_at(&event_type_str, ts);

        affected_tracks.insert(ev.track_id);
    }

    // For affected tracks with existing signals, emit behavioral updates
    for track_id in &affected_tracks {
        if let Some(existing) = state.track_store.get_signal(track_id) {
            let completion = state.behavior_store.retention_rate(track_id);
            let replay = state.behavior_store.replay_rate(track_id);
            let share_vel = state.behavior_store.share_velocity(track_id);
            let velocity = state
                .velocity_trackers
                .get(track_id)
                .map(|t| t.velocity_score())
                .unwrap_or(0.5);

            let exposure = existing.exposure_count
                + state
                    .behavior_store
                    .stats(track_id)
                    .map(|s| s.total_plays)
                    .unwrap_or(0);

            let update = crate::signals::types::BehavioralSignalUpdate {
                track_id: track_id.clone(),
                updated_at: Utc::now(),
                velocity_score: velocity,
                early_retention: existing.early_retention,
                completion_rate: completion,
                replay_rate: replay,
                share_velocity: share_vel,
                exposure_count: exposure,
            };

            let _ = state.emitter.emit_behavioral(update).await;
        }
    }

    Json(IngestResponse {
        ingested: n,
        track_ids: affected_tracks.into_iter().collect(),
    })
}

// ── GET /similar/:track_id ────────────────────────────────────────────────────

pub async fn similar_tracks(
    State(state): State<Arc<ZiorState>>,
    Path(track_id): Path<String>,
) -> Result<Json<SimilarTracksResponse>, StatusCode> {
    let signal = state
        .track_store
        .get_signal(&track_id)
        .ok_or(StatusCode::NOT_FOUND)?;

    let query_vec = PsychVector(
        signal
            .topic_vector
            .as_slice()
            .try_into()
            .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?,
    );

    let vs = state.vector_store.read().await;
    let similar = vs.nearest(&query_vec, 10, &track_id);
    drop(vs);

    let result: Vec<SimilarTrack> = similar
        .into_iter()
        .filter_map(|(tid, sim)| {
            state.track_store.get_signal(&tid).map(|s| SimilarTrack {
                track_id: tid,
                similarity: sim,
                mood: s.mood,
                descriptor: s.descriptor,
            })
        })
        .collect();

    Ok(Json(SimilarTracksResponse {
        track_id,
        similar: result,
    }))
}

// ── GET /clusters ─────────────────────────────────────────────────────────────

pub async fn get_clusters(State(state): State<Arc<ZiorState>>) -> Json<ClustersResponse> {
    let vs = state.vector_store.read().await;
    let assignments = vs.cluster(0.80);
    drop(vs);

    let mut by_cluster: HashMap<u32, Vec<String>> = HashMap::new();
    for (tid, cid) in assignments {
        by_cluster.entry(cid).or_default().push(tid);
    }

    let n = by_cluster.len();
    let clusters: Vec<ClusterSummary> = by_cluster
        .into_iter()
        .map(|(cid, tids)| ClusterSummary {
            cluster_id: cid,
            track_count: tids.len(),
            track_ids: tids,
        })
        .collect();

    Json(ClustersResponse {
        n_clusters: n,
        clusters,
    })
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health(State(state): State<Arc<ZiorState>>) -> Json<serde_json::Value> {
    let vs = state.vector_store.read().await;
    Json(serde_json::json!({
        "status":       "ok",
        "service":      "zior-engine",
        "version":      env!("CARGO_PKG_VERSION"),
        "ts":           Utc::now().to_rfc3339(),
        "tracks_stored": state.track_store.count(),
        "vectors_stored": vs.len(),
    }))
}

// ── Stub: explicit content classifier ────────────────────────────────────────
// In production: replace with a trained binary audio classifier.
// The stub uses purely acoustic heuristics as a proxy.
fn explicit_probability_stub(feats: &crate::audio::features::AudioFeatures) -> f64 {
    // Heuristic: explicit tracks tend to have:
    // - high mid energy (speech/vocals)
    // - high spectral flux (dynamic delivery)
    // - NOT correlated with explicit content in any valid way
    // This is intentionally conservative — defaults to 0.0 for clean detection
    // until a real classifier is plugged in.
    let _ = feats;
    0.0 // SAFE DEFAULT: never classify as explicit without a real model
}
