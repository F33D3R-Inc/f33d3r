//! Astraon's reads.
//!
//! # Where these facts come from, and why that is wrong
//!
//! Every query below runs against `f33d3r_feed` — feed-engine's database. That
//! is a direct violation of the rule the naming plane enforces: no brain may
//! reference another brain's rows, only a NAME it resolves through Manhattan.
//! Astraon does not reference feed-engine's works by name; it reads the tables.
//!
//! It is declared here rather than hidden because the cost is already visible:
//! when feed-engine dropped `works.reply_count` and `works.quote_count`, nothing
//! told astraon. The only reason a query here did not break is that astraon was
//! still reading the *older* `posts`/`post_metrics` pair, which feed-engine had
//! already stopped writing — so the numbers were not wrong, they were zero.
//!
//! What this file fixes now:
//!   * reads move from the retired `posts`/`post_metrics` pair onto `works`,
//!     `work_reactions` and `work_citations`, which is where feed-engine writes;
//!   * every tally is a correlated count over row truth — the same shape
//!     feed-engine adopted in 0004 — so there is no cached counter to drift;
//!   * the author filter is `works.author_pial`, the work's own attribution,
//!     instead of a join into `users`, which is elohim-veni's table to own.
//!
//! What it does NOT fix, and what a full decoupling requires, is at the bottom
//! of this comment block:
//!
//!   1. `follows` — follower counts are keyed by `users.id`, and only `users`
//!      maps a PIAL to that id. Manhattan resolves a name to a Manhattan node,
//!      never to another brain's primary key, so this join cannot be replaced by
//!      a resolution. It has to be replaced by feed-engine answering "how many
//!      identities follow this name", over HTTP or on the event stream.
//!   2. `users` — the site summary counts registered identities and new signups.
//!      Astraon has no identities of its own to count.
//!   3. everything else here — `works`, `work_reactions`, `work_citations` are
//!      feed-engine's rows. The end state is astraon's own database, fed by the
//!      work/reaction/citation events on Sitra Achra, so that a schema change in
//!      feed-engine breaks a contract loudly instead of a query silently.

use chrono::{NaiveDate, Utc};
use sqlx::PgPool;
use uuid::Uuid;

use crate::error::Result;
use crate::models::*;

pub async fn migrate(_pool: &PgPool) -> anyhow::Result<()> {
    // Astraon owns no table in f33d3r_feed and must never create one there —
    // it is not astraon's database. When astraon gets its own, this is where
    // that schema is applied.
    Ok(())
}

// ── Derived tallies ───────────────────────────────────────────────────────────
// Row truth, counted at read time. `works.reply_count` and `works.quote_count`
// were dropped in feed-engine migration 0004 precisely because a cached copy of
// these numbers was allowed to be wrong; the correlated count over
// idx_work_citations_target_type is what replaced them.

const LIKES:   &str = "(SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id  = w.id AND wr.reaction_type = 'like')";
const REPOSTS: &str = "(SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id  = w.id AND wr.reaction_type = 'repost')";
const SAVES:   &str = "(SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id  = w.id AND wr.reaction_type = 'bookmark')";
const DISLIKES:&str = "(SELECT COUNT(*) FROM work_reactions wr WHERE wr.work_id  = w.id AND wr.reaction_type = 'dislike')";
const REPLIES: &str = "(SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type = 'reply')";
const QUOTES:  &str = "(SELECT COUNT(*) FROM work_citations wc WHERE wc.target_id = w.id AND wc.citation_type = 'quote')";

/// The work's content type, derived from the media the row actually carries.
///
/// `works.content_type` exists but is never written — every insert leaves it at
/// its 'text' default — so reading it would report the entire site as text and
/// the breakdown would be a single meaningless bucket. `works.kind` is the
/// structural kind (post|reply|vision|thread_part|quote|react_video), not the
/// media shape. The columns below are the only honest source.
const CONTENT_TYPE: &str = r#"
    CASE
        WHEN w.video_master_url IS NOT NULL AND w.video_master_url <> '' THEN 'video'
        WHEN w.voice_url        IS NOT NULL AND w.voice_url        <> '' THEN 'voice'
        WHEN w.poll_options     IS NOT NULL AND COALESCE(array_length(w.poll_options, 1), 0) > 0 THEN 'poll'
        WHEN COALESCE(array_length(w.media_urls, 1), 0) > 0 THEN 'image'
        ELSE 'text'
    END
