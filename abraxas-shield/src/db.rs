use deadpool_postgres::Pool;
use tracing::{error, warn};

/// All data from the content + users join needed to run the scan pipeline.
#[allow(dead_code)]
pub struct ContentScanRecord {
    pub content_id: String,
    pub pial_id: String,
    pub body: String,
    pub is_adult_creator: bool,
    pub author_handle: String,
}

/// Lane a piece of content lives in. Determines which table Abraxas reads the
/// content from and writes the verdict back to.
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum ContentSource {
    /// Permanent content in the `works` table (Malkuth signed-work path).
    Works,
    /// Ephemeral 24-hour content in the `visions` table.
    Visions,
}

impl ContentSource {
    /// Short lane tag recorded on scan-result and moderation-log rows so a Vision
    /// verdict is never mistaken for a Work verdict.
    pub fn lane(&self) -> &'static str {
        match self {
            ContentSource::Works => "work",
            ContentSource::Visions => "vision",
        }
    }

    /// notifications.target_type for content in this lane. Works keep the
    /// established 'post' value the notification renderers already dispatch on.
    pub fn target_type(&self) -> &'static str {
        match self {
            ContentSource::Works => "post",
            ContentSource::Visions => "vision",
        }
    }
}

/// Result of applying a verdict to a content row.
pub enum VerdictApplied {
    /// The row was live and took the new scan_state. Carries the previous state.
    Updated { from_state: String },
    /// The row is soft-deleted or (Visions) already past expires_at. Nothing was
    /// written. Not an error — an expired Vision outliving its own scan is normal.
    RowGone,
}

/// A Vision's address: a position in its author's PIAL-owned lane, with no id
/// of its own. Mirrors feed-engine's db.VisionRef and devserver's
/// store.VisionRef — the same `<author_pial>.<seq>` string travels everywhere.
struct VisionRef {
    author_pial: String,
    seq: i64,
}

/// Parse the canonical `<author_pial>.<seq>` form. None on anything else,
/// including a bare UUID — a Vision content id is never a UUID.
fn parse_vision_ref(id: &str) -> Option<VisionRef> {
    let (pial, seq_text) = id.rsplit_once('.')?;
    if !is_uuid(pial) {
        return None;
    }
    let seq: i64 = seq_text.parse().ok()?;
    if seq <= 0 {
        return None;
    }
    Some(VisionRef { author_pial: pial.to_string(), seq })
}

