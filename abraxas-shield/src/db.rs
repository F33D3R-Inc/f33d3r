use deadpool_postgres::Pool;
use tracing::{error, warn};

/// All data from the posts + users join needed to run the scan pipeline.
#[allow(dead_code)]
pub struct PostScanRecord {
    pub post_id: String,
    pub pial_id: String,
    pub body: String,
    pub is_adult_creator: bool,
    pub author_handle: String,
}

/// Source table from which a PostScanRecord was resolved.
/// Determines which table Abraxas writes scan results back to.
#[derive(Debug, Clone, PartialEq)]
pub enum ContentSource {
    /// Content lives in the `works` table (Malkuth signed-work path).
    Works,
    /// Content lives in the legacy `posts` table.
    Posts,
}

/// Full output of the shield pipeline, written to content_scan_results.
pub struct ScanResult {
    pub post_id: String,
    pub scan_version: String,
    pub nudity_score: f64,
    pub gore_score: f64,
    pub clickbait_score: f64,
    pub hate_signals: Vec<String>,
    pub violence_signals: Vec<String>,
    pub self_harm_signals: Vec<String>,
    pub risk_level: String,
    pub recommendation: String,
    pub signals: Vec<String>,
    pub is_duplicate: bool,
    pub duplicate_type: String,
    pub original_post_id: String,
}

/// Fetch content data needed for scanning.
/// When `work_id` is non-empty, queries the `works` table first.
/// Falls back to the legacy `posts` table when `work_id` is empty or not found.
/// Returns None if neither table has the content (deleted or never existed).
/// Also returns the ContentSource so callers know where to write results.
pub async fn get_post_for_scan(
    pool: &Pool,
    post_id: &str,
    work_id: &str,
) -> Option<(PostScanRecord, ContentSource)> {
    let client = pool.get().await.map_err(|e| {
        error!(post_id, err = %e, "db pool error in get_post_for_scan");
    }).ok()?;

    // Try works table first when work_id is provided.
    if !work_id.is_empty() {
        let row = client
            .query_opt(
                "SELECT
                    w.id::TEXT,
                    w.author_pial::TEXT,
                    w.body,
                    COALESCE(pr.is_adult_creator, FALSE),
                    u.handle
                 FROM works w
                 JOIN users u ON u.id = w.author_id
                 LEFT JOIN user_profiles pr ON pr.user_id = w.author_id
                 WHERE w.id = $1::uuid
                   AND w.deleted_at IS NULL",
                &[&work_id],
            )
            .await
            .map_err(|e| {
                error!(work_id, err = %e, "query error in get_post_for_scan (works)");
            })
            .ok()
            .flatten();

        if let Some(row) = row {
            return Some((
                PostScanRecord {
                    post_id: row.get(0),
                    pial_id: row.get(1),
                    body: row.get(2),
                    is_adult_creator: row.get(3),
                    author_handle: row.get(4),
                },
                ContentSource::Works,
            ));
        }

        // work_id given but not found — do not fall through to posts; the content doesn't exist.
        warn!(work_id, "Work not found or deleted in works table, skipping");
        return None;
    }

    // Legacy path: query the posts table.
    let row = client
        .query_opt(
            "SELECT
                p.id::TEXT,
                COALESCE(u.pial_id::TEXT, ''),
                p.body,
                COALESCE(pr.is_adult_creator, FALSE),
                u.handle
             FROM posts p
             JOIN users u ON u.id = p.author_id
             LEFT JOIN user_profiles pr ON pr.user_id = p.author_id
             WHERE p.id = $1::uuid
               AND p.deleted_at IS NULL",
            &[&post_id],
        )
        .await
        .map_err(|e| {
            error!(post_id, err = %e, "query error in get_post_for_scan (posts)");
        })
        .ok()??;

    Some((
        PostScanRecord {
            post_id: row.get(0),
            pial_id: row.get(1),
            body: row.get(2),
            is_adult_creator: row.get(3),
            author_handle: row.get(4),
        },
        ContentSource::Posts,
    ))
}

/// Check whether a hash is in the banned_content_hashes registry.
/// Returns Some(category) if banned, None if not found.
/// Called in Phase 3.1 when media hashes are available from the content event.
#[allow(dead_code)]
pub async fn is_banned_hash(pool: &Pool, hash_type: &str, hash_value: &str) -> Option<String> {
    let client = pool.get().await.map_err(|e| {
        error!(err = %e, "db pool error in is_banned_hash");
    }).ok()?;

    let row = client
        .query_opt(
            "SELECT category FROM banned_content_hashes
             WHERE hash_type = $1 AND hash_value = $2
             LIMIT 1",
            &[&hash_type, &hash_value],
        )
        .await
        .map_err(|e| {
            error!(err = %e, "query error in is_banned_hash");
        })
        .ok()??;

    Some(row.get(0))
}

