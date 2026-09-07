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

// ── Manhattan outbox ──────────────────────────────────────────────────────────
//
// Zodacare's durable handoff to the Manhattan naming plane. Port of
// feed-engine/internal/db/migrations/0005_manhattan_outbox.sql into the
// mechanism this brain already has: idempotent DDL applied by migrate().
//
// Manhattan is a separate brain reached over HTTP. A graph write sent
// fire-and-forget is a graph write that a restart, a timeout or a rolling
// deploy silently loses — and a naming plane that is silently missing rows is
// worse than no naming plane, because everything downstream trusts it.
//
// So the handoff is transactional. These rows are written by triggers on the
// tables they describe, inside the SAME transaction as the row that caused
// them. Either a report exists AND the identities it names are queued for
// registration, or neither happened; a rollback takes the registration with it.
//
// The triggers are the reason this cannot rot. A future writer that inserts a
// moderation action through some path nobody has thought of yet still registers
// its subject, because registering is a property of the table and not a step a
// caller must remember.
//
// Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
// does not know them and must not learn them, or the two planes acquire a
// second shared identifier and we are back where we started.
//
// What zodacare enqueues, and what it deliberately does not:
//
//   * identity NODES, named `pial:<uuid>`. Any brain may create a node —
//     registration has to work whoever encounters the entity first — while the
//     node's OWNER is decided by Manhattan's authority map, which assigns
//     `identity` to elohim-veni. Zodacare registers; it does not claim.
//
//   * no name bindings. The `pial` namespace is elohim-veni's and `handle` is
//     registry-brain's (0002_authority_map.sql). Zodacare binding either would
//     be refused at the wire, correctly.
//
//   * no edges. An edge's subject must be owned by the caller, and identity
//     nodes are not zodacare's. Moderation facts stay in f33d3r_safety, where
//     they belong; only the NAME crosses the boundary.
const MANHATTAN_OUTBOX: &str = r#"
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL CHECK (op IN ('node', 'name', 'edge', 'revoke_name')),
    payload         JSONB       NOT NULL,
    -- dedup_key makes enqueue idempotent, so a replayed insert or a backfill
    -- cannot queue the same registration twice.
    dedup_key       TEXT        NOT NULL UNIQUE,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ
);

-- The drain's only query: undelivered, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_pending
    ON manhattan_outbox(next_attempt_at, id) WHERE delivered_at IS NULL;

-- ── Quarantine ───────────────────────────────────────────────────────────────
-- Head-of-line blocking is correct while a row can still land: later rows depend
-- on earlier ones, and running ahead would write edges pointing at nodes that do
-- not exist. It stops being correct the moment a row cannot land at all — then
-- it is not ordering the queue, it is ending it. One unfixable row used to stop
-- this brain's entire naming-plane output indefinitely, with a log line as the
-- only signal and a hand-written migration as the only cure.
--
-- A quarantined row is neither dropped nor delivered. It keeps its payload, its
-- error and its id; the drain simply stops claiming it, so everything queued
-- behind it moves again. It represents a naming-plane write that did NOT happen,
-- so it is reported as an incident on every tick until a person clears
-- blocked_at — loud by construction rather than by anyone remembering to look.
--
--   what is stuck:  SELECT id, op, blocked_at, blocked_reason, payload
--                     FROM manhattan_outbox
--                    WHERE blocked_at IS NOT NULL AND delivered_at IS NULL
--                    ORDER BY id;
--   put one back:   UPDATE manhattan_outbox
--                      SET blocked_at = NULL, blocked_reason = NULL,
--                          attempts = 0, next_attempt_at = NOW()
--                    WHERE id = $1;
--
-- Returning a row to the queue re-enters it at its original id, so the ordering
-- the drain depends on survives the round trip.
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_at     TIMESTAMPTZ;
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_reason TEXT;

-- The drain's query is now "undelivered, not quarantined, due, oldest first", so
-- it gets its own partial index. The one above stays: the backlog counters still
-- ask for everything undelivered, quarantined rows included, because a write
-- that did not happen must not disappear from the depth a health check reports.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_deliverable
    ON manhattan_outbox(next_attempt_at, id)
 WHERE delivered_at IS NULL AND blocked_at IS NULL;

-- What is quarantined, read on every drain tick.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_blocked
    ON manhattan_outbox(id) WHERE blocked_at IS NOT NULL AND delivered_at IS NULL;

-- ── enqueue helper ───────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

-- ── identity registration ────────────────────────────────────────────────────
-- A person's identity node is named by PIAL, never by handle. A handle is a
-- pointer that can be transferred, sold or reclaimed; PIAL is the root that
-- never moves. Zodacare stores no handle copies at all, which is why it has no
-- handle to revoke here — it resolves one every time it is asked to.
--
-- Three columns in this database spell the same concept three ways
-- (reported_pial_id, reporter_pial_id, pial_id). One function makes them one
-- registration, so they can no longer drift apart unnoticed.
CREATE OR REPLACE FUNCTION manhattan_register_pial(p_pial UUID) RETURNS VOID AS $$
BEGIN
    IF p_pial IS NULL THEN
        RETURN;
    END IF;
    PERFORM manhattan_enqueue(
        'node',
        'node:pial:' || p_pial::text,
        jsonb_build_object(
            'kind',      'identity',
            'name',      'pial:' || p_pial::text,
            'namespace', 'pial'));
END;
$$ LANGUAGE plpgsql;

