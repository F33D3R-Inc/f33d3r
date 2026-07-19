use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::Json,
};
use serde_json::{json, Value};
use tracing::{info, warn};

use crate::{db, models::LoreEvent};

#[derive(Clone)]
pub struct AppState {
    pub pool: sqlx::PgPool,
    pub herald_url: String,
    pub herald_client: reqwest::Client,
}

pub async fn health() -> Json<Value> {
    Json(json!({"status": "ok", "service": "lore"}))
}

pub async fn list_achievements(State(s): State<AppState>) -> Result<Json<Value>, StatusCode> {
    match db::list_achievements(&s.pool).await {
        Ok(defs) => Ok(Json(json!({"achievements": defs}))),
        Err(e) => { tracing::error!("list_achievements: {e}"); Err(StatusCode::INTERNAL_SERVER_ERROR) }
    }
}

pub async fn get_user_achievements(
    State(s): State<AppState>,
    Path(pial_shard_id): Path<String>,
) -> Result<Json<Value>, StatusCode> {
    let achievements = db::user_achievements(&s.pool, &pial_shard_id).await
        .map_err(|e| { tracing::error!("user_achievements: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;
    let xp = db::get_user_xp(&s.pool, &pial_shard_id).await
        .map_err(|e| { tracing::error!("get_user_xp: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;
    let xp_status = db::xp_status(xp.total_xp);

    Ok(Json(json!({
        "pial_shard_id": pial_shard_id,
        "achievements": achievements,
        "xp": xp_status,
    })))
}

pub async fn award_achievement(
    State(s): State<AppState>,
    Path((pial_shard_id, achievement_id)): Path<(String, String)>,
) -> Result<Json<Value>, StatusCode> {
    match db::award_achievement(&s.pool, &pial_shard_id, &achievement_id).await {
        Ok(Some(xp)) => {
            info!(pial = %pial_shard_id, achievement = %achievement_id, xp, "achievement awarded");
            // Notify Herald to push to device
            let _ = notify_herald(&s, &pial_shard_id, &achievement_id, xp).await;
            Ok(Json(json!({"awarded": true, "xp_reward": xp})))
        }
        Ok(None) => Ok(Json(json!({"awarded": false, "reason": "already_earned"}))),
        Err(e) => { tracing::error!("award_achievement: {e}"); Err(StatusCode::INTERNAL_SERVER_ERROR) }
    }
}

pub async fn get_xp(
    State(s): State<AppState>,
    Path(pial_shard_id): Path<String>,
) -> Result<Json<Value>, StatusCode> {
    let xp = db::get_user_xp(&s.pool, &pial_shard_id).await
        .map_err(|e| { tracing::error!("get_xp: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;
    let status = db::xp_status(xp.total_xp);
    Ok(Json(json!(status)))
}

pub async fn receive_event(
    State(s): State<AppState>,
    Json(event): Json<LoreEvent>,
) -> Json<Value> {
    info!(event_type = %event.event_type, pial = %event.pial_shard_id, "lore event received");

    // Map event types to achievement IDs — expand as triggers are wired up
    let achievement_id = match event.event_type.as_str() {
        "user.registered"         => Some("first_pulse"),
        "avatar.uploaded"         => Some("face_reveal"),
        "profile.completed"       => Some("signal_found"),
        "verification.completed"  => Some("echo_online"),
        "message.sent"            => Some("first_contact"),
        "audio.uploaded"          => Some("mic_check"),
        "video.uploaded"          => Some("directors_cut"),
        "song.published"          => Some("bedroom_producer"),
        "sale.completed"          => Some("first_sale"),
        "payout.received"         => Some("paid_creator"),
        "user.early_adopter"      => Some("founding_member"),
        _ => {
            warn!(event_type = %event.event_type, "no achievement mapping");
            None
        }
    };

    if let Some(ach_id) = achievement_id {
        match db::award_achievement(&s.pool, &event.pial_shard_id, ach_id).await {
            Ok(Some(xp)) => {
                info!(achievement = ach_id, xp, "achievement triggered by event");
                let _ = notify_herald(&s, &event.pial_shard_id, ach_id, xp).await;
            }
            Ok(None) => {}
            Err(e) => tracing::error!("award from event: {e}"),
        }
    }

    Json(json!({"received": true}))
}

async fn notify_herald(s: &AppState, pial_shard_id: &str, achievement_id: &str, xp: i32) -> anyhow::Result<()> {
    // TODO: full push delivery via Herald when Kafka integration is wired.
    // For now, call Herald's future notification endpoint directly.
    let url = format!("{}/v1/notify/achievement", s.herald_url);
    let _ = s.herald_client.post(&url)
        .json(&json!({
            "pial_shard_id": pial_shard_id,
            "achievement_id": achievement_id,
            "xp_reward": xp,
        }))
        .send().await;
    Ok(())
}
