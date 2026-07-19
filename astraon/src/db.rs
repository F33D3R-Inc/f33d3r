use chrono::{NaiveDate, Utc};
use sqlx::PgPool;

use crate::error::Result;
use crate::models::*;

pub async fn migrate(_pool: &PgPool) -> anyhow::Result<()> {
    // Astraon is read-only against f33d3r_feed for now.
    Ok(())
}

// ── Creator summary ───────────────────────────────────────────────────────────

pub async fn get_creator_summary(pool: &PgPool, pial_id: &str, days: i32) -> Result<CreatorSummary> {
    let period_filter = if days > 0 {
        format!("AND p.created_at > NOW() - INTERVAL '{days} days'")
    } else {
        String::new()
    };

    let sql = format!(
        r#"
        SELECT
            COUNT(p.id)::BIGINT,
            COALESCE(SUM(m.impressions), 0)::BIGINT,
            COALESCE(SUM(m.likes),       0)::BIGINT,
            COALESCE(SUM(m.reposts),     0)::BIGINT,
            COALESCE(SUM(m.comments),    0)::BIGINT,
            COALESCE(SUM(m.saves),       0)::BIGINT,
            COALESCE(SUM(COALESCE(p.view_time_seconds,0)),0)::BIGINT
        FROM posts p
        JOIN users u ON u.id = p.author_id
        LEFT JOIN post_metrics m ON m.post_id = p.id
        WHERE u.pial_id::TEXT = $1
          AND p.is_reply = FALSE
          {period_filter}
        "#
    );

    let row: (i64, i64, i64, i64, i64, i64, i64) = sqlx::query_as(&sql)
        .bind(pial_id)
        .fetch_one(pool)
        .await?;

    let (total_posts, impressions, likes, reposts, replies, saves, view_time_secs) = row;

    // Follower count
    let current_followers: i64 = sqlx::query_scalar(
        "SELECT COUNT(*)::BIGINT FROM follows f JOIN users u ON u.id = f.following_id WHERE u.pial_id::TEXT = $1"
    )
    .bind(pial_id)
    .fetch_one(pool)
    .await
    .unwrap_or(0);

    let followers_gained: i64 = if days > 0 {
        let sql2 = format!(
            "SELECT COUNT(*)::BIGINT FROM follows f JOIN users u ON u.id = f.following_id WHERE u.pial_id::TEXT = $1 AND f.created_at > NOW() - INTERVAL '{days} days'"
        );
        sqlx::query_scalar(&sql2)
            .bind(pial_id)
            .fetch_one(pool)
            .await
            .unwrap_or(0)
    } else {
        0
    };

    let engagement_rate = if impressions > 0 {
        (likes + reposts + replies) as f64 / impressions as f64 * 100.0
    } else {
        0.0
    };

    Ok(CreatorSummary {
        pial_id: pial_id.to_string(),
        period_days: days,
        total_posts,
        impressions,
        likes,
        reposts,
        replies,
        saves,
        profile_visits: 0,
        followers_gained,
        followers_lost: 0,
        net_followers: followers_gained,
        current_followers,
        engagement_rate,
        view_time_secs,
        impressions_pct_change: None,
        likes_pct_change: None,
        followers_pct_change: None,
    })
}

// ── Post performance table ────────────────────────────────────────────────────

pub async fn get_creator_posts(pool: &PgPool, pial_id: &str, days: i32, sort_by: &str, limit: i64) -> Result<Vec<PostRow>> {
    let period_filter = if days > 0 {
        format!("AND p.created_at > NOW() - INTERVAL '{days} days'")
    } else {
        String::new()
    };

    let order = match sort_by {
        "likes"      => "COALESCE(m.likes,0) DESC",
        "reposts"    => "COALESCE(m.reposts,0) DESC",
        "engagement" => "eng_pct DESC",
        "replies"    => "COALESCE(m.comments,0) DESC",
        "saves"      => "COALESCE(m.saves,0) DESC",
        "date"       => "p.created_at DESC",
        _            => "COALESCE(m.impressions,0) DESC",
    };

    let sql = format!(
        r#"
        SELECT
            p.id::TEXT,
            LEFT(COALESCE(p.body,''), 120),
            p.content_type,
            p.created_at,
            COALESCE(m.impressions, 0)::BIGINT,
            COALESCE(m.likes,       0)::BIGINT,
            COALESCE(m.reposts,     0)::BIGINT,
            COALESCE(m.comments,    0)::BIGINT,
            COALESCE(m.saves,       0)::BIGINT,
            CASE WHEN COALESCE(m.impressions,0) > 0
                 THEN (COALESCE(m.likes,0)+COALESCE(m.reposts,0)+COALESCE(m.comments,0))::FLOAT8
                      / COALESCE(m.impressions,1)::FLOAT8 * 100.0
                 ELSE 0.0
            END AS eng_pct
        FROM posts p
        JOIN users u ON u.id = p.author_id
        LEFT JOIN post_metrics m ON m.post_id = p.id
        WHERE u.pial_id::TEXT = $1
          AND p.is_reply = FALSE
          {period_filter}
        ORDER BY {order}
        LIMIT $2
        "#
    );

    let rows: Vec<(String, String, String, chrono::DateTime<Utc>, i64, i64, i64, i64, i64, f64)> =
        sqlx::query_as(&sql)
            .bind(pial_id)
            .bind(limit)
            .fetch_all(pool)
            .await?;

    let max_imp = rows.iter().map(|r| r.4).max().unwrap_or(1).max(1);
    let now = Utc::now();

    let posts = rows.into_iter().map(|(post_id, body_preview, content_type, created_at, impressions, likes, reposts, replies, saves, engagement_pct)| {
        let secs = (now - created_at).num_seconds();
        let time_ago = if secs < 60        { "now".into() }
            else if secs < 3600  { format!("{}m", secs / 60) }
            else if secs < 86400 { format!("{}h", secs / 3600) }
            else                 { format!("{}d", secs / 86400) };
        PostRow {
            post_id,
            body_preview,
            content_type,
            created_at,
            time_ago,
            impressions,
            likes,
            reposts,
            replies,
            saves,
            engagement_pct,
            bar_pct: impressions as f64 / max_imp as f64 * 100.0,
        }
    }).collect();

    Ok(posts)
}