-- ── content_reports ──────────────────────────────────────────────────────────
-- Both parties are registered. The reporter matters as much as the reported:
-- a report whose author cannot be named later is a report nobody can audit.
CREATE OR REPLACE FUNCTION manhattan_register_report() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_register_pial(NEW.reported_pial_id);
    PERFORM manhattan_register_pial(NEW.reporter_pial_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_report ON content_reports;
CREATE TRIGGER trg_manhattan_register_report
    AFTER INSERT OR UPDATE OF reporter_pial_id, reported_pial_id ON content_reports
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_report();

-- ── moderation_actions ───────────────────────────────────────────────────────
-- The sharpest case. A ban is enforcement handed to another brain by name; if
-- that name was never registered, the enforcement cannot be attributed to a
-- person at all.
CREATE OR REPLACE FUNCTION manhattan_register_action() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_register_pial(NEW.pial_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_action ON moderation_actions;
CREATE TRIGGER trg_manhattan_register_action
    AFTER INSERT OR UPDATE OF pial_id ON moderation_actions
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_action();

-- ── user_risk_profiles ───────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_register_risk_profile() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_register_pial(NEW.pial_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_risk_profile ON user_risk_profiles;
CREATE TRIGGER trg_manhattan_register_risk_profile
    AFTER INSERT OR UPDATE OF pial_id ON user_risk_profiles
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_risk_profile();

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Every identity this brain already references is enqueued once, so the plane
-- starts complete instead of only knowing about people reported after this
-- deploy.
--
-- migrate() is idempotent and runs on every boot, so the backfill records that
-- it ran. ON CONFLICT DO NOTHING would already make a repeat harmless, but
-- "harmless" is not "free": without the marker every boot rescans every report,
-- action and risk profile this brain has ever held.
CREATE TABLE IF NOT EXISTS manhattan_backfill (
    step         TEXT        PRIMARY KEY,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DO $$
DECLARE r RECORD;
BEGIN
    IF EXISTS (SELECT 1 FROM manhattan_backfill WHERE step = 'identities_v1') THEN
        RETURN;
    END IF;

    FOR r IN SELECT DISTINCT p FROM (
        SELECT reported_pial_id AS p FROM content_reports WHERE reported_pial_id IS NOT NULL
        UNION SELECT reporter_pial_id FROM content_reports WHERE reporter_pial_id IS NOT NULL
        UNION SELECT pial_id         FROM moderation_actions
        UNION SELECT pial_id         FROM user_risk_profiles
    ) AS every_identity LOOP
        PERFORM manhattan_register_pial(r.p);
    END LOOP;

    -- Written in the same transaction as the rows it enqueued: if the backfill
    -- fails halfway, the marker is not there and the next boot starts over.
    INSERT INTO manhattan_backfill (step) VALUES ('identities_v1');
END $$;
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    // Applied after SCHEMA: the triggers below attach to tables SCHEMA creates,
    // and the backfill reads them.
    sqlx::raw_sql(MANHATTAN_OUTBOX).execute(pool).await?;
    Ok(())
}

// ── Stats ─────────────────────────────────────────────────────────────────────

pub async fn count_total_reports(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as("SELECT COUNT(*) FROM content_reports")
        .fetch_one(pool)
        .await?;
    Ok(row.0)
}

pub async fn count_pending_reports(pool: &PgPool) -> Result<i64> {
    let row: (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM content_reports WHERE status = 'pending'")
            .fetch_one(pool)
            .await?;
    Ok(row.0)
}

pub async fn count_actioned_today(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM content_reports \
         WHERE status = 'actioned' AND resolved_at >= NOW() - INTERVAL '24 hours'",
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}

pub async fn count_banned_users(pool: &PgPool) -> Result<i64> {
    let row: (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM user_risk_profiles WHERE status = 'banned'")
            .fetch_one(pool)
            .await?;
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
         RETURNING id",
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
                   updated_at = NOW()",
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
             ORDER BY created_at DESC LIMIT $2 OFFSET $3",
        )
        .bind(s)
        .bind(limit)
        .bind(offset)
        .fetch_all(pool)
        .await?
    } else {
        sqlx::query_as::<_, ContentReport>(
            "SELECT id, reporter_pial_id, reported_pial_id, content_id, content_type, \
                    reason, detail, status, resolved_by, resolved_at, escalated, created_at \
             FROM content_reports \
             ORDER BY created_at DESC LIMIT $1 OFFSET $2",
        )
        .bind(limit)
        .bind(offset)
        .fetch_all(pool)
        .await?
    };
    Ok(reports)
}

pub async fn get_report(pool: &PgPool, id: Uuid) -> Result<Option<ContentReport>> {
    let row = sqlx::query_as::<_, ContentReport>(
        "SELECT id, reporter_pial_id, reported_pial_id, content_id, content_type, \
                reason, detail, status, resolved_by, resolved_at, escalated, created_at \
         FROM content_reports WHERE id = $1",
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
         WHERE id = $1 AND status IN ('pending', 'reviewing')",
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
        "UPDATE content_reports SET escalated = TRUE, status = 'reviewing' WHERE id = $1",
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
         FROM user_risk_profiles WHERE pial_id = $1",
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
        "ban" => "banned",
        "restrict" => "restricted",
        "warn" => "warned",
        "unban" => "active",
        _ => "active",
    };
    let risk_delta: f64 = match action_type {
        "ban" => 0.4,
        "restrict" => 0.2,
        "warn" => 0.1,
        "unban" => -0.3,
        _ => 0.0,
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
         VALUES ($1, $2, $3, $4, $5) RETURNING id",
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
