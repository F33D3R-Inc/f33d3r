use axum::{
    extract::{Path, Query, State},
    http::StatusCode,
    Json,
};
use sqlx::PgPool;

use crate::{db, error::Result, models::*};

pub async fn health() -> (StatusCode, Json<serde_json::Value>) {
    (StatusCode::OK, Json(serde_json::json!({ "brain": "astraon", "status": "ok" })))
}

// GET /v1/analytics/creator/:pial_id/summary?days=30
pub async fn creator_summary(
    Path(pial_id): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(pool): State<PgPool>,
) -> Result<Json<CreatorSummary>> {
    let days = q.days.unwrap_or(30);
    let summary = db::get_creator_summary(&pool, &pial_id, days).await?;
    Ok(Json(summary))
}

// GET /v1/analytics/creator/:pial_id/posts?days=30&sort_by=impressions&limit=20
pub async fn creator_posts(
    Path(pial_id): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(pool): State<PgPool>,
) -> Result<Json<CreatorPosts>> {
    let days    = q.days.unwrap_or(30);
    let sort_by = q.sort_by.as_deref().unwrap_or("impressions").to_string();
    let limit   = q.limit.unwrap_or(20).min(100);
    let posts   = db::get_creator_posts(&pool, &pial_id, days, &sort_by, limit).await?;
    Ok(Json(CreatorPosts { posts, sort_by, period: days }))
}

// GET /v1/analytics/creator/:pial_id/timeline?days=30
pub async fn creator_timeline(
    Path(pial_id): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(pool): State<PgPool>,
) -> Result<Json<CreatorTimeline>> {
    let days = q.days.unwrap_or(30);
    let tl = db::get_creator_timeline(&pool, &pial_id, days).await?;
    Ok(Json(tl))
}

// GET /v1/analytics/creator/:pial_id/audience?days=30
pub async fn creator_audience(
    Path(pial_id): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(pool): State<PgPool>,
) -> Result<Json<CreatorAudience>> {
    let days = q.days.unwrap_or(30);
    let aud = db::get_creator_audience(&pool, &pial_id, days).await?;
    Ok(Json(aud))
}

// GET /v1/analytics/creator/:pial_id/breakdown?days=30
pub async fn creator_breakdown(
    Path(pial_id): Path<String>,
    Query(q): Query<PeriodQuery>,
    State(pool): State<PgPool>,
) -> Result<Json<CreatorBreakdown>> {
    let days = q.days.unwrap_or(30);
    let bd = db::get_creator_breakdown(&pool, &pial_id, days).await?;
    Ok(Json(bd))
}

// GET /v1/analytics/post/:post_id
pub async fn post_detail(
    Path(post_id): Path<String>,
    State(pool): State<PgPool>,
) -> Result<Json<PostDetail>> {
    let detail = db::get_post_detail(&pool, &post_id).await?;
    Ok(Json(detail))
}

// GET /v1/analytics/site/summary
pub async fn site_summary(
    State(pool): State<PgPool>,
) -> Result<Json<SiteSummary>> {
    let s = db::get_site_summary(&pool).await?;
    Ok(Json(s))
}
