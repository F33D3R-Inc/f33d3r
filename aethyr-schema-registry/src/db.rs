//! Database layer for the schema registry.
//! All queries are typed and parameterised — no raw string interpolation.

use anyhow::Result;
use chrono::{DateTime, Utc};
use sqlx::{PgPool, Row};
use uuid::Uuid;

use crate::api::types::{BrainRegistrationRecord, CompatibilityLogEntry, SchemaRecord};

// ── Connection pool ───────────────────────────────────────────────────────────

pub async fn connect(url: &str, max_connections: u32) -> Result<PgPool> {
    let pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(max_connections)
        .acquire_timeout(std::time::Duration::from_secs(5))
        .connect(url)
        .await?;
    tracing::info!("connected to schema registry database");
    Ok(pool)
}

pub async fn run_migrations(pool: &PgPool) -> Result<()> {
    let sql = include_str!("../migrations/001_initial.sql");
    sqlx::raw_sql(sql).execute(pool).await?;
    tracing::info!("migrations applied");
    Ok(())
}

// ── Brain registrations ───────────────────────────────────────────────────────

pub async fn upsert_brain(pool: &PgPool, reg: &BrainRegistrationRecord) -> Result<()> {
    sqlx::query(
        r#"
        INSERT INTO brain_registrations
            (brain_id, current_version, port, host, signal_types, description, status, last_seen)
        VALUES ($1, $2, $3, $4, $5, $6, 'online', NOW())
        ON CONFLICT (brain_id) DO UPDATE SET
            current_version = EXCLUDED.current_version,
            port            = EXCLUDED.port,
            host            = EXCLUDED.host,
            signal_types    = EXCLUDED.signal_types,
            description     = EXCLUDED.description,
            status          = 'online',
            last_seen       = NOW()
        "#,
    )
    .bind(&reg.brain_id)
    .bind(reg.current_version)
    .bind(reg.port)
    .bind(&reg.host)
    .bind(&reg.signal_types)
    .bind(&reg.description)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn update_brain_heartbeat(pool: &PgPool, brain_id: &str) -> Result<()> {
    sqlx::query(
        "UPDATE brain_registrations SET last_seen = NOW(), status = 'online' WHERE brain_id = $1",
    )
    .bind(brain_id)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn get_all_brains(pool: &PgPool) -> Result<Vec<BrainRegistrationRecord>> {
    let rows = sqlx::query(
        r#"
        SELECT brain_id, current_version, port, host, signal_types, description, status, last_seen
        FROM brain_registrations
        ORDER BY port ASC
        "#,
    )
    .fetch_all(pool)
    .await?;

    let mut results = Vec::with_capacity(rows.len());
    for row in rows {
        results.push(BrainRegistrationRecord {
            brain_id: row.try_get("brain_id")?,
            current_version: row.try_get("current_version")?,
            port: row.try_get("port")?,
            host: row.try_get("host")?,
            signal_types: row.try_get::<Vec<String>, _>("signal_types")?,
            description: row.try_get("description")?,
            status: row.try_get("status")?,
            last_seen: row.try_get("last_seen")?,
        });
    }
    Ok(results)
}

pub async fn mark_stale_brains(pool: &PgPool, stale_secs: i64) -> Result<u64> {
    let result = sqlx::query(
        r#"
        UPDATE brain_registrations
        SET status = CASE
            WHEN EXTRACT(EPOCH FROM (NOW() - last_seen)) > $1 * 2 THEN 'offline'
            WHEN EXTRACT(EPOCH FROM (NOW() - last_seen)) > $1     THEN 'degraded'
            ELSE status
        END
        WHERE status NOT IN ('planned', 'offline')
          AND EXTRACT(EPOCH FROM (NOW() - last_seen)) > $1
        "#,
    )
    .bind(stale_secs)
    .execute(pool)
    .await?;
    Ok(result.rows_affected())
}

// ── Schema storage ────────────────────────────────────────────────────────────

pub async fn insert_schema(
    pool: &PgPool,
    brain_id: &str,
    signal_type: &str,
    version: i32,
    schema_json: &serde_json::Value,
    description: &str,
    registered_by: &str,
) -> Result<Uuid> {
    let row = sqlx::query(
        r#"
        INSERT INTO schemas (brain_id, signal_type, version, schema_json, description, registered_by)
        VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (brain_id, signal_type, version) DO UPDATE
        SET schema_json   = EXCLUDED.schema_json,
            description   = EXCLUDED.description,
            registered_by = EXCLUDED.registered_by,
            registered_at = NOW()
        RETURNING id
        "#,
    )
    .bind(brain_id)
    .bind(signal_type)
    .bind(version)
    .bind(schema_json)
    .bind(description)
    .bind(registered_by)
    .fetch_one(pool)
    .await?;

    Ok(row.try_get("id")?)
}

pub async fn get_schema(
    pool: &PgPool,
    brain_id: &str,
    signal_type: &str,
    version: Option<i32>,
) -> Result<Option<SchemaRecord>> {
    let row = if let Some(v) = version {
        sqlx::query(
            r#"
            SELECT id, brain_id, signal_type, version, schema_json, description, registered_at
            FROM schemas
            WHERE brain_id = $1 AND signal_type = $2 AND version = $3
            "#,
        )
        .bind(brain_id)
        .bind(signal_type)
        .bind(v)
        .fetch_optional(pool)
        .await?
    } else {
        // Latest version
        sqlx::query(
            r#"
            SELECT id, brain_id, signal_type, version, schema_json, description, registered_at
            FROM schemas
            WHERE brain_id = $1 AND signal_type = $2
            ORDER BY version DESC
            LIMIT 1
            "#,
        )
        .bind(brain_id)
        .bind(signal_type)
        .fetch_optional(pool)
        .await?
    };

    match row {
        None => Ok(None),
        Some(r) => Ok(Some(SchemaRecord {
            id: r.try_get("id")?,
            brain_id: r.try_get("brain_id")?,
            signal_type: r.try_get("signal_type")?,
            version: r.try_get("version")?,
            schema_json: r.try_get("schema_json")?,
            description: r.try_get("description")?,
            registered_at: r.try_get("registered_at")?,
        })),
    }
}

pub async fn get_all_versions(
    pool: &PgPool,
    brain_id: &str,
    signal_type: &str,
) -> Result<Vec<SchemaRecord>> {
    let rows = sqlx::query(
        r#"
        SELECT id, brain_id, signal_type, version, schema_json, description, registered_at
        FROM schemas
        WHERE brain_id = $1 AND signal_type = $2
        ORDER BY version DESC
        "#,
    )
    .bind(brain_id)
    .bind(signal_type)
    .fetch_all(pool)
    .await?;

    let mut records = Vec::with_capacity(rows.len());
    for r in rows {
        records.push(SchemaRecord {
            id: r.try_get("id")?,
            brain_id: r.try_get("brain_id")?,
            signal_type: r.try_get("signal_type")?,
            version: r.try_get("version")?,
            schema_json: r.try_get("schema_json")?,
            description: r.try_get("description")?,
            registered_at: r.try_get("registered_at")?,
        });
    }
    Ok(records)
}

// ── Compatibility log ─────────────────────────────────────────────────────────

pub async fn log_compatibility(
    pool: &PgPool,
    brain_id: &str,
    signal_type: &str,
    from_version: Option<i32>,
    to_version: i32,
    is_compatible: bool,
    violations: &serde_json::Value,
) -> Result<()> {
    sqlx::query(
        r#"
        INSERT INTO compatibility_log
            (brain_id, signal_type, from_version, to_version, is_compatible, violations)
        VALUES ($1, $2, $3, $4, $5, $6)
        "#,
    )
    .bind(brain_id)
    .bind(signal_type)
    .bind(from_version)
    .bind(to_version)
    .bind(is_compatible)
    .bind(violations)
    .execute(pool)
    .await?;
    Ok(())
}