"#;

/// A creator's own top-level, live works. `kind <> 'reply'` and
/// `deleted_at IS NULL` are the same predicate feed-engine reads through.
const AUTHORED: &str = "w.author_pial = $1 AND w.kind <> 'reply' AND w.deleted_at IS NULL";

/// Impressions have exactly one owner on a work: `works.view_count`.
const IMPRESSIONS: &str = "COALESCE(w.view_count, 0)";

fn period(days: i32) -> String {
    if days > 0 {
        format!("AND w.created_at > NOW() - INTERVAL '{days} days'")
    } else {
        String::new()
    }
}

// ── Creator summary ───────────────────────────────────────────────────────────

pub async fn get_creator_summary(pool: &PgPool, pial: Uuid, days: i32) -> Result<CreatorSummary> {
    let period_filter = period(days);

    let sql = format!(
        r#"
        SELECT
            COUNT(w.id)::BIGINT,
            COALESCE(SUM({IMPRESSIONS}), 0)::BIGINT,
            COALESCE(SUM({LIKES}),       0)::BIGINT,
            COALESCE(SUM({REPOSTS}),     0)::BIGINT,
            COALESCE(SUM({REPLIES}),     0)::BIGINT,
            COALESCE(SUM({QUOTES}),      0)::BIGINT,
            COALESCE(SUM({SAVES}),       0)::BIGINT
        FROM works w
        WHERE {AUTHORED}
          {period_filter}
        "#
    );

    let (total_posts, impressions, likes, reposts, replies, quotes, saves): (
        i64,
        i64,
        i64,
        i64,
        i64,
        i64,
        i64,
    ) = sqlx::query_as(&sql).bind(pial).fetch_one(pool).await?;

    let current_followers = count_followers(pool, pial).await?;
    let followers_gained = count_followers_since(pool, pial, days).await?;

    let engagement_rate = if impressions > 0 {
        (likes + reposts + replies) as f64 / impressions as f64 * 100.0
    } else {
        0.0
    };

    Ok(CreatorSummary {
        identity_name: manhattan_client::pial_name(&pial.to_string()),
        pial_id: pial.to_string(),
        period_days: days,
        total_posts,
        impressions,
        likes,
        reposts,
        replies,
        quotes,
        saves,
        profile_visits: 0,
        followers_gained,
        followers_lost: 0,
        net_followers: followers_gained,
        current_followers,
        engagement_rate,
        // No work carries watch seconds. The column this used to read,
        // posts.view_time_seconds, was an analytics stub that nothing ever
        // incremented, and `works` has no equivalent — astraon may not add one
        // to a database it does not own. Reported as 0 rather than guessed.
        view_time_secs: 0,
        impressions_pct_change: None,
        likes_pct_change: None,
        followers_pct_change: None,
    })
}

// ── Follower counts — the residual row reach ─────────────────────────────────
// `follows` is keyed by `users.id`, and `users` is the only table that maps a
// PIAL to that id. Manhattan resolves a name to a Manhattan node, never to
// another brain's primary key, so no resolution can stand in for this join.
// Removing it means feed-engine answering the question instead.

async fn count_followers(pool: &PgPool, pial: Uuid) -> Result<i64> {
    let n: i64 = sqlx::query_scalar(
        r#"
        SELECT COUNT(*)::BIGINT
        FROM follows f
        JOIN users u ON u.id = f.following_id
        WHERE u.pial_id = $1
        "#,
    )
    .bind(pial)
    .fetch_one(pool)
    .await?;
    Ok(n)
}

async fn count_followers_since(pool: &PgPool, pial: Uuid, days: i32) -> Result<i64> {
    if days <= 0 {
        return Ok(0);
    }
    let sql = format!(
        r#"
        SELECT COUNT(*)::BIGINT
        FROM follows f
        JOIN users u ON u.id = f.following_id
        WHERE u.pial_id = $1
          AND f.created_at > NOW() - INTERVAL '{days} days'
        "#
    );
    let n: i64 = sqlx::query_scalar(&sql).bind(pial).fetch_one(pool).await?;
    Ok(n)
}

// ── Work performance table ────────────────────────────────────────────────────

