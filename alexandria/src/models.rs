use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

#[derive(Debug, Serialize, Deserialize, sqlx::FromRow)]
pub struct Author {
    pub pial_id: Uuid,
    pub handle: String,
    pub display_name: String,
    pub avatar_url: String,
    pub header_url: String,
    pub bio: String,
    pub primary_section_id: Option<Uuid>,
    pub creator_tier: String,
    pub is_verified: bool,
    pub title_count: i32,
    pub follower_count: i32,
    pub following_count: i32,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Deserialize)]
pub struct BootstrapAuthorReq {
    pub pial_id: Uuid,
    pub handle: String,
    pub display_name: Option<String>,
    pub avatar_url: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct UpdateAuthorReq {
    pub display_name: Option<String>,
    pub avatar_url: Option<String>,
    pub header_url: Option<String>,
    pub bio: Option<String>,
    pub primary_section_id: Option<Uuid>,
    pub creator_tier: Option<String>,
    pub is_verified: Option<bool>,
}

#[derive(Debug, Serialize, Deserialize, sqlx::FromRow)]
pub struct Title {
    pub id: Uuid,
    pub author_pial_id: Uuid,
    pub section_id: Option<Uuid>,
    pub genre_id: Option<Uuid>,
    pub headline: Option<String>,
    pub body: String,
    pub media_area: String,
    pub status: String,
    pub visibility: String,
    pub parent_id: Option<Uuid>,
    pub root_id: Option<Uuid>,
    pub thread_id: Option<Uuid>,
    pub depth: i16,
    pub lineage: String,
    pub content_hash: Option<String>,
    pub is_nsfw: bool,
    pub is_sensitive: bool,
    pub reply_restriction: String,
    pub published_at: Option<DateTime<Utc>>,
    pub scheduled_at: Option<DateTime<Utc>>,
    pub tombstoned_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub legacy_work_id: Option<Uuid>,
    pub legacy_post_id: Option<Uuid>,
}

#[derive(Debug, Deserialize)]
pub struct CreateTitleReq {
    pub author_pial_id: Uuid,
    pub section_slug: Option<String>,
    pub genre_slug: Option<String>,
    pub headline: Option<String>,
    pub body: String,
    pub media_area: Option<String>,
    pub visibility: Option<String>,
    pub parent_id: Option<Uuid>,
    pub root_id: Option<Uuid>,
    pub thread_id: Option<Uuid>,
    pub is_nsfw: Option<bool>,
    pub is_sensitive: Option<bool>,
    pub tags: Option<Vec<String>>,
    pub holdings: Option<Vec<HoldingInput>>,
    pub legacy_work_id: Option<Uuid>,
    pub legacy_post_id: Option<Uuid>,
}

#[derive(Debug, Deserialize, Clone)]
pub struct HoldingInput {
    pub asset_type: String,
    pub caeor_path: String,
    pub mime_type: Option<String>,
    pub width: Option<i32>,
    pub height: Option<i32>,
    pub duration_secs: Option<i32>,
    pub is_primary: Option<bool>,
}

#[derive(Debug, Deserialize)]
pub struct UpdateTitleReq {
    pub headline: Option<String>,
    pub body: Option<String>,
    pub status: Option<String>,
    pub visibility: Option<String>,
    pub genre_slug: Option<String>,
    pub is_nsfw: Option<bool>,
    pub is_sensitive: Option<bool>,
}

#[derive(Debug, Deserialize)]
pub struct IncrementSignalsReq {
    pub like_delta: Option<i32>,
    pub repost_delta: Option<i32>,
    pub reply_delta: Option<i32>,
    pub quote_delta: Option<i32>,
    pub view_delta: Option<i64>,
    pub bookmark_delta: Option<i32>,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct TitleSignals {
    pub title_id: Uuid,
    pub like_count: i32,
    pub repost_count: i32,
    pub reply_count: i32,
    pub quote_count: i32,
    pub view_count: i64,
    pub bookmark_count: i32,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct Holding {
    pub id: Uuid,
    pub title_id: Uuid,
    pub asset_type: String,
    pub caeor_path: String,
    pub mime_type: String,
    pub file_size: Option<i64>,
    pub width: Option<i32>,
    pub height: Option<i32>,
    pub duration_secs: Option<i32>,
    pub is_primary: bool,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct Section {
    pub id: Uuid,
    pub slug: String,
    pub name: String,
    pub description: String,
    pub display_order: i32,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct Genre {
    pub id: Uuid,
    pub section_id: Uuid,
    pub slug: String,
    pub name: String,
    pub parent_genre_id: Option<Uuid>,
    pub display_order: i32,
}

#[derive(Debug, Serialize)]
pub struct TitleFull {
    #[serde(flatten)]
    pub title: Title,
    pub author: Option<Author>,
    pub signals: Option<TitleSignals>,
    pub holdings: Vec<Holding>,
    pub tags: Vec<String>,
}

#[derive(Debug, Deserialize)]
pub struct ListTitlesQuery {
    pub section: Option<String>,
    pub genre: Option<String>,
    pub author_pial: Option<Uuid>,
    pub after: Option<DateTime<Utc>>,
    pub limit: Option<i64>,
}

#[derive(Debug, Deserialize)]
pub struct ListGenresQuery {
    pub section: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct ApiError {
    pub error: String,
}

impl ApiError {
    pub fn new(msg: impl Into<String>) -> Self {
        Self { error: msg.into() }
    }
}
