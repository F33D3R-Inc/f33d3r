use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    Json,
};
use manhattan_client::{handle_name, pial_name, ManhattanError};
use tracing::warn;
use uuid::Uuid;

use crate::{
    db,
    error::{AstraonError, Result},
    models::*,
    AppState,
};

pub async fn health(State(state): State<AppState>) -> (StatusCode, Json<serde_json::Value>) {
    (
        StatusCode::OK,
        Json(serde_json::json!({
            "brain":     "astraon",
            "status":    "ok",
            "manhattan": state.manhattan.configured(),
        })),
    )
}

/// Turn the identity in the path into the PIAL that analytics is keyed on.
///
/// The path segment is a NAME, not a row id. A uuid is read as `pial:<uuid>`;
/// anything else is read as `handle:<h>` — a transferable, revocable pointer at
/// an identity, which is exactly why it has to be resolved rather than trusted.
///
/// Astraon does not own identity — elohim-veni does — so this translation is a
/// resolution through Manhattan and never a join into another brain's `users`
/// table. When Manhattan cannot answer, the request fails; there is deliberately
/// no local lookup to fall back to, because that fallback is the drift the
/// naming plane exists to end.
async fn identity_pial(state: &AppState, ident: &str) -> Result<Uuid> {
    if let Ok(pial) = Uuid::parse_str(ident) {
        match state.manhattan.resolve(&pial_name(&pial.to_string())).await {
            Ok(node) if node.status == "active" => Ok(pial),
            // Revoked or tombstoned identities do not have analytics.
            Ok(_) => Err(AstraonError::NotFound),
            Err(ManhattanError::NotConfigured) => {
                // No naming plane in this deployment. The caller gave a PIAL,
                // which is the identity itself and not a pointer at one, so the
                // query can still be keyed on it — but it is unverified, and
                // that is said out loud rather than passed off as resolved.
                warn!(
                    pial = %pial,
                    "MANHATTAN_URL unset — serving analytics for an unresolved identity name"
                );
                Ok(pial)
            }
            Err(e) => Err(AstraonError::Manhattan(e)),
        }
    } else {
        // A handle only means something once resolved. There is no second way
        // for astraon to look one up, and inventing one would put a copy of
        // elohim-veni's mapping in a brain that must not hold it.
        let pial = state.manhattan.resolve_pial(&handle_name(ident)).await?;
        Uuid::parse_str(&pial).map_err(|_| {
            AstraonError::Internal(anyhow::anyhow!(
                "manhattan resolved {} to a non-uuid pial name: {}",
                handle_name(ident),
                pial
            ))
        })
    }
}

// GET /v1/analytics/creator/:identity/summary?days=30
pub async fn creator_summary(
    Path(identity): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(state): State<AppState>,
) -> Result<Json<CreatorSummary>> {
    let pial = identity_pial(&state, &identity).await?;
    let days = q.days.unwrap_or(30);
    let summary = db::get_creator_summary(&state.pool, pial, days).await?;
    Ok(Json(summary))
}

// GET /v1/analytics/creator/:identity/posts?days=30&sort_by=impressions&limit=20
pub async fn creator_posts(
    Path(identity): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(state): State<AppState>,
) -> Result<Json<CreatorPosts>> {
    let pial = identity_pial(&state, &identity).await?;
    let days = q.days.unwrap_or(30);
    let sort_by = q.sort_by.as_deref().unwrap_or("impressions").to_string();
    let limit = q.limit.unwrap_or(20).clamp(1, 100);
    let posts = db::get_creator_posts(&state.pool, pial, days, &sort_by, limit).await?;
    Ok(Json(CreatorPosts {
        posts,
        sort_by,
        period: days,
    }))
}

// GET /v1/analytics/creator/:identity/timeline?days=30
pub async fn creator_timeline(
    Path(identity): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(state): State<AppState>,
) -> Result<Json<CreatorTimeline>> {
    let pial = identity_pial(&state, &identity).await?;
    let days = q.days.unwrap_or(30);
    let tl = db::get_creator_timeline(&state.pool, pial, days).await?;
    Ok(Json(tl))
}

// GET /v1/analytics/creator/:identity/audience?days=30
pub async fn creator_audience(
    Path(identity): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(state): State<AppState>,
) -> Result<Json<CreatorAudience>> {
    let pial = identity_pial(&state, &identity).await?;
    let days = q.days.unwrap_or(30);
    let aud = db::get_creator_audience(&state.pool, pial, days).await?;
    Ok(Json(aud))
}

// GET /v1/analytics/creator/:identity/breakdown?days=30
pub async fn creator_breakdown(
    Path(identity): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(state): State<AppState>,
) -> Result<Json<CreatorBreakdown>> {
    let pial = identity_pial(&state, &identity).await?;
    let days = q.days.unwrap_or(30);
    let bd = db::get_creator_breakdown(&state.pool, pial, days).await?;
    Ok(Json(bd))
}

// GET /v1/analytics/post/:work_id
pub async fn post_detail(
    Path(work_id): Path<String>,
    State(state): State<AppState>,
) -> Result<Json<PostDetail>> {
    let id = Uuid::parse_str(&work_id)
        .map_err(|_| AstraonError::BadRequest(format!("{work_id} is not a work id")))?;
    let detail = db::get_work_detail(&state.pool, id).await?;
    Ok(Json(detail))
}

// GET /v1/analytics/site/summary
pub async fn site_summary(State(state): State<AppState>) -> Result<Json<SiteSummary>> {
    let s = db::get_site_summary(&state.pool).await?;
    Ok(Json(s))
}
