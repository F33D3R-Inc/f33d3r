use deadpool_postgres::Pool;
use tracing::{error, info};

/// Ensure the post_rank_signals table exists and the posts table has the
/// rank_score / score_band columns. Safe to call on every startup.
pub async fn ensure_schema(pool: &Pool) -> Result<(), Box<dyn std::error::Error>> {
    let client = pool.get().await?;

    // Signals table — each row is one discrete signal event.
    client
        .execute(
            "CREATE TABLE IF NOT EXISTS post_rank_signals (
                post_id      UUID        NOT NULL,
                signal_type  TEXT        NOT NULL,
                pial_id      UUID        NOT NULL,
                occurred_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                weight       FLOAT8      NOT NULL DEFAULT 1.0,
                PRIMARY KEY (post_id, signal_type, pial_id, occurred_at)
            )",
            &[],
        )
        .await?;

    client
        .execute(
            "CREATE INDEX IF NOT EXISTS idx_post_rank_signals_post_id
             ON post_rank_signals (post_id)",
            &[],
        )
        .await?;

    // Extend posts table with ranking columns if not already present.
    client
        .execute(
            "ALTER TABLE posts
             ADD COLUMN IF NOT EXISTS rank_score FLOAT8 NOT NULL DEFAULT 0.0",
            &[],
        )
        .await?;

    client
        .execute(
            "ALTER TABLE posts
             ADD COLUMN IF NOT EXISTS score_band TEXT NOT NULL DEFAULT 'fading'",
            &[],
        )
        .await?;

    info!("Schema ready (post_rank_signals, rank_score, score_band)");
    Ok(())
}

/// Insert one ranking signal row.
pub async fn record_signal(
    pool: &Pool,
    post_id: &str,
    signal_type: &str,
    pial_id: &str,
    weight: f64,
) -> Result<(), Box<dyn std::error::Error>> {
    let client = pool.get().await?;
    client
        .execute(
            "INSERT INTO post_rank_signals
             (post_id, signal_type, pial_id, weight, occurred_at)
             VALUES ($1::uuid, $2, $3::uuid, $4, NOW())",
            &[&post_id, &signal_type, &pial_id, &weight],
        )
        .await?;
    Ok(())
}

/// Recompute rank_score and score_band for a post using signals from the last
/// 48 hours, then write the result back to posts.
///
/// Weight map (mirrors the signal weights assigned in pipeline.rs):
///   post_created  +0.5
///   like          +1.0
///   repost        +3.0
///   view          +0.1
///   unlike        -0.5
///   unrepost      -1.0
///
/// Score bands:
///   > 50  → trending
///   > 20  → rising
///   > 5   → steady
///   else  → fading
pub async fn update_post_score(
    pool: &Pool,
    post_id: &str,
) -> Result<(), Box<dyn std::error::Error>> {
    let client = pool.get().await?;

    let row = client
        .query_one(
            "SELECT COALESCE(SUM(weight), 0.0) AS total
             FROM post_rank_signals
             WHERE post_id = $1::uuid
               AND occurred_at >= NOW() - INTERVAL '48 hours'",
            &[&post_id],
        )
        .await?;

    let score: f64 = row.get::<_, f64>("total");

    let band = if score > 50.0 {
        "trending"
    } else if score > 20.0 {
        "rising"
    } else if score > 5.0 {
        "steady"
    } else {
        "fading"
    };

    let updated = client
        .execute(
            "UPDATE posts SET rank_score = $1, score_band = $2
             WHERE id = $3::uuid",
            &[&score, &band, &post_id],
        )
        .await?;

    if updated == 0 {
        // Post may not exist yet (race) or has been deleted — not an error.
        error!(
            post_id = %post_id,
            "update_post_score: no rows updated — post not found"
        );
    }

    Ok(())
}
