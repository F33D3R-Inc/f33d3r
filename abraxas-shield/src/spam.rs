/// Rate-based spam detection.
/// Queries the posts table via the author's PIAL UUID.
use deadpool_postgres::Pool;

#[allow(dead_code)]
pub struct SpamResult {
    pub is_spam: bool,
    pub signals: Vec<String>,
    pub post_rate_1h: i64,
}

pub async fn detect(pool: &Pool, pial_id: &str, body: &str) -> SpamResult {
    let rate_1h = crate::db::count_posts_by_pial_last_hours(pool, pial_id, 1).await;
    let has_dupe_body = crate::db::has_duplicate_body(pool, pial_id, body).await;

    let mut signals = Vec::new();
    let mut is_spam = false;

    if rate_1h > 20 {
        signals.push("spam_rate_high".to_string());
        is_spam = true;
    } else if rate_1h > 10 {
        signals.push("spam_rate_moderate".to_string());
        is_spam = true;
    }

    if has_dupe_body {
        signals.push("spam_duplicate_body".to_string());
        is_spam = true;
    }

    SpamResult {
        is_spam,
        signals,
        post_rate_1h: rate_1h,
    }
}