pub async fn get_creator_posts(
    pool: &PgPool,
    pial: Uuid,
    days: i32,
    sort_by: &str,
    limit: i64,
) -> Result<Vec<PostRow>> {
    let period_filter = period(days);

    let order = match sort_by {
        "likes" => "likes DESC",
        "reposts" => "reposts DESC",
        "engagement" => "engagement_pct DESC",
        "replies" => "replies DESC",
        "saves" => "saves DESC",
        "date" => "created_at DESC",
        _ => "impressions DESC",
    };

    let sql = format!(
        r#"
        SELECT
            t.*,
            CASE WHEN t.impressions > 0
                 THEN (t.likes + t.reposts + t.replies)::FLOAT8 / t.impressions::FLOAT8 * 100.0
                 ELSE 0.0
            END AS engagement_pct
        FROM (
            SELECT
                w.id::TEXT                        AS work_id,
                w.cid                             AS cid,
                LEFT(COALESCE(w.body, ''), 120)   AS body_preview,
                ({CONTENT_TYPE})                  AS content_type,
                w.created_at                      AS created_at,
                {IMPRESSIONS}::BIGINT             AS impressions,
                {LIKES}::BIGINT                   AS likes,
                {REPOSTS}::BIGINT                 AS reposts,
                {REPLIES}::BIGINT                 AS replies,
                {QUOTES}::BIGINT                  AS quotes,
                {SAVES}::BIGINT                   AS saves
            FROM works w
            WHERE {AUTHORED}
              {period_filter}
        ) t
        ORDER BY {order}
        LIMIT $2
        "#
    );

    let rows: Vec<(
        String,
        String,
        String,
        String,
        chrono::DateTime<Utc>,
        i64,
        i64,
        i64,
        i64,
        i64,
        i64,
        f64,
    )> = sqlx::query_as(&sql)
        .bind(pial)
        .bind(limit)
        .fetch_all(pool)
        .await?;

    let max_imp = rows.iter().map(|r| r.5).max().unwrap_or(1).max(1);
    let now = Utc::now();

    let posts = rows
        .into_iter()
        .map(
            |(
                work_id,
                cid,
                body_preview,
                content_type,
                created_at,
                impressions,
                likes,
                reposts,
                replies,
                quotes,
                saves,
                engagement_pct,
            )| {
                let secs = (now - created_at).num_seconds();
                let time_ago = if secs < 60 {
                    "now".into()
                } else if secs < 3600 {
                    format!("{}m", secs / 60)
                } else if secs < 86400 {
                    format!("{}h", secs / 3600)
                } else {
                    format!("{}d", secs / 86400)
                };
                PostRow {
                    work_name: manhattan_client::uuid_name(&work_id),
                    cid_name: manhattan_client::cid_name(&cid),
                    post_id: work_id,
                    body_preview,
                    content_type,
                    created_at,
                    time_ago,
                    impressions,
                    likes,
                    reposts,
                    replies,
                    quotes,
                    saves,
                    engagement_pct,
                    bar_pct: impressions as f64 / max_imp as f64 * 100.0,
                }
            },
        )
        .collect();

    Ok(posts)
}

// ── Timeline ──────────────────────────────────────────────────────────────────

pub async fn get_creator_timeline(pool: &PgPool, pial: Uuid, days: i32) -> Result<CreatorTimeline> {
    let actual_days = if days <= 0 { 30 } else { days };

    let sql = format!(
        r#"
        SELECT
            DATE(w.created_at)              AS day,
            COUNT(w.id)::BIGINT             AS works,
            COALESCE(SUM({IMPRESSIONS}), 0)::BIGINT AS impressions,
            COALESCE(SUM({LIKES}),       0)::BIGINT AS likes,
            COALESCE(SUM({REPOSTS}),     0)::BIGINT AS reposts,
            COALESCE(SUM({REPLIES}),     0)::BIGINT AS replies
        FROM works w
        WHERE {AUTHORED}
          AND w.created_at > NOW() - INTERVAL '{actual_days} days'
        GROUP BY DATE(w.created_at)
        ORDER BY day ASC
        "#
    );

    let rows: Vec<(NaiveDate, i64, i64, i64, i64, i64)> =
        sqlx::query_as(&sql).bind(pial).fetch_all(pool).await?;

    let mut max_impressions = 0i64;
    let mut max_engagement = 0f64;

    let points: Vec<TimelinePoint> = rows
        .into_iter()
        .map(|(day, posts, impressions, likes, reposts, replies)| {
            let eng = if impressions > 0 {
                (likes + reposts + replies) as f64 / impressions as f64 * 100.0
            } else {
                0.0
            };
            if impressions > max_impressions {
                max_impressions = impressions;
            }
            if eng > max_engagement {
                max_engagement = eng;
            }
            TimelinePoint {
                date: day,
                impressions,
                likes,
                reposts,
                replies,
                posts,
                engagement: eng,
            }
        })
        .collect();

    Ok(CreatorTimeline {
        points,
        period_days: actual_days,
        max_impressions: max_impressions.max(1),
        max_engagement: max_engagement.max(0.01),
    })
}