// ── Timeline ──────────────────────────────────────────────────────────────────

pub async fn get_creator_timeline(pool: &PgPool, pial_id: &str, days: i32) -> Result<CreatorTimeline> {
    let actual_days = if days <= 0 { 30 } else { days };

    let sql = format!(
        r#"
        SELECT
            DATE(p.created_at)                          AS day,
            COUNT(p.id)::BIGINT                         AS posts,
            COALESCE(SUM(m.impressions), 0)::BIGINT     AS impressions,
            COALESCE(SUM(m.likes),       0)::BIGINT     AS likes,
            COALESCE(SUM(m.reposts),     0)::BIGINT     AS reposts,
            COALESCE(SUM(m.comments),    0)::BIGINT     AS replies
        FROM posts p
        JOIN users u ON u.id = p.author_id
        LEFT JOIN post_metrics m ON m.post_id = p.id
        WHERE u.pial_id::TEXT = $1
          AND p.is_reply = FALSE
          AND p.created_at > NOW() - INTERVAL '{actual_days} days'
        GROUP BY DATE(p.created_at)
        ORDER BY day ASC
        "#
    );

    let rows: Vec<(NaiveDate, i64, i64, i64, i64, i64)> = sqlx::query_as(&sql)
        .bind(pial_id)
        .fetch_all(pool)
        .await?;

    let mut max_impressions = 0i64;
    let mut max_engagement  = 0f64;

    let points: Vec<TimelinePoint> = rows.into_iter().map(|(day, posts, impressions, likes, reposts, replies)| {
        let eng = if impressions > 0 {
            (likes + reposts + replies) as f64 / impressions as f64 * 100.0
        } else { 0.0 };
        if impressions > max_impressions { max_impressions = impressions; }
        if eng > max_engagement { max_engagement = eng; }
        TimelinePoint { date: day, impressions, likes, reposts, replies, posts, engagement: eng }
    }).collect();

    Ok(CreatorTimeline {
        points,
        period_days: actual_days,
        max_impressions: max_impressions.max(1),
        max_engagement: max_engagement.max(0.01),
    })
}

// ── Audience growth ───────────────────────────────────────────────────────────

pub async fn get_creator_audience(pool: &PgPool, pial_id: &str, days: i32) -> Result<CreatorAudience> {
    let actual_days = if days <= 0 { 30 } else { days };

    let total_followers: i64 = sqlx::query_scalar(
        "SELECT COUNT(*)::BIGINT FROM follows f JOIN users u ON u.id = f.following_id WHERE u.pial_id::TEXT = $1"
    )
    .bind(pial_id)
    .fetch_one(pool)
    .await
    .unwrap_or(0);

    let sql = format!(
        r#"
        SELECT DATE(f.created_at) AS day, COUNT(*)::BIGINT AS new_followers
        FROM follows f
        JOIN users u ON u.id = f.following_id
        WHERE u.pial_id::TEXT = $1
          AND f.created_at > NOW() - INTERVAL '{actual_days} days'
        GROUP BY DATE(f.created_at)
        ORDER BY day ASC
        "#
    );

    let rows: Vec<(NaiveDate, i64)> = sqlx::query_as(&sql)
        .bind(pial_id)
        .fetch_all(pool)
        .await?;

    let points = rows.into_iter().map(|(date, new_followers)| AudiencePoint {
        date, new_followers, unfollows: 0, net: new_followers,
    }).collect();

    Ok(CreatorAudience { points, total_followers, period_days: actual_days })
}

// ── Content type breakdown ────────────────────────────────────────────────────

