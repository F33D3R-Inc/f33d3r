use anyhow::Result;
use sqlx::PgPool;
use uuid::Uuid;

use crate::models::{ContentReport, UserRiskProfile};

const SCHEMA: &str = r#"
-- ── Content Reports ───────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS content_reports (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    reporter_pial_id  UUID,
    reported_pial_id  UUID,
    content_id        TEXT        NOT NULL,
    content_type      TEXT        NOT NULL DEFAULT 'post',
    reason            TEXT        NOT NULL,
    detail            TEXT,
    status            TEXT        NOT NULL DEFAULT 'pending', -- pending | reviewing | actioned | dismissed
    resolved_by       TEXT,
    resolved_at       TIMESTAMPTZ,
    escalated         BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_cr_status       ON content_reports(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_cr_reported     ON content_reports(reported_pial_id);
CREATE INDEX IF NOT EXISTS idx_cr_content      ON content_reports(content_id);

-- ── User Risk Profiles ────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS user_risk_profiles (
    pial_id          UUID        PRIMARY KEY,
    risk_score       DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    report_count     INT         NOT NULL DEFAULT 0,
    violation_count  INT         NOT NULL DEFAULT 0,
    last_action      TEXT,
    status           TEXT        NOT NULL DEFAULT 'active', -- active | warned | restricted | banned
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_urp_status      ON user_risk_profiles(status);
CREATE INDEX IF NOT EXISTS idx_urp_risk        ON user_risk_profiles(risk_score DESC);

-- ── Moderation Actions ────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS moderation_actions (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id      UUID        NOT NULL,
    action_type  TEXT        NOT NULL, -- warn | restrict | ban | unban
    reason       TEXT        NOT NULL,
    applied_by   TEXT,
    expires_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ma_pial         ON moderation_actions(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ma_active       ON moderation_actions(action_type, expires_at);
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    Ok(())
}

// ── Stats ─────────────────────────────────────────────────────────────────────

pub async fn count_total_reports(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as("SELECT COUNT(*) FROM content_reports")
        .fetch_one(pool).await?;
    Ok(row.0)
}

pub async fn count_pending_reports(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM content_reports WHERE status = 'pending'"
    ).fetch_one(pool).await?;
    Ok(row.0)
}

pub async fn count_actioned_today(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM content_reports \
         WHERE status = 'actioned' AND resolved_at >= NOW() - INTERVAL '24 hours'"
    ).fetch_one(pool).await?;
    Ok(row.0)
}

pub async fn count_banned_users(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM user_risk_profiles WHERE status = 'banned'"
    ).fetch_one(pool).await?;
    Ok(row.0)
}

// ── Reports ───────────────────────────────────────────────────────────────────

pub async fn insert_report(
    pool: &PgPool,
    reporter_pial_id: Option<Uuid>,
    reported_pial_id: Option<Uuid>,
    content_id: &str,
    content_type: &str,
    reason: &str,
    detail: Option<&str>,
) -> Result<Uuid> {
    let row: (Uuid,) = sqlx::query_as(
        "INSERT INTO content_reports \
           (reporter_pial_id, reported_pial_id, content_id, content_type, reason, detail) \
         VALUES ($1, $2, $3, $4, $5, $6) \
         RETURNING id"
    )
    .bind(reporter_pial_id)
    .bind(reported_pial_id)
    .bind(content_id)
    .bind(content_type)
    .bind(reason)
    .bind(detail)
    .fetch_one(pool)
    .await?;

    // Update or create risk profile for the reported PIAL.
    if let Some(pial_id) = reported_pial_id {
        let _ = sqlx::query(
            "INSERT INTO user_risk_profiles (pial_id, report_count, risk_score) \
             VALUES ($1, 1, 0.1) \
             ON CONFLICT (pial_id) DO UPDATE \
               SET report_count = user_risk_profiles.report_count + 1, \
                   risk_score = LEAST(1.0, user_risk_profiles.risk_score + 0.05), \
                   updated_at = NOW()"
        )
        .bind(pial_id)
        .execute(pool)
        .await;
    }

    Ok(row.0)
}

pub async fn list_reports(
    pool: &PgPool,
    status: Option<&str>,
    limit: i64,
    offset: i64,
) -> Result<Vec<ContentReport>> {
    let reports = if let Some(s) = status {
        sqlx::query_as::<_, ContentReport>(
            "SELECT id, reporter_pial_id, reported_pial_id, content_id, content_type, \
                    reason, detail, status, resolved_by, resolved_at, escalated, created_at \
             FROM content_reports WHERE status = $1 \
             ORDER BY created_at DESC LIMIT $2 OFFSET $3"
        )
        .bind(s).bind(limit).bind(offset)
        .fetch_all(pool).await?
    } else {
        sqlx::query_as::<_, ContentReport>(
            "SELECT id, reporter_pial_id, reported_pial_id, content_id, content_type, \
                    reason, detail, status, resolved_by, resolved_at, escalated, created_at \
             FROM content_reports \
             ORDER BY created_at DESC LIMIT $1 OFFSET $2"
        )
        .bind(limit).bind(offset)
        .fetch_all(pool).await?
    };
    Ok(reports)
}

pub async fn get_report(pool: &PgPool, id: Uuid) -> Result<Option<ContentReport>> {
    let row = sqlx::query_as::<_, ContentReport>(
        "SELECT id, reporter_pial_id, reported_pial_id, content_id, content_type, \
                reason, detail, status, resolved_by, resolved_at, escalated, created_at \
         FROM content_reports WHERE id = $1"
    )
    .bind(id)
    .fetch_optional(pool)
    .await?;
    Ok(row)
}

pub async fn resolve_report(
    pool: &PgPool,
    id: Uuid,
    status: &str,
    resolved_by: Option<&str>,
) -> Result<bool> {
    let result = sqlx::query(
        "UPDATE content_reports \
         SET status = $2, resolved_by = $3, resolved_at = NOW() \
         WHERE id = $1 AND status IN ('pending', 'reviewing')"
    )
    .bind(id)
    .bind(status)
    .bind(resolved_by)
    .execute(pool)
    .await?;
    Ok(result.rows_affected() > 0)
}

pub async fn escalate_report(pool: &PgPool, id: Uuid) -> Result<bool> {
    let result = sqlx::query(
        "UPDATE content_reports SET escalated = TRUE, status = 'reviewing' WHERE id = $1"
    )
    .bind(id)
    .execute(pool)
    .await?;
    Ok(result.rows_affected() > 0)
}

// ── User Risk ─────────────────────────────────────────────────────────────────

pub async fn get_risk_profile(pool: &PgPool, pial_id: Uuid) -> Result<Option<UserRiskProfile>> {
    let row = sqlx::query_as::<_, UserRiskProfile>(
        "SELECT pial_id, risk_score, report_count, violation_count, \
                last_action, status, created_at, updated_at \
         FROM user_risk_profiles WHERE pial_id = $1"
    )
    .bind(pial_id)
    .fetch_optional(pool)
    .await?;
    Ok(row)
}

pub async fn apply_moderation_action(
    pool: &PgPool,
    pial_id: Uuid,
    action_type: &str,
    reason: &str,
    applied_by: Option<&str>,
    expires_at: Option<chrono::DateTime<chrono::Utc>>,
) -> Result<Uuid> {
    let new_status = match action_type {
        "ban"       => "banned",
        "restrict"  => "restricted",
        "warn"      => "warned",
        "unban"     => "active",
        _           => "active",
    };
    let risk_delta: f64 = match action_type {
        "ban"      => 0.4,
        "restrict" => 0.2,
        "warn"     => 0.1,
        "unban"    => -0.3,
        _          => 0.0,
    };

    // Upsert risk profile.
    sqlx::query(
        "INSERT INTO user_risk_profiles (pial_id, risk_score, violation_count, last_action, status) \
         VALUES ($1, $2, 1, $3, $4) \
         ON CONFLICT (pial_id) DO UPDATE \
           SET risk_score = LEAST(1.0, GREATEST(0.0, user_risk_profiles.risk_score + $5)), \
               violation_count = CASE WHEN $6 = 'unban' \
                 THEN user_risk_profiles.violation_count \
                 ELSE user_risk_profiles.violation_count + 1 END, \
               last_action = $3, \
               status = $4, \
               updated_at = NOW()"
    )
    .bind(pial_id)
    .bind(risk_delta.clamp(0.0, 1.0))
    .bind(action_type)
    .bind(new_status)
    .bind(risk_delta)
    .bind(action_type)
    .execute(pool)
    .await?;

    // Log the action.
    let row: (Uuid,) = sqlx::query_as(
        "INSERT INTO moderation_actions (pial_id, action_type, reason, applied_by, expires_at) \
         VALUES ($1, $2, $3, $4, $5) RETURNING id"
    )
    .bind(pial_id)
    .bind(action_type)
    .bind(reason)
    .bind(applied_by)
    .bind(expires_at)
    .fetch_one(pool)
    .await?;

    Ok(row.0)
}
