/// Elohim Veni — Security Brain database schema.
///
/// Design principles:
///   - PIAL (Principal Identity Access Layer) is the root of all trust decisions.
///   - Capability grants are explicit and auditable.
///   - Every decision is logged — the audit trail is immutable.
///   - Trust scores are derived signals, updated after each decision.
///   - Schema is additive-only for forward compatibility.

use anyhow::Result;
use sqlx::PgPool;

const SCHEMA: &str = r#"
-- ── PIAL States ───────────────────────────────────────────────────────────────
-- One row per account. The PIAL is the authoritative identity anchor.
CREATE TABLE IF NOT EXISTS pial_states (
    pial_id     UUID        PRIMARY KEY,  -- provided by feed-engine/PIAL system; no auto-generate
    account_id  UUID        NOT NULL UNIQUE,
    status      TEXT        NOT NULL DEFAULT 'ACTIVE',   -- ACTIVE | SUSPENDED | REVOKED
    tier        TEXT        NOT NULL DEFAULT 'BASIC',    -- BASIC | VERIFIED | TRUSTED | PRIVILEGED
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_pial_account  ON pial_states(account_id);
CREATE INDEX IF NOT EXISTS idx_pial_status   ON pial_states(status);
CREATE INDEX IF NOT EXISTS idx_pial_tier     ON pial_states(tier);

-- ── Capability Grants ─────────────────────────────────────────────────────────
-- Explicit allow/deny for each named capability per PIAL.
-- A missing row means the capability inherits from tier defaults.
CREATE TABLE IF NOT EXISTS capability_grants (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id     UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    capability  TEXT        NOT NULL,   -- post | message | monetize | adult_content | node_relay
    granted     BOOLEAN     NOT NULL DEFAULT FALSE,
    granted_by  UUID,                   -- admin PIAL that issued the grant
    reason      TEXT,
    expires_at  TIMESTAMPTZ,            -- NULL = never expires
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pial_id, capability)
);
CREATE INDEX IF NOT EXISTS idx_cap_pial       ON capability_grants(pial_id);
CREATE INDEX IF NOT EXISTS idx_cap_capability ON capability_grants(capability);
CREATE INDEX IF NOT EXISTS idx_cap_expires    ON capability_grants(expires_at) WHERE expires_at IS NOT NULL;

-- ── Trust Scores ──────────────────────────────────────────────────────────────
-- Running trust signal per PIAL — updated after each decision.
CREATE TABLE IF NOT EXISTS trust_scores (
    pial_id         UUID        PRIMARY KEY REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    score           DOUBLE PRECISION NOT NULL DEFAULT 1.0,   -- 0.0 (no trust) – 1.0 (full trust)
    anomaly_score   DOUBLE PRECISION NOT NULL DEFAULT 0.0,   -- 0.0 (normal) – 1.0 (highly anomalous)
    violation_count INT         NOT NULL DEFAULT 0,
    last_decision   TEXT,                                    -- last decision outcome
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Decision Log ──────────────────────────────────────────────────────────────
-- Immutable audit trail. Never deleted, only appended.
CREATE TABLE IF NOT EXISTS decision_log (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id             UUID        NOT NULL REFERENCES pial_states(pial_id) ON DELETE CASCADE,
    action              TEXT        NOT NULL,
    decision            TEXT        NOT NULL,   -- ALLOW | ALLOW_RESTRICTED | QUARANTINE | DENY
    confidence          DOUBLE PRECISION NOT NULL,
    reason              TEXT        NOT NULL,
    capability_required TEXT,
    content_risk        DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    anomaly_score       DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_dl_pial      ON decision_log(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_dl_decision  ON decision_log(decision, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_dl_action    ON decision_log(action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_dl_today     ON decision_log(created_at DESC);

-- ── PIAL Signing Keys ─────────────────────────────────────────────────────────
-- ECDSA-P256 public keys used by Themis to verify marketplace event signatures.
CREATE TABLE IF NOT EXISTS pial_signing_keys (
    pial_id       UUID        PRIMARY KEY,
    public_key_b64 TEXT       NOT NULL,
    algorithm     TEXT        NOT NULL DEFAULT 'ECDSA-P256',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── PIAL ECDH Keys ────────────────────────────────────────────────────────────
-- ECDH-P256 public keys used by Themis to wrap content encryption keys (CEK).
CREATE TABLE IF NOT EXISTS pial_ecdh_keys (
    pial_id        UUID        PRIMARY KEY,
    public_key_b64 TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    Ok(())
}

/// Count total PIALs registered.
pub async fn count_pials(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as("SELECT COUNT(*) FROM pial_states")
        .fetch_one(pool)
        .await?;
    Ok(row.0)
}

/// Count total decisions ever logged.
pub async fn count_decisions_total(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as("SELECT COUNT(*) FROM decision_log")
        .fetch_one(pool)
        .await?;
    Ok(row.0)
}

/// Count decisions logged in the last 24 hours.
pub async fn count_decisions_today(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM decision_log WHERE created_at >= NOW() - INTERVAL '24 hours'"
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}

/// Count DENY decisions in the last 24 hours (for deny-rate metric).
pub async fn count_denials_today(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM decision_log \
         WHERE decision = 'DENY' AND created_at >= NOW() - INTERVAL '24 hours'"
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}

/// Upsert a PIAL ECDSA signing public key.
pub async fn upsert_signing_key(pool: &PgPool, pial_id: uuid::Uuid, public_key_b64: &str, algorithm: &str) -> Result<()> {
    sqlx::query(
        "INSERT INTO pial_signing_keys (pial_id, public_key_b64, algorithm, updated_at)
         VALUES ($1, $2, $3, NOW())
         ON CONFLICT (pial_id) DO UPDATE
           SET public_key_b64 = EXCLUDED.public_key_b64,
               algorithm = EXCLUDED.algorithm,
               updated_at = NOW()"
    ).bind(pial_id).bind(public_key_b64).bind(algorithm)
     .execute(pool).await?;
    Ok(())
}

/// Fetch a PIAL ECDSA signing public key, returning None if not found.
pub async fn get_signing_key(pool: &PgPool, pial_id: uuid::Uuid) -> Result<Option<String>> {
    let row = sqlx::query_scalar::<_, String>(
        "SELECT public_key_b64 FROM pial_signing_keys WHERE pial_id = $1"
    ).bind(pial_id).fetch_optional(pool).await?;
    Ok(row)
}

/// Upsert a PIAL ECDH public key.
pub async fn upsert_ecdh_key(pool: &PgPool, pial_id: uuid::Uuid, public_key_b64: &str) -> Result<()> {
    sqlx::query(
        "INSERT INTO pial_ecdh_keys (pial_id, public_key_b64, updated_at)
         VALUES ($1, $2, NOW())
         ON CONFLICT (pial_id) DO UPDATE
           SET public_key_b64 = EXCLUDED.public_key_b64,
               updated_at = NOW()"
    ).bind(pial_id).bind(public_key_b64)
     .execute(pool).await?;
    Ok(())
}

/// Fetch a PIAL ECDH public key, returning None if not found.
pub async fn get_ecdh_key(pool: &PgPool, pial_id: uuid::Uuid) -> Result<Option<String>> {
    let row = sqlx::query_scalar::<_, String>(
        "SELECT public_key_b64 FROM pial_ecdh_keys WHERE pial_id = $1"
    ).bind(pial_id).fetch_optional(pool).await?;
    Ok(row)
}