/// Upsert scan result into content_scan_results.
/// Matches the exact schema from the existing table (no violence_signals / self_harm_signals
/// columns — those signals are included in the hate_signals and signals arrays respectively).
pub async fn store_scan_result(pool: &Pool, result: &ScanResult) -> Result<(), String> {
    let client = pool.get().await.map_err(|e| {
        format!("db pool error: {e}")
    })?;

    // Merge all toxicity signals into a combined list for the hate_signals column.
    // violence and self_harm signals are included in the top-level signals array.
    let mut hate_and_tox: Vec<String> = result.hate_signals.clone();
    hate_and_tox.extend(result.violence_signals.iter().cloned());
    hate_and_tox.extend(result.self_harm_signals.iter().cloned());

    client
        .execute(
            "INSERT INTO content_scan_results
               (post_id, scan_version, nudity_score, gore_score, clickbait_score,
                ocr_text, transcript, hate_signals, risk_level, recommendation, signals,
                is_duplicate, duplicate_type, original_post_id, scanned_at)
             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NOW())
             ON CONFLICT (post_id) DO UPDATE SET
               scan_version     = EXCLUDED.scan_version,
               nudity_score     = EXCLUDED.nudity_score,
               gore_score       = EXCLUDED.gore_score,
               clickbait_score  = EXCLUDED.clickbait_score,
               ocr_text         = EXCLUDED.ocr_text,
               transcript       = EXCLUDED.transcript,
               hate_signals     = EXCLUDED.hate_signals,
               risk_level       = EXCLUDED.risk_level,
               recommendation   = EXCLUDED.recommendation,
               signals          = EXCLUDED.signals,
               is_duplicate     = EXCLUDED.is_duplicate,
               duplicate_type   = EXCLUDED.duplicate_type,
               original_post_id = EXCLUDED.original_post_id,
               scanned_at       = NOW()",
            &[
                &result.post_id,
                &result.scan_version,
                &result.nudity_score,
                &result.gore_score,
                &result.clickbait_score,
                &"",           // ocr_text — Phase 3.1
                &"",           // transcript — Phase 3.1
                &hate_and_tox,
                &result.risk_level,
                &result.recommendation,
                &result.signals,
                &result.is_duplicate,
                &result.duplicate_type,
                &result.original_post_id,
            ],
        )
        .await
        .map_err(|e| format!("store_scan_result exec error: {e}"))?;

    Ok(())
}

/// Write per-signal scores for a work to the `work_scores` table.
/// The `work_scores` schema: PRIMARY KEY (work_id, signal), columns: score, updated_at.
/// Called in addition to store_scan_result when the content came from the works table.
pub async fn store_work_scores(
    pool: &Pool,
    work_id: &str,
    result: &ScanResult,
) -> Result<(), String> {
    let client = pool.get().await.map_err(|e| format!("db pool error: {e}"))?;

    // Upsert each scored dimension into work_scores.
    let scores: &[(&str, f64)] = &[
        ("nudity",     result.nudity_score),
        ("gore",       result.gore_score),
        ("clickbait",  result.clickbait_score),
    ];

    for (signal, score) in scores {
        client
            .execute(
                "INSERT INTO work_scores (work_id, signal, score, updated_at)
                 VALUES ($1::uuid, $2, $3, NOW())
                 ON CONFLICT (work_id, signal) DO UPDATE SET
                   score      = EXCLUDED.score,
                   updated_at = NOW()",
                &[&work_id, signal, score],
            )
            .await
            .map_err(|e| format!("store_work_scores upsert error ({signal}): {e}"))?;
    }

    Ok(())
}

