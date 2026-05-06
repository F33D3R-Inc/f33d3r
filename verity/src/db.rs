use anyhow::Result;
use sqlx::PgPool;

const SCHEMA: &str = r#"
-- ── Attestations ─────────────────────────────────────────────────────────────
-- One row per verification event per PIAL.
-- Immutable once written. New decisions append as new rows.
CREATE TABLE IF NOT EXISTS attestations (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id         UUID        NOT NULL,
    context         TEXT        NOT NULL,           -- onboarding | age_gate | creator_signup | payout
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('approved','denied','pending_review','pending')),
    age_band        TEXT        NOT NULL DEFAULT 'unknown'
                    CHECK (age_band IN ('18+','21+','underage','unknown')),
    creator_tier    INTEGER     NOT NULL DEFAULT 0 CHECK (creator_tier BETWEEN 0 AND 3),
    payout_enabled  BOOLEAN     NOT NULL DEFAULT FALSE,
    nsfw_access     BOOLEAN     NOT NULL DEFAULT FALSE,
    risk_score      REAL        NOT NULL DEFAULT 0.0 CHECK (risk_score BETWEEN 0.0 AND 1.0),
    required_actions TEXT[]     NOT NULL DEFAULT '{}',
    -- Cryptographic anchor: SHA-256(pial_id + status + age_band + tier + timestamp)
    decision_hash   TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,
    metadata        JSONB       NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_att_pial    ON attestations(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_att_status  ON attestations(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_att_context ON attestations(context, pial_id);

-- ── KYC submissions ───────────────────────────────────────────────────────────
-- Tracks verification attempts. Raw documents are NEVER stored here.
-- Only hashes, status, and non-sensitive extracted values.
CREATE TABLE IF NOT EXISTS kyc_submissions (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id         UUID        NOT NULL,
    submission_type TEXT        NOT NULL   -- gov_id | liveness | phone | email
                    CHECK (submission_type IN ('gov_id','liveness','phone','email','manual')),
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','processing','passed','failed','manual_review')),
    -- Hash of the submitted document (SHA-256) — document itself is never stored
    document_hash   TEXT,
    age_band        TEXT        DEFAULT 'unknown',
    confidence      REAL        DEFAULT 0.0,
    failure_reason  TEXT,
    provider        TEXT        NOT NULL DEFAULT 'internal', -- 'internal' | 'jumio' | 'persona' | etc
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at    TIMESTAMPTZ,
    metadata        JSONB       NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_kyc_pial   ON kyc_submissions(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_kyc_status ON kyc_submissions(status);

-- ── Risk profiles ─────────────────────────────────────────────────────────────
-- Per-PIAL aggregated fraud signals. Mutable — updated on each decision.
CREATE TABLE IF NOT EXISTS risk_profiles (
    pial_id             UUID    PRIMARY KEY,
    risk_score          REAL    NOT NULL DEFAULT 0.0,
    device_count        INTEGER NOT NULL DEFAULT 0,
    failed_kyc_attempts INTEGER NOT NULL DEFAULT 0,
    ip_reputation_score REAL    NOT NULL DEFAULT 1.0,  -- 1.0 = clean, 0.0 = datacenter/VPN
    velocity_score      REAL    NOT NULL DEFAULT 0.0,  -- 0.0 = normal, 1.0 = burst anomaly
    duplicate_detected  BOOLEAN NOT NULL DEFAULT FALSE,
    last_evaluated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Audit log ─────────────────────────────────────────────────────────────────
-- Append-only. Every decision recorded.
CREATE TABLE IF NOT EXISTS verity_audit (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id     UUID        NOT NULL,
    action      TEXT        NOT NULL,  -- decision | kyc_submit | risk_update | tier_change
    context     TEXT,
    before_tier INTEGER,
    after_tier  INTEGER,
    metadata    JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_audit_pial ON verity_audit(pial_id, created_at DESC);
"#;

const COMPLIANCE_SCHEMA: &str = r#"
-- ── 18 U.S.C. 2257 Records ──────────────────────────────────────────────────
-- One record per verified adult creator. Immutable once created.
-- We store a decision hash and document hash only — no ID images or biometrics.
-- Record-keeper statement is derived from this row at query time.
CREATE TABLE IF NOT EXISTS records_2257 (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_pial_id     UUID        NOT NULL UNIQUE,
    -- Linked to the KYC submission that established age verification
    kyc_submission_id   UUID        REFERENCES kyc_submissions(id),
    age_band            TEXT        NOT NULL CHECK (age_band IN ('18+','21+')),
    document_type       TEXT        NOT NULL DEFAULT 'gov_id',
    -- SHA-256 hash of the verified document — document itself is never stored
    document_hash       TEXT        NOT NULL,
    verification_date   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    record_keeper       TEXT        NOT NULL DEFAULT 'F33D3R Platform',
    -- Custodian URL per 18 USC 2257(f)(5)
    record_location     TEXT        NOT NULL DEFAULT 'https://f33d3r.com/legal/2257',
    -- SHA-256 of (creator_pial_id + age_band + verification_date) — audit anchor
    statement_hash      TEXT        NOT NULL,
    is_active           BOOLEAN     NOT NULL DEFAULT TRUE,
    revoked_at          TIMESTAMPTZ,
    revocation_reason   TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_2257_creator ON records_2257(creator_pial_id);
CREATE INDEX IF NOT EXISTS idx_2257_active  ON records_2257(is_active);

-- ── CSAM scan log ─────────────────────────────────────────────────────────────
-- Every media upload is checked. We store the result for audit and repeat-check avoidance.
-- In production this integrates with NCMEC / PhotoDNA. Local dev: stub returns "clean".
CREATE TABLE IF NOT EXISTS csam_scans (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Perceptual hash of the uploaded content (SHA-256 of raw bytes)
    content_hash    TEXT        NOT NULL,
    uploader_pial   UUID        NOT NULL,
    -- Provider: ncmec_stub | photodna | aws_rekognition
    scan_provider   TEXT        NOT NULL DEFAULT 'ncmec_stub',
    -- Result: clean | flagged | error | skipped
    result          TEXT        NOT NULL DEFAULT 'clean'
                    CHECK (result IN ('clean','flagged','error','skipped')),
    match_count     INTEGER     NOT NULL DEFAULT 0,
    -- If flagged: was a CyberTip report sent to NCMEC?
    report_sent     BOOLEAN     NOT NULL DEFAULT FALSE,
    report_sent_at  TIMESTAMPTZ,
    media_url       TEXT,
    media_type      TEXT,
    scanned_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_csam_hash     ON csam_scans(content_hash);
CREATE        INDEX IF NOT EXISTS idx_csam_flagged  ON csam_scans(result) WHERE result = 'flagged';
CREATE        INDEX IF NOT EXISTS idx_csam_uploader ON csam_scans(uploader_pial, scanned_at DESC);
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    sqlx::raw_sql(COMPLIANCE_SCHEMA).execute(pool).await?;
    Ok(())
}

/// Returns the latest attestation for a PIAL + context combination.
pub async fn get_latest_attestation(
    pool: &PgPool,
    pial_id: uuid::Uuid,
    context: &str,
) -> Option<crate::models::AttestationRow> {
    sqlx::query_as(
        "SELECT * FROM attestations WHERE pial_id = $1 AND context = $2
         ORDER BY created_at DESC LIMIT 1",
    )
    .bind(pial_id)
    .bind(context)
    .fetch_optional(pool)
    .await
    .unwrap_or(None)
}

/// Returns the current risk profile, or a default if none exists.
pub async fn get_risk_profile(
    pool: &PgPool,
    pial_id: uuid::Uuid,
) -> crate::models::RiskProfile {
    sqlx::query_as(
        "SELECT * FROM risk_profiles WHERE pial_id = $1",
    )
    .bind(pial_id)
    .fetch_optional(pool)
    .await
    .unwrap_or(None)
    .unwrap_or_else(|| crate::models::RiskProfile {
        pial_id,
        risk_score: 0.0,
        device_count: 0,
        failed_kyc_attempts: 0,
        ip_reputation_score: 1.0,
        velocity_score: 0.0,
        duplicate_detected: false,
        last_evaluated_at: chrono::Utc::now(),
        updated_at: chrono::Utc::now(),
    })
}

/// Upserts a risk profile.
pub async fn upsert_risk_profile(pool: &PgPool, p: &crate::models::RiskProfile) {
    let _ = sqlx::query(
        "INSERT INTO risk_profiles
            (pial_id, risk_score, device_count, failed_kyc_attempts,
             ip_reputation_score, velocity_score, duplicate_detected, last_evaluated_at, updated_at)
         VALUES ($1,$2,$3,$4,$5,$6,$7,NOW(),NOW())
         ON CONFLICT (pial_id) DO UPDATE SET
            risk_score          = EXCLUDED.risk_score,
            device_count        = EXCLUDED.device_count,
            failed_kyc_attempts = EXCLUDED.failed_kyc_attempts,
            ip_reputation_score = EXCLUDED.ip_reputation_score,
            velocity_score      = EXCLUDED.velocity_score,
            duplicate_detected  = EXCLUDED.duplicate_detected,
            last_evaluated_at   = NOW(),
            updated_at          = NOW()",
    )
    .bind(p.pial_id)
    .bind(p.risk_score)
    .bind(p.device_count)
    .bind(p.failed_kyc_attempts)
    .bind(p.ip_reputation_score)
    .bind(p.velocity_score)
    .bind(p.duplicate_detected)
    .execute(pool)
    .await;
}

/// Returns the 2257 record for a creator, if it exists and is active.
pub async fn get_2257_record(pool: &PgPool, creator_pial: uuid::Uuid) -> Option<crate::models::Record2257> {
    sqlx::query_as(
        "SELECT * FROM records_2257 WHERE creator_pial_id = $1 AND is_active = true",
    )
    .bind(creator_pial)
    .fetch_optional(pool)
    .await
    .unwrap_or(None)
}

/// Inserts a new 2257 record.
pub async fn create_2257_record(pool: &PgPool, r: &crate::models::Record2257) -> anyhow::Result<uuid::Uuid> {
    let id: uuid::Uuid = sqlx::query_scalar(
        "INSERT INTO records_2257
           (creator_pial_id, kyc_submission_id, age_band, document_type, document_hash,
            record_keeper, record_location, statement_hash)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id",
    )
    .bind(r.creator_pial_id)
    .bind(r.kyc_submission_id)
    .bind(&r.age_band)
    .bind(&r.document_type)
    .bind(&r.document_hash)
    .bind(&r.record_keeper)
    .bind(&r.record_location)
    .bind(&r.statement_hash)
    .fetch_one(pool)
    .await?;
    Ok(id)
}

/// Gets a CSAM scan result by content hash. Returns None if never scanned.
pub async fn get_csam_scan(pool: &PgPool, content_hash: &str) -> Option<crate::models::CsamScan> {
    sqlx::query_as(
        "SELECT * FROM csam_scans WHERE content_hash = $1",
    )
    .bind(content_hash)
    .fetch_optional(pool)
    .await
    .unwrap_or(None)
}

/// Records a CSAM scan result.
pub async fn record_csam_scan(
    pool: &PgPool,
    content_hash: &str,
    uploader_pial: uuid::Uuid,
    result: &str,
    match_count: i32,
    media_url: Option<&str>,
    media_type: Option<&str>,
) -> anyhow::Result<uuid::Uuid> {
    let id: uuid::Uuid = sqlx::query_scalar(
        "INSERT INTO csam_scans (content_hash, uploader_pial, result, match_count, media_url, media_type)
         VALUES ($1,$2,$3,$4,$5,$6) RETURNING id
         ON CONFLICT (content_hash) DO UPDATE SET
           result      = EXCLUDED.result,
           match_count = EXCLUDED.match_count,
           scanned_at  = NOW()",
    )
    .bind(content_hash)
    .bind(uploader_pial)
    .bind(result)
    .bind(match_count)
    .bind(media_url)
    .bind(media_type)
    .fetch_one(pool)
    .await?;
    Ok(id)
}

/// Appends an audit event.
pub async fn audit(
    pool: &PgPool,
    pial_id: uuid::Uuid,
    action: &str,
    context: Option<&str>,
    before_tier: Option<i32>,
    after_tier: Option<i32>,
    metadata: serde_json::Value,
) {
    let _ = sqlx::query(
        "INSERT INTO verity_audit (pial_id, action, context, before_tier, after_tier, metadata)
         VALUES ($1,$2,$3,$4,$5,$6)",
    )
    .bind(pial_id)
    .bind(action)
    .bind(context)
    .bind(before_tier)
    .bind(after_tier)
    .bind(metadata)
    .execute(pool)
    .await;
}
