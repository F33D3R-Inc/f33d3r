//! Astraon's wire shapes.
//!
//! Field names are a contract: feed-engine decodes these structures in
//! `internal/handler/analytics.go`. Existing names are therefore never renamed —
//! `post_id` still says post, though it now carries a work id — and the naming
//! plane arrives as *added* fields (`identity_name`, `work_name`, `cid_name`,
//! `author_name`) so a caller can hold a NAME instead of a row id.

use chrono::{DateTime, NaiveDate, Utc};
use serde::{Deserialize, Serialize};

// ── Creator summary ───────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize, Default)]
pub struct CreatorSummary {
    /// `pial:<uuid>` — the identity this page measures, as a resolvable name.
    pub identity_name: String,
    pub pial_id: String,
    pub period_days: i32, // 0 = all-time
    pub total_posts: i64,
    pub impressions: i64,
    pub likes: i64,
    pub reposts: i64,
    pub replies: i64,
    /// Derived from work_citations. The column this used to come from,
    /// works.quote_count, was never incremented by anything and reported zero
    /// for every work forever; feed-engine dropped it in migration 0004.
    pub quotes: i64,
    pub saves: i64,
    pub profile_visits: i64,
    pub followers_gained: i64,
    pub followers_lost: i64,
    pub net_followers: i64,
    pub current_followers: i64,
    pub engagement_rate: f64, // (likes+reposts+replies) / max(impressions,1) * 100
    pub view_time_secs: i64,
    // 30d change percentages vs prior 30d (null when insufficient history)
    pub impressions_pct_change: Option<f64>,
    pub likes_pct_change: Option<f64>,
    pub followers_pct_change: Option<f64>,
}

// ── Work performance ──────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize)]
pub struct PostRow {
    /// `uuid:<work_id>` — this work as a resolvable name.
    pub work_name: String,
    /// `cid:sha256:<hex>` — the same work by content address.
    pub cid_name: String,
    pub post_id: String,
    pub body_preview: String, // first 120 chars
    pub content_type: String, // text | image | video | poll | voice
    pub created_at: DateTime<Utc>,
    pub time_ago: String,
    pub impressions: i64,
    pub likes: i64,
    pub reposts: i64,
    pub replies: i64,
    pub quotes: i64,
    pub saves: i64,
    pub engagement_pct: f64,
    pub bar_pct: f64, // normalised to top work = 100%
}

#[derive(Debug, Serialize, Deserialize)]
pub struct CreatorPosts {
    pub posts: Vec<PostRow>,
    pub sort_by: String,
    pub period: i32,
}

// ── Timeline (day-by-day chart data) ─────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize)]
pub struct TimelinePoint {
    pub date: NaiveDate,
    pub impressions: i64,
    pub likes: i64,
    pub reposts: i64,
    pub replies: i64,
    pub posts: i64,
    pub engagement: f64,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct CreatorTimeline {
    pub points: Vec<TimelinePoint>,
    pub period_days: i32,
    pub max_impressions: i64,
    pub max_engagement: f64,
}

// ── Audience growth ───────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize)]
pub struct AudiencePoint {
    pub date: NaiveDate,
    pub new_followers: i64,
    pub unfollows: i64,
    pub net: i64,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct CreatorAudience {
    pub points: Vec<AudiencePoint>,
    pub total_followers: i64,
    pub period_days: i32,
}

// ── Content breakdown ─────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize)]
pub struct ContentTypeBreakdown {
    pub content_type: String,
    pub count: i64,
    pub impressions: i64,
    pub avg_engagement: f64,
    pub pct_of_posts: f64,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct CreatorBreakdown {
    pub types: Vec<ContentTypeBreakdown>,
}

// ── Single work detail ────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize)]
pub struct PostDetail {
    /// `uuid:<work_id>` — this work as a resolvable name.
    pub work_name: String,
    /// `cid:sha256:<hex>` — the same work by content address.
    pub cid_name: String,
    /// `pial:<uuid>` — the author as a name. Never a handle: a handle is a
    /// transferable pointer at an identity, not the identity.
    pub author_name: String,
    pub post_id: String,
    pub body: String,
    pub content_type: String,
    pub created_at: DateTime<Utc>,
    pub impressions: i64,
    pub likes: i64,
    pub reposts: i64,
    pub replies: i64,
    /// Derived from work_citations — see CreatorSummary::quotes.
    pub quotes: i64,
    pub saves: i64,
    pub dislikes: i64,
    pub view_time_secs: i64,
    pub engagement_pct: f64,
    pub link_clicks: i64, // future: not tracked yet
}

// ── Site-wide admin summary ───────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize, Default)]
pub struct SiteSummary {
    pub total_users: i64,
    pub total_posts: i64,
    pub total_impressions: i64,
    pub total_likes: i64,
    pub total_reposts: i64,
    pub total_comments: i64,
    pub new_users_today: i64,
    pub new_users_week: i64,
    pub new_posts_today: i64,
    pub new_posts_week: i64,
    pub active_users_day: i64, // distinct identities that published in last 24h
    pub active_users_week: i64,
}

// ── Query params ──────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct PeriodQuery {
    pub days: Option<i32>,       // 7 | 30 | 90 | 0=all-time; default 30
    pub sort_by: Option<String>, // impressions|likes|engagement|date; default impressions
    pub limit: Option<i64>,      // default 20
}