/// Transition scan_state on the correct content table (works or posts) and append a
/// row to content_moderation_log.
///
/// Both `works` and `posts` have a `scan_state` column. This function updates it on
/// the correct table, applies `is_blocked`/`is_nsfw` side-effects where required, and
/// always writes a content_moderation_log row for a complete audit trail.
///
/// For posts-table content this replicates the exact SQL from dbpkg.SetPostScanState in Go.
pub async fn set_post_scan_state(
    pool: &Pool,
    post_id: &str,
    state: &str,
    reason: &str,
    source: &crate::db::ContentSource,
) -> Result<(), String> {
    let client = pool.get().await.map_err(|e| format!("db pool error: {e}"))?;

    match source {
        ContentSource::Works => {
            // Read current scan_state for the moderation log's from_state.
            let row = client
                .query_opt(
                    "SELECT scan_state FROM works WHERE id = $1::uuid",
                    &[&post_id],
                )
                .await
                .map_err(|e| format!("get works scan_state error: {e}"))?;

            let from_state: String = row
                .map(|r| r.get::<_, Option<String>>(0).unwrap_or_default())
                .unwrap_or_default();

            // Update scan_state on works.
            client
                .execute(
                    "UPDATE works SET scan_state = $1 WHERE id = $2::uuid",
                    &[&state, &post_id],
                )
                .await
                .map_err(|e| format!("update works scan_state error: {e}"))?;

            // Blocked content: also set is_blocked = TRUE.
            if state == "blocked" {
                client
                    .execute(
                        "UPDATE works SET is_blocked = TRUE WHERE id = $1::uuid",
                        &[&post_id],
                    )
                    .await
                    .map_err(|e| format!("update works is_blocked error: {e}"))?;
            }

            // Age-gated content: also set is_nsfw = TRUE.
            if state == "age_gate" || state == "age_gated" {
                client
                    .execute(
                        "UPDATE works SET is_nsfw = TRUE WHERE id = $1::uuid",
                        &[&post_id],
                    )
                    .await
                    .map_err(|e| format!("update works is_nsfw error: {e}"))?;
            }

            // Append moderation log — fire-and-forget on error.
            if let Err(e) = client
                .execute(
                    "INSERT INTO content_moderation_log
                       (post_id, from_state, to_state, reason, actor, created_at)
                     VALUES ($1, $2, $3, $4, 'abraxas-shield', NOW())",
                    &[&post_id, &from_state, &state, &reason],
                )
                .await
            {
                warn!(post_id, err = %e, "moderation log insert failed (non-fatal)");
            }
        }
        ContentSource::Posts => {
            // Read current state first (for the log).
            let row = client
                .query_opt(
                    "SELECT scan_state FROM posts WHERE id = $1::uuid",
                    &[&post_id],
                )
                .await
                .map_err(|e| format!("get scan_state error: {e}"))?;

            let from_state: String = row
                .map(|r| r.get::<_, Option<String>>(0).unwrap_or_default())
                .unwrap_or_default();

            client
                .execute(
                    "UPDATE posts SET scan_state = $1 WHERE id = $2::uuid",
                    &[&state, &post_id],
                )
                .await
                .map_err(|e| format!("update scan_state error: {e}"))?;

            // Append moderation log — fire-and-forget on error.
            if let Err(e) = client
                .execute(
                    "INSERT INTO content_moderation_log
                       (post_id, from_state, to_state, reason, actor, created_at)
                     VALUES ($1, $2, $3, $4, 'abraxas-shield', NOW())",
                    &[&post_id, &from_state, &state, &reason],
                )
                .await
            {
                warn!(post_id, err = %e, "moderation log insert failed (non-fatal)");
            }
        }
    }

    Ok(())
}

/// Set is_nsfw = true when content is age_gated.
/// Updates `works.is_nsfw` for works-table content, `posts.is_nsfw` for legacy posts.
pub async fn set_post_nsfw(
    pool: &Pool,
    post_id: &str,
    source: &crate::db::ContentSource,
) -> Result<(), String> {
    let client = pool.get().await.map_err(|e| format!("db pool error: {e}"))?;

    let table = match source {
        ContentSource::Works => "works",
        ContentSource::Posts => "posts",
    };

    client
        .execute(
            &format!("UPDATE {table} SET is_nsfw = TRUE WHERE id = $1::uuid"),
            &[&post_id],
        )
        .await
        .map_err(|e| format!("set_post_nsfw error ({table}): {e}"))?;

    Ok(())
}

/// Return user IDs of all admin users (role = 'admin').
pub async fn get_admin_user_ids(pool: &Pool) -> Vec<String> {
    let client = match pool.get().await {
        Ok(c) => c,
        Err(e) => {
            error!(err = %e, "db pool error in get_admin_user_ids");
            return vec![];
        }
    };

    let rows = match client
        .query("SELECT id::TEXT FROM users WHERE role = 'admin'", &[])
        .await
    {
        Ok(r) => r,
        Err(e) => {
            error!(err = %e, "query error in get_admin_user_ids");
            return vec![];
        }
    };

    rows.iter()
        .filter_map(|r| r.get::<_, Option<String>>(0))
        .collect()
}

