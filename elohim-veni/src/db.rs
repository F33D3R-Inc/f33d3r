/// Elohim Veni — Security Brain database access.
///
/// The schema lives in elohim-veni/migrations. Design principles:
///   - PIAL (Principal Identity Access Layer) is the root of all trust decisions.
///   - Capability grants are explicit and auditable.
///   - Every decision is logged — the audit trail is immutable.
///   - Trust scores are derived signals, updated after each decision.
///   - Schema is additive-only for forward compatibility.
use anyhow::Result;
use sqlx::PgPool;

/// Brings f33d3r_security to the latest schema version.
///
/// The schema used to be four idempotent blobs replayed on every boot, with no
/// version and no record of what ran. Those blobs are now migrations/0001_baseline.sql,
/// verbatim, and every change after them is a numbered, checksummed, forward-only
/// migration applied by `crate::migrate`.
pub async fn migrate(pool: &PgPool) -> Result<()> {
    crate::migrate::run(pool).await
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
        "SELECT COUNT(*) FROM decision_log WHERE created_at >= NOW() - INTERVAL '24 hours'",
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}

/// Count DENY decisions in the last 24 hours (for deny-rate metric).
pub async fn count_denials_today(pool: &PgPool) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM decision_log \
         WHERE decision = 'DENY' AND created_at >= NOW() - INTERVAL '24 hours'",
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}

/// Upsert a PIAL ECDSA signing public key.
///
/// New key material is a new key, so it gets a new key_id and therefore its own
/// node and `signs_for` edge in the graph. Re-registering the key already on
/// file keeps the id it was registered under: the graph must not grow a second
/// node for one key.
///
/// The trigger on this table enqueues that registration in this same
/// transaction. Nothing here calls Manhattan directly — a write that only
/// happened in HTTP is a write a restart loses.
pub async fn upsert_signing_key(
    pool: &PgPool,
    pial_id: uuid::Uuid,
    public_key_b64: &str,
    algorithm: &str,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO pial_signing_keys (pial_id, public_key_b64, algorithm, updated_at)
         VALUES ($1, $2, $3, NOW())
         ON CONFLICT (pial_id) DO UPDATE
           SET key_id = CASE
                            WHEN pial_signing_keys.public_key_b64 IS DISTINCT FROM EXCLUDED.public_key_b64
                            THEN gen_random_uuid()
                            ELSE pial_signing_keys.key_id
                        END,
               public_key_b64 = EXCLUDED.public_key_b64,
               algorithm = EXCLUDED.algorithm,
               updated_at = NOW()"
    ).bind(pial_id).bind(public_key_b64).bind(algorithm)
     .execute(pool).await?;
    Ok(())
}

/// Fetch a PIAL ECDSA signing public key, returning None if not found.
/// Append one decision to the immutable audit trail.
#[allow(clippy::too_many_arguments)]
pub async fn log_decision(
    pool: &PgPool,
    pial_id: uuid::Uuid,
    action: &str,
    decision: &str,
    confidence: f64,
    reason: &str,
    capability_required: Option<&str>,
    content_risk: f64,
    anomaly_score: f64,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO decision_log \
         (pial_id, action, decision, confidence, reason, capability_required, content_risk, anomaly_score) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
    )
    .bind(pial_id)
    .bind(action)
    .bind(decision)
    .bind(confidence)
    .bind(reason)
    .bind(capability_required)
    .bind(content_risk)
    .bind(anomaly_score)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn get_signing_key(pool: &PgPool, pial_id: uuid::Uuid) -> Result<Option<String>> {
    let row = sqlx::query_scalar::<_, String>(
        "SELECT public_key_b64 FROM pial_signing_keys WHERE pial_id = $1",
    )
    .bind(pial_id)
    .fetch_optional(pool)
    .await?;
    Ok(row)
}

/// Upsert a PIAL ECDH public key. Rotation mints a new key_id for the same
/// reason it does for signing keys.
pub async fn upsert_ecdh_key(
    pool: &PgPool,
    pial_id: uuid::Uuid,
    public_key_b64: &str,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO pial_ecdh_keys (pial_id, public_key_b64, updated_at)
         VALUES ($1, $2, NOW())
         ON CONFLICT (pial_id) DO UPDATE
           SET key_id = CASE
                            WHEN pial_ecdh_keys.public_key_b64 IS DISTINCT FROM EXCLUDED.public_key_b64
                            THEN gen_random_uuid()
                            ELSE pial_ecdh_keys.key_id
                        END,
               public_key_b64 = EXCLUDED.public_key_b64,
               updated_at = NOW()"
    ).bind(pial_id).bind(public_key_b64)
     .execute(pool).await?;
    Ok(())
}

/// Fetch a PIAL ECDH public key, returning None if not found.
pub async fn get_ecdh_key(pool: &PgPool, pial_id: uuid::Uuid) -> Result<Option<String>> {
    let row = sqlx::query_scalar::<_, String>(
        "SELECT public_key_b64 FROM pial_ecdh_keys WHERE pial_id = $1",
    )
    .bind(pial_id)
    .fetch_optional(pool)
    .await?;
    Ok(row)
}

// ── Manhattan outbox ──────────────────────────────────────────────────────────

/// What the outbox has not yet delivered. Depth alone says a queue is behind;
/// the age of the head and the error it last failed with say whether it is
/// moving. All three are reported by /health so a stalled plane is visible on
/// the tick it stalls rather than the day someone notices a missing name.
pub struct OutboxBacklog {
    pub pending: i64,
    pub oldest_age_seconds: Option<f64>,
    pub head_last_error: Option<String>,
    /// Rows the drain has taken out of the queue because they cannot be
    /// delivered. Counted separately and still counted in `pending`: a
    /// quarantined row is a naming-plane write that did NOT happen, so it must
    /// not vanish from the depth this endpoint reports.
    pub quarantined: i64,
}

pub async fn outbox_backlog(pool: &PgPool) -> Result<OutboxBacklog> {
    let row: (i64, Option<f64>, Option<String>, i64) = sqlx::query_as(
        "SELECT COUNT(*)::BIGINT,
                EXTRACT(EPOCH FROM (NOW() - MIN(created_at)))::DOUBLE PRECISION,
                (SELECT last_error FROM manhattan_outbox
                  WHERE delivered_at IS NULL AND blocked_at IS NULL ORDER BY id LIMIT 1),
                COUNT(*) FILTER (WHERE blocked_at IS NOT NULL)::BIGINT
           FROM manhattan_outbox
          WHERE delivered_at IS NULL",
    )
    .fetch_one(pool)
    .await?;
    Ok(OutboxBacklog {
        pending: row.0,
        oldest_age_seconds: row.1,
        head_last_error: row.2,
        quarantined: row.3,
    })
}