/// Full output of the shield pipeline, written to content_scan_results.
pub struct ScanResult {
    pub content_id: String,
    /// Lane tag, from ContentSource::lane().
    pub lane: String,
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

/// Reject ids that are not UUIDs before they reach a `::uuid` cast. A bad cast
/// is a query error, and a query error on a poison event would retry the same
/// offset forever and stall the partition.
///
/// Every uuid/interval parameter below goes through `::text::` first. Postgres
/// types a bare `$1::uuid` parameter AS uuid, and tokio-postgres then refuses to
/// serialize the Rust &str the brain actually sends ("error serializing
/// parameter"). Casting via text types the parameter text, which is correct.
fn is_uuid(id: &str) -> bool {
    uuid::Uuid::parse_str(id).is_ok()
}

/// Load the content row for scanning from the lane the event named.
///
/// Ok(Some) — row is live and scannable.
/// Ok(None) — row is absent, soft-deleted, or (Visions) already expired, or the
///            id is malformed for its lane. Caller logs and commits the
///            offset; there is nothing left to scan.
/// Err      — database failure. Caller must NOT commit; the scan is retried.
///
/// Visions are excluded once `expires_at` has passed: a 24-hour object that is
/// already gone must not be scanned or written back to.
pub async fn get_content_for_scan(
    pool: &Pool,
    content_id: &str,
    source: ContentSource,
) -> Result<Option<ContentScanRecord>, String> {
    let client = pool
        .get()
        .await
        .map_err(|e| format!("db pool error in get_content_for_scan: {e}"))?;

    const WORKS_SQL: &str = "SELECT
            w.id::TEXT,
            w.author_pial::TEXT,
            w.body,
            COALESCE(pr.is_adult_creator, FALSE),
            u.handle
         FROM works w
         JOIN users u ON u.id = w.author_id
         LEFT JOIN user_profiles pr ON pr.user_id = w.author_id
         WHERE w.id = $1::text::uuid
           AND w.deleted_at IS NULL";

    const VISIONS_SQL: &str = "SELECT
            $3::text,
            v.author_pial::TEXT,
            v.body,
            COALESCE(pr.is_adult_creator, FALSE),
            u.handle
         FROM visions v
         JOIN users u ON u.pial_id = v.author_pial
         LEFT JOIN user_profiles pr ON pr.user_id = u.id
         WHERE v.author_pial = $1::text::uuid
           AND v.seq = $2
           AND v.deleted_at IS NULL
           AND v.expires_at > NOW()";

    let row = match source {
        ContentSource::Works => {
            if !is_uuid(content_id) {
                warn!(content_id, lane = source.lane(), "Content id is not a UUID — unroutable event, skipping");
                return Ok(None);
            }
            client
                .query_opt(WORKS_SQL, &[&content_id])
                .await
                .map_err(|e| format!("get_content_for_scan query error ({}): {e}", source.lane()))?
        }
        ContentSource::Visions => {
            let Some(r) = parse_vision_ref(content_id) else {
                warn!(content_id, lane = source.lane(), "Content id is not a Vision address — unroutable event, skipping");
                return Ok(None);
            };
            client
                .query_opt(VISIONS_SQL, &[&r.author_pial, &r.seq, &content_id])
                .await
                .map_err(|e| format!("get_content_for_scan query error ({}): {e}", source.lane()))?
        }
    };

    Ok(row.map(|row| ContentScanRecord {
        content_id: row.get(0),
        pial_id: row.get(1),
        body: row.get(2),
        is_adult_creator: row.get(3),
        author_handle: row.get(4),
    }))
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
///
/// content_scan_results has no lane column, so the lane is recorded as a
/// `lane:<work|vision>` entry at the head of the signals array. It is queryable
/// (`signals @> ARRAY['lane:vision']`) and visible wherever signals are rendered.
/// A dedicated column would need a feed-engine migration, which is out of scope
/// for this brain — see the report.
pub async fn store_scan_result(pool: &Pool, result: &ScanResult) -> Result<(), String> {
    let client = pool.get().await.map_err(|e| {
        format!("db pool error: {e}")
    })?;

    // Merge all toxicity signals into a combined list for the hate_signals column.
    // violence and self_harm signals are included in the top-level signals array.
    let mut hate_and_tox: Vec<String> = result.hate_signals.clone();
    hate_and_tox.extend(result.violence_signals.iter().cloned());
    hate_and_tox.extend(result.self_harm_signals.iter().cloned());

    let mut signals: Vec<String> = Vec::with_capacity(result.signals.len() + 1);
    signals.push(format!("lane:{}", result.lane));
    signals.extend(result.signals.iter().cloned());

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
                &result.content_id,
                &result.scan_version,
                &result.nudity_score,
                &result.gore_score,
                &result.clickbait_score,
                &"",           // ocr_text — Phase 3.1
                &"",           // transcript — Phase 3.1
                &hate_and_tox,
                &result.risk_level,
                &result.recommendation,
                &signals,
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
/// Works lane only — the ephemeral Vision lane has no ranking surface.
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
                 VALUES ($1::text::uuid, $2, $3, NOW())
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

// One statement per lane: read the previous scan_state, apply the new state plus
// the is_blocked / is_nsfw side-effects, and return the previous state. Returns
// zero rows when the row is not live, which is how "row is gone" is detected
// without a second round trip and without a window between the reads.
// The flags are OR-ed so a verdict never clears a flag another brain has set.
const WORKS_APPLY_SQL: &str = "
    WITH before AS (
        SELECT id, scan_state
          FROM works
         WHERE id = $2::text::uuid
           AND deleted_at IS NULL
    ),
    applied AS (
        UPDATE works w
           SET scan_state = $1,
               is_blocked = (w.is_blocked OR $1 = 'blocked'),
               is_nsfw    = (w.is_nsfw    OR $1 IN ('age_gate', 'age_gated'))
          FROM before b
         WHERE w.id = b.id
        RETURNING w.id
    )
    SELECT b.scan_state
      FROM before b
      JOIN applied a ON a.id = b.id";

// Visions additionally require a live expiry window: a verdict must never
// resurrect or rewrite content whose 24 hours are already up. Addressed by
// (author_pial, seq) — a Vision has no id of its own.
const VISIONS_APPLY_SQL: &str = "
    WITH before AS (
        SELECT author_pial, seq, scan_state
          FROM visions
         WHERE author_pial = $2::text::uuid
           AND seq = $3
           AND deleted_at IS NULL
           AND expires_at > NOW()
    ),
    applied AS (
        UPDATE visions v
           SET scan_state = $1,
               is_blocked = (v.is_blocked OR $1 = 'blocked'),
               is_nsfw    = (v.is_nsfw    OR $1 IN ('age_gate', 'age_gated'))
          FROM before b
         WHERE v.author_pial = b.author_pial AND v.seq = b.seq
        RETURNING v.author_pial
    )
    SELECT b.scan_state
      FROM before b
      JOIN applied a ON a.author_pial = b.author_pial";

/// Apply a verdict to the content row in its own lane and append the transition
/// to content_moderation_log.
///
/// Returns VerdictApplied::RowGone when the row is no longer live — soft-deleted,
/// or a Vision past its expiry. That is a normal outcome, not an error: nothing is
/// written and the caller commits the offset instead of retrying forever.
///
/// The moderation log has no lane column, so the lane is carried in `actor` as
/// `abraxas-shield:<work|vision>`.
pub async fn set_content_scan_state(
    pool: &Pool,
    content_id: &str,
    state: &str,
    reason: &str,
    source: ContentSource,
) -> Result<VerdictApplied, String> {
    let client = pool.get().await.map_err(|e| format!("db pool error: {e}"))?;

    let row = match source {
        ContentSource::Works => {
            if !is_uuid(content_id) {
                return Err(format!("set_content_scan_state: content id is not a UUID: {content_id}"));
            }
            client
                .query_opt(WORKS_APPLY_SQL, &[&state, &content_id])
                .await
                .map_err(|e| format!("apply scan_state error ({}): {e}", source.lane()))?
        }
        ContentSource::Visions => {
            let Some(r) = parse_vision_ref(content_id) else {
                return Err(format!("set_content_scan_state: content id is not a Vision address: {content_id}"));
            };
            client
                .query_opt(VISIONS_APPLY_SQL, &[&state, &r.author_pial, &r.seq])
                .await
                .map_err(|e| format!("apply scan_state error ({}): {e}", source.lane()))?
        }
    };

    let Some(row) = row else {
        return Ok(VerdictApplied::RowGone);
    };

    let from_state: String = row.get::<_, Option<String>>(0).unwrap_or_default();
    let actor = format!("abraxas-shield:{}", source.lane());

    client
        .execute(
            "INSERT INTO content_moderation_log
               (post_id, from_state, to_state, reason, actor, created_at)
             VALUES ($1, $2, $3, $4, $5, NOW())",
            &[&content_id, &from_state, &state, &reason, &actor],
        )
        .await
        .map_err(|e| format!("moderation log insert error ({}): {e}", source.lane()))?;

    Ok(VerdictApplied::Updated { from_state })
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
/// `target_type` names the lane the flagged content lives in, so the admin
/// surface can resolve the target id against the right table.
pub async fn notify_user(
    pool: &Pool,
    user_id: &str,
    notif_type: &str,
    target_id: &str,
    target_type: &str,
) -> Result<(), String> {
    let client = pool.get().await.map_err(|e| format!("db pool error: {e}"))?;

    client
        .execute(
            "INSERT INTO notifications (user_id, type, actor_id, target_id, target_type)
             VALUES ($1::text::uuid, $2, NULL, $3, $4)",
            &[&user_id, &notif_type, &target_id, &target_type],
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
        Err(e) => {
            error!(err = %e, "db pool error in load_suppressed_signals — no signals suppressed");
            return vec![];
        }
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
        Err(e) => {
            error!(err = %e, "query error in load_suppressed_signals — no signals suppressed");
            return vec![];
        }
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

/// Count content published by a PIAL in the last N hours across both lanes.
/// A Vision burst is publishing volume exactly as a Work burst is, so rate-based
/// spam detection counts both. Expired Visions still count inside the window —
/// the window is short and expiry is 24 hours.
pub async fn count_content_by_pial_last_hours(
    pool: &Pool,
    pial_id: &str,
    hours: i64,
) -> i64 {
    let client = match pool.get().await {
        Ok(c) => c,
        Err(e) => {
            error!(err = %e, "db pool error in count_content_by_pial_last_hours — rate treated as 0");
            return 0;
        }
    };

    let interval = format!("{} hours", hours);
    let row = match client
        .query_opt(
            "SELECT (SELECT COUNT(*) FROM works w
                      WHERE w.author_pial = $1::text::uuid
                        AND w.created_at > NOW() - $2::text::interval
                        AND w.deleted_at IS NULL)
                  + (SELECT COUNT(*) FROM visions v
                      WHERE v.author_pial = $1::text::uuid
                        AND v.created_at > NOW() - $2::text::interval
                        AND v.deleted_at IS NULL)",
            &[&pial_id, &interval],
        )
        .await
    {
        Ok(r) => r,
        Err(e) => {
            error!(err = %e, "query error in count_content_by_pial_last_hours — rate treated as 0");
            return 0;
        }
    };

    row.map(|r| r.get::<_, i64>(0)).unwrap_or(0)
}

/// Check whether this PIAL published the same body text in the last 24 hours,
/// across both lanes. `content_id` is the row currently being scanned and is
/// excluded — it is already inserted by the time the event is published, so
/// counting it would mark every single piece of content a duplicate of itself.
///
/// `content_id` names the row currently being scanned, in whichever lane
/// `source` says it lives in — a Work carries no `id` in the Visions half of
/// the count and a Vision carries no `id` in the Works half, so each half
/// only ever excludes something in its own lane.
pub async fn has_duplicate_body(
    pool: &Pool,
    pial_id: &str,
    body: &str,
    content_id: &str,
    source: ContentSource,
) -> bool {
    let client = match pool.get().await {
        Ok(c) => c,
        Err(e) => {
            error!(err = %e, "db pool error in has_duplicate_body — treated as not duplicate");
            return false;
        }
    };

    let row = match source {
        ContentSource::Works => {
            if !is_uuid(content_id) {
                return false;
            }
            client
                .query_opt(
                    "SELECT (SELECT COUNT(*) FROM works w
                              WHERE w.author_pial = $1::text::uuid
                                AND w.body = $2
                                AND w.id <> $3::text::uuid
                                AND w.created_at > NOW() - INTERVAL '24 hours'
                                AND w.deleted_at IS NULL)
                          + (SELECT COUNT(*) FROM visions v
                              WHERE v.author_pial = $1::text::uuid
                                AND v.body = $2
                                AND v.created_at > NOW() - INTERVAL '24 hours'
                                AND v.deleted_at IS NULL)",
                    &[&pial_id, &body, &content_id],
                )
                .await
        }
        ContentSource::Visions => {
            let Some(r) = parse_vision_ref(content_id) else {
                return false;
            };
            client
                .query_opt(
                    "SELECT (SELECT COUNT(*) FROM works w
                              WHERE w.author_pial = $1::text::uuid
                                AND w.body = $2
                                AND w.created_at > NOW() - INTERVAL '24 hours'
                                AND w.deleted_at IS NULL)
                          + (SELECT COUNT(*) FROM visions v
                              WHERE v.author_pial = $1::text::uuid
                                AND v.body = $2
                                AND v.seq <> $3
                                AND v.created_at > NOW() - INTERVAL '24 hours'
                                AND v.deleted_at IS NULL)",
                    &[&pial_id, &body, &r.seq],
                )
                .await
        }
    };
    let row = match row {
        Ok(r) => r,
        Err(e) => {
            error!(err = %e, "query error in has_duplicate_body — treated as not duplicate");
            return false;
        }
    };

    row.map(|r| r.get::<_, i64>(0) > 0).unwrap_or(false)
}

/// Return the account creation time and total published-content count for a PIAL,
/// counting both the permanent Work lane and the ephemeral Vision lane.
pub async fn get_account_age_and_content_count(
    pool: &Pool,
    pial_id: &str,
) -> Option<(chrono::DateTime<chrono::Utc>, i64)> {
    let client = pool.get().await.map_err(|e| {
        error!(err = %e, "db pool error in get_account_age_and_content_count");
    }).ok()?;

    let row = client
        .query_opt(
            "SELECT u.created_at,
                    (SELECT COUNT(*) FROM works w
                      WHERE w.author_id = u.id AND w.deleted_at IS NULL)
                  + (SELECT COUNT(*) FROM visions v
                      WHERE v.author_pial = u.pial_id AND v.deleted_at IS NULL)
             FROM users u
             WHERE u.pial_id = $1::text::uuid
             LIMIT 1",
            &[&pial_id],
        )
        .await
        .map_err(|e| {
            error!(err = %e, "query error in get_account_age_and_content_count");
        })
        .ok()??;

    let created_at: chrono::DateTime<chrono::Utc> = row.get(0);
    let content_count: i64 = row.get(1);
    Some((created_at, content_count))
}