// ── Audience growth ───────────────────────────────────────────────────────────

pub async fn get_creator_audience(pool: &PgPool, pial: Uuid, days: i32) -> Result<CreatorAudience> {
    let actual_days = if days <= 0 { 30 } else { days };

    let total_followers = count_followers(pool, pial).await?;

    let sql = format!(
        r#"
        SELECT DATE(f.created_at) AS day, COUNT(*)::BIGINT AS new_followers
        FROM follows f
        JOIN users u ON u.id = f.following_id
        WHERE u.pial_id = $1
          AND f.created_at > NOW() - INTERVAL '{actual_days} days'
        GROUP BY DATE(f.created_at)
        ORDER BY day ASC
        "#
    );

    let rows: Vec<(NaiveDate, i64)> = sqlx::query_as(&sql).bind(pial).fetch_all(pool).await?;

    let points = rows
        .into_iter()
        .map(|(date, new_followers)| AudiencePoint {
            date,
            new_followers,
            unfollows: 0,
            net: new_followers,
        })
        .collect();

    Ok(CreatorAudience {
        points,
        total_followers,
        period_days: actual_days,
    })
}

// ── Content type breakdown ────────────────────────────────────────────────────

pub async fn get_creator_breakdown(
    pool: &PgPool,
    pial: Uuid,
    days: i32,
) -> Result<CreatorBreakdown> {
    let period_filter = period(days);

    let sql = format!(
        r#"
        SELECT
            ({CONTENT_TYPE})                        AS content_type,
            COUNT(w.id)::BIGINT                     AS works,
            COALESCE(SUM({IMPRESSIONS}), 0)::BIGINT AS impressions,
            CASE WHEN COALESCE(SUM({IMPRESSIONS}), 0) > 0
                 THEN (COALESCE(SUM({LIKES}), 0) + COALESCE(SUM({REPOSTS}), 0) + COALESCE(SUM({REPLIES}), 0))::FLOAT8
                      / COALESCE(SUM({IMPRESSIONS}), 1)::FLOAT8 * 100.0
                 ELSE 0.0
            END                                     AS avg_engagement
        FROM works w
        WHERE {AUTHORED}
          {period_filter}
        GROUP BY 1
        ORDER BY 2 DESC
        "#
    );

    let rows: Vec<(String, i64, i64, f64)> =
        sqlx::query_as(&sql).bind(pial).fetch_all(pool).await?;

    let total: i64 = rows.iter().map(|r| r.1).sum::<i64>().max(1);

    let types = rows
        .into_iter()
        .map(
            |(content_type, count, impressions, avg_engagement)| ContentTypeBreakdown {
                content_type,
                count,
                impressions,
                avg_engagement,
                pct_of_posts: count as f64 / total as f64 * 100.0,
            },
        )
        .collect();

    Ok(CreatorBreakdown { types })
}

// ── Single work detail ────────────────────────────────────────────────────────