/// Insert a moderation notification for an admin user.
/// Matches the exact schema of CreateNotification in Go:
///   INSERT INTO notifications (user_id, type, actor_id, target_id, target_type)
pub async fn notify_user(
    pool: &Pool,
    user_id: &str,
    notif_type: &str,
    target_id: &str,
) -> Result<(), String> {
    let client = pool.get().await.map_err(|e| format!("db pool error: {e}"))?;

    client
        .execute(
            "INSERT INTO notifications (user_id, type, actor_id, target_id, target_type)
             VALUES ($1, $2, NULL, $3, 'post')",
            &[&user_id, &notif_type, &target_id],
        )
        .await
        .map_err(|e| format!("notify_user error: {e}"))?;

    Ok(())
}

/// A signal's accuracy as recorded by admin moderation decisions.
pub struct SignalFeedback {
    pub signal:          String,
    pub true_positives:  i64,
    pub false_positives: i64,
    pub total:           i64,
}

/// Load per-signal feedback from admin moderation decisions.
/// Returns signals with high false-positive rates that Shield should suppress.
/// A signal is suppressed when: total >= 3 AND false_positive_rate >= 0.70
pub async fn load_suppressed_signals(pool: &Pool) -> Vec<String> {
    let client = match pool.get().await {
        Ok(c) => c,
        Err(_) => return vec![],
    };
    let rows = match client.query(
        "SELECT unnest(signals) AS signal,
                COUNT(*) FILTER (WHERE admin_decision = 'block')   AS tp,
                COUNT(*) FILTER (WHERE admin_decision = 'approve') AS fp,
                COUNT(*)                                           AS total
         FROM scan_feedback
         GROUP BY signal
         HAVING COUNT(*) >= 3",
        &[],
    ).await {
        Ok(r) => r,
        Err(_) => return vec![],
    };

    rows.iter()
        .filter_map(|row| {
            let signal: String = row.get(0);
            let tp: i64 = row.get(1);
            let fp: i64 = row.get(2);
            let total: i64 = row.get(3);
            let fp_rate = fp as f64 / total as f64;
            // Suppress only if: not reinforced (tp < fp) AND fp_rate >= 70%
            if fp_rate >= 0.70 && fp > tp {
                Some(signal)
            } else {
                None
            }
        })
        .collect()
}

/// Count posts created by a PIAL in the last N hours.
/// posts table links author via author_id (users.id); we join to get pial_id.
pub async fn count_posts_by_pial_last_hours(
    pool: &Pool,
    pial_id: &str,
    hours: i64,
) -> i64 {
    let client = match pool.get().await {
        Ok(c) => c,
        Err(_) => return 0,
    };

    let interval = format!("{} hours", hours);
    let row = client
        .query_opt(
            "SELECT COUNT(*) FROM posts p
             JOIN users u ON u.id = p.author_id
             WHERE u.pial_id = $1::uuid
               AND p.created_at > NOW() - $2::interval
               AND p.deleted_at IS NULL",
            &[&pial_id, &interval],
        )
        .await
        .ok()
        .flatten();

    row.map(|r| r.get::<_, i64>(0)).unwrap_or(0)
}

/// Check whether this PIAL posted the same body text in the last 24 hours (exact dupe).
pub async fn has_duplicate_body(pool: &Pool, pial_id: &str, body: &str) -> bool {
    let client = match pool.get().await {
        Ok(c) => c,
        Err(_) => return false,
    };

    let row = client
        .query_opt(
            "SELECT COUNT(*) FROM posts p
             JOIN users u ON u.id = p.author_id
             WHERE u.pial_id = $1::uuid
               AND p.body = $2
               AND p.created_at > NOW() - INTERVAL '24 hours'
               AND p.deleted_at IS NULL",
            &[&pial_id, &body],
        )
        .await
        .ok()
        .flatten();

    row.map(|r| r.get::<_, i64>(0) > 0).unwrap_or(false)
}

/// Return the account creation time and total post count for a PIAL.
pub async fn get_account_age_and_post_count(
    pool: &Pool,
    pial_id: &str,
) -> Option<(chrono::DateTime<chrono::Utc>, i64)> {
    let client = pool.get().await.map_err(|e| {
        error!(err = %e, "db pool error in get_account_age_and_post_count");
    }).ok()?;

    let row = client
        .query_opt(
            "SELECT u.created_at,
                    (SELECT COUNT(*) FROM posts p2 WHERE p2.author_id = u.id AND p2.deleted_at IS NULL)
             FROM users u
             WHERE u.pial_id = $1::uuid
             LIMIT 1",
            &[&pial_id],
        )
        .await
        .map_err(|e| {
            error!(err = %e, "query error in get_account_age_and_post_count");
        })
        .ok()??;

    let created_at: chrono::DateTime<chrono::Utc> = row.get(0);
    let post_count: i64 = row.get(1);
    Some((created_at, post_count))
}
