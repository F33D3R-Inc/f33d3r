/// Bot heuristic detection based on account age and publishing volume.
use chrono::Utc;
use deadpool_postgres::Pool;

pub struct BotResult {
    pub is_suspected_bot: bool,
    pub signals: Vec<String>,
}

pub async fn detect(pool: &Pool, pial_id: &str) -> BotResult {
    let Some((created_at, content_count)) =
        crate::db::get_account_age_and_content_count(pool, pial_id).await
    else {
        return BotResult {
            is_suspected_bot: false,
            signals: vec![],
        };
    };

    let age_secs = (Utc::now() - created_at).num_seconds().max(0);
    let age_days = age_secs / 86400;
    let age_hours = age_secs / 3600;

    let mut signals = Vec::new();
    let mut is_suspected_bot = false;

    // New account burst: <1 day old AND >5 items published
    if age_hours < 24 && content_count > 5 {
        signals.push("bot_new_account_burst".to_string());
        is_suspected_bot = true;
    }
    // New account high volume: <3 days old AND >15 items published
    if age_days < 3 && content_count > 15 {
        signals.push("bot_new_account_high_volume".to_string());
        is_suspected_bot = true;
    }

    BotResult {
        is_suspected_bot,
        signals,
    }
}