pub async fn get_creator_breakdown(pool: &PgPool, pial_id: &str, days: i32) -> Result<CreatorBreakdown> {
    let period_filter = if days > 0 {
        format!("AND p.created_at > NOW() - INTERVAL '{days} days'")
    } else {
        String::new()
    };

    let sql = format!(
        r#"
        SELECT
            p.content_type,
            COUNT(p.id)::BIGINT,
            COALESCE(SUM(m.impressions), 0)::BIGINT,
            CASE WHEN COALESCE(SUM(m.impressions),0) > 0
                 THEN (COALESCE(SUM(m.likes),0)+COALESCE(SUM(m.reposts),0)+COALESCE(SUM(m.comments),0))::FLOAT8
                      / COALESCE(SUM(m.impressions),1)::FLOAT8 * 100.0
                 ELSE 0.0
            END
        FROM posts p
        JOIN users u ON u.id = p.author_id
        LEFT JOIN post_metrics m ON m.post_id = p.id
        WHERE u.pial_id::TEXT = $1
          AND p.is_reply = FALSE
          {period_filter}
        GROUP BY p.content_type
        ORDER BY COUNT(p.id) DESC
        "#
    );

    let rows: Vec<(String, i64, i64, f64)> = sqlx::query_as(&sql)
        .bind(pial_id)
        .fetch_all(pool)
        .await?;

    let total: i64 = rows.iter().map(|r| r.1).sum::<i64>().max(1);

    let types = rows.into_iter().map(|(content_type, count, impressions, avg_engagement)| {
        ContentTypeBreakdown {
            content_type,
            count,
            impressions,
            avg_engagement,
            pct_of_posts: count as f64 / total as f64 * 100.0,
        }
    }).collect();

    Ok(CreatorBreakdown { types })
}

// ── Single post detail ────────────────────────────────────────────────────────

pub async fn get_post_detail(pool: &PgPool, post_id: &str) -> Result<PostDetail> {
    let row: Option<(String, String, String, chrono::DateTime<Utc>, i64, i64, i64, i64, i64, i64, i64)> =
        sqlx::query_as(
            r#"
            SELECT
                p.id::TEXT, p.body, p.content_type, p.created_at,
                COALESCE(m.impressions, 0)::BIGINT,
                COALESCE(m.likes,       0)::BIGINT,
                COALESCE(m.reposts,     0)::BIGINT,
                COALESCE(m.comments,    0)::BIGINT,
                COALESCE(m.saves,       0)::BIGINT,
                COALESCE(m.dislikes,    0)::BIGINT,
                COALESCE(p.view_time_seconds, 0)::BIGINT
            FROM posts p
            LEFT JOIN post_metrics m ON m.post_id = p.id
            WHERE p.id::TEXT = $1
            "#
        )
        .bind(post_id)
        .fetch_optional(pool)
        .await?;

    let (post_id, body, content_type, created_at, impressions, likes, reposts, replies, saves, dislikes, view_time_secs) =
        row.ok_or(crate::error::AstraonError::NotFound)?;

    let engagement_pct = if impressions > 0 {
        (likes + reposts + replies) as f64 / impressions as f64 * 100.0
    } else { 0.0 };

    Ok(PostDetail { post_id, body, content_type, created_at, impressions, likes, reposts, replies, saves, dislikes, view_time_secs, engagement_pct, link_clicks: 0 })
}

// ── Site-wide summary ─────────────────────────────────────────────────────────

pub async fn get_site_summary(pool: &PgPool) -> Result<SiteSummary> {
    let row: (i64,i64,i64,i64,i64,i64,i64,i64,i64,i64,i64,i64) = sqlx::query_as(r#"
        SELECT
            (SELECT COUNT(*)::BIGINT FROM users),
            (SELECT COUNT(*)::BIGINT FROM posts WHERE is_reply = FALSE),
            (SELECT COALESCE(SUM(impressions),0)::BIGINT FROM post_metrics),
            (SELECT COALESCE(SUM(likes),0)::BIGINT FROM post_metrics),
            (SELECT COALESCE(SUM(reposts),0)::BIGINT FROM post_metrics),
            (SELECT COALESCE(SUM(comments),0)::BIGINT FROM post_metrics),
            (SELECT COUNT(*)::BIGINT FROM users WHERE created_at > NOW() - INTERVAL '1 day'),
            (SELECT COUNT(*)::BIGINT FROM users WHERE created_at > NOW() - INTERVAL '7 days'),
            (SELECT COUNT(*)::BIGINT FROM posts WHERE is_reply=FALSE AND created_at > NOW() - INTERVAL '1 day'),
            (SELECT COUNT(*)::BIGINT FROM posts WHERE is_reply=FALSE AND created_at > NOW() - INTERVAL '7 days'),
            (SELECT COUNT(DISTINCT author_id)::BIGINT FROM posts WHERE created_at > NOW() - INTERVAL '1 day'),
            (SELECT COUNT(DISTINCT author_id)::BIGINT FROM posts WHERE created_at > NOW() - INTERVAL '7 days')
    "#).fetch_one(pool).await?;

    Ok(SiteSummary {
        total_users: row.0, total_posts: row.1,
        total_impressions: row.2, total_likes: row.3,
        total_reposts: row.4, total_comments: row.5,
        new_users_today: row.6, new_users_week: row.7,
        new_posts_today: row.8, new_posts_week: row.9,
        active_users_day: row.10, active_users_week: row.11,
    })
}