pub async fn get_work_detail(pool: &PgPool, work_id: Uuid) -> Result<PostDetail> {
    let sql = format!(
        r#"
        SELECT
            w.id::TEXT,
            w.cid,
            w.author_pial::TEXT,
            w.body,
            ({CONTENT_TYPE}),
            w.created_at,
            {IMPRESSIONS}::BIGINT,
            {LIKES}::BIGINT,
            {REPOSTS}::BIGINT,
            {REPLIES}::BIGINT,
            {QUOTES}::BIGINT,
            {SAVES}::BIGINT,
            {DISLIKES}::BIGINT
        FROM works w
        WHERE w.id = $1
          AND w.deleted_at IS NULL
        "#
    );

    let row: Option<(
        String,
        String,
        String,
        String,
        String,
        chrono::DateTime<Utc>,
        i64,
        i64,
        i64,
        i64,
        i64,
        i64,
        i64,
    )> = sqlx::query_as(&sql)
        .bind(work_id)
        .fetch_optional(pool)
        .await?;

    let (
        id,
        cid,
        author_pial,
        body,
        content_type,
        created_at,
        impressions,
        likes,
        reposts,
        replies,
        quotes,
        saves,
        dislikes,
    ) = row.ok_or(crate::error::AstraonError::NotFound)?;

    let engagement_pct = if impressions > 0 {
        (likes + reposts + replies) as f64 / impressions as f64 * 100.0
    } else {
        0.0
    };

    Ok(PostDetail {
        work_name: manhattan_client::uuid_name(&id),
        cid_name: manhattan_client::cid_name(&cid),
        author_name: manhattan_client::pial_name(&author_pial),
        post_id: id,
        body,
        content_type,
        created_at,
        impressions,
        likes,
        reposts,
        replies,
        quotes,
        saves,
        dislikes,
        // Same absent fact as in the creator summary: no work carries watch
        // seconds, and astraon will not invent a column in another brain's
        // database to hold one.
        view_time_secs: 0,
        engagement_pct,
        link_clicks: 0,
    })
}

// ── Site-wide summary ─────────────────────────────────────────────────────────

pub async fn get_site_summary(pool: &PgPool) -> Result<SiteSummary> {
    // `users` appears twice here and nowhere else: total registered identities
    // and new signups are facts astraon cannot hold, because astraon does not
    // own identity. Everything else counts works and their edges.
    //
    // Active creators are counted by DISTINCT author_pial — an identity by its
    // name-value — rather than by DISTINCT author_id, which would be counting
    // another brain's primary keys.
    let row: (i64, i64, i64, i64, i64, i64, i64, i64, i64, i64, i64, i64) = sqlx::query_as(
        r#"
        SELECT
            (SELECT COUNT(*)::BIGINT FROM users),
            (SELECT COUNT(*)::BIGINT FROM works WHERE kind <> 'reply' AND deleted_at IS NULL),
            (SELECT COALESCE(SUM(view_count), 0)::BIGINT FROM works WHERE deleted_at IS NULL),
            (SELECT COUNT(*)::BIGINT FROM work_reactions WHERE reaction_type = 'like'),
            (SELECT COUNT(*)::BIGINT FROM work_reactions WHERE reaction_type = 'repost'),
            (SELECT COUNT(*)::BIGINT FROM work_citations WHERE citation_type = 'reply'),
            (SELECT COUNT(*)::BIGINT FROM users WHERE created_at > NOW() - INTERVAL '1 day'),
            (SELECT COUNT(*)::BIGINT FROM users WHERE created_at > NOW() - INTERVAL '7 days'),
            (SELECT COUNT(*)::BIGINT FROM works WHERE kind <> 'reply' AND deleted_at IS NULL AND created_at > NOW() - INTERVAL '1 day'),
            (SELECT COUNT(*)::BIGINT FROM works WHERE kind <> 'reply' AND deleted_at IS NULL AND created_at > NOW() - INTERVAL '7 days'),
            (SELECT COUNT(DISTINCT author_pial)::BIGINT FROM works WHERE deleted_at IS NULL AND created_at > NOW() - INTERVAL '1 day'),
            (SELECT COUNT(DISTINCT author_pial)::BIGINT FROM works WHERE deleted_at IS NULL AND created_at > NOW() - INTERVAL '7 days')
        "#,
    )
    .fetch_one(pool)
    .await?;

    Ok(SiteSummary {
        total_users: row.0,
        total_posts: row.1,
        total_impressions: row.2,
        total_likes: row.3,
        total_reposts: row.4,
        total_comments: row.5,
        new_users_today: row.6,
        new_users_week: row.7,
        new_posts_today: row.8,
        new_posts_week: row.9,
        active_users_day: row.10,
        active_users_week: row.11,
    })
}
