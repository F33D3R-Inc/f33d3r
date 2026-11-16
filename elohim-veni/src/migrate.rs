//! Ordered, versioned, forward-only migrations for f33d3r_security.
//!
//! The same rules Manhattan and feed-engine run under (manhattan/src/migrate.rs,
//! feed-engine/internal/db/migrate.go):
//!
//!   * Every schema change is a numbered file in migrations/, embedded here in an
//!     explicitly ordered array.
//!   * Files apply in ascending version order, each inside its own transaction,
//!     together with the schema_migrations row that records it.
//!   * An applied file is frozen: its checksum is recorded, and a later edit is a
//!     hard boot failure, never a silent skip.
//!   * Forward-only. To undo, write a higher-numbered migration.
//!
//! Filenames: NNNN_snake_case_name.sql

use std::time::Instant;

use anyhow::{bail, Context, Result};
use sha2::{Digest, Sha256};
use sqlx::{Acquire, PgConnection, PgPool};

/// Serialises migration runs across every process that boots against
/// f33d3r_security. Fixed: changing it breaks the exclusion it provides.
const ADVISORY_LOCK_KEY: i64 = 7331_0010;

struct Migration {
    version: i64,
    name: &'static str,
    filename: &'static str,
    body: &'static str,
}

/// The migration plane. Append only; never reorder, never edit an applied entry.
const MIGRATIONS: &[Migration] = &[
    Migration {
        version: 1,
        name: "baseline",
        filename: "0001_baseline.sql",
        body: include_str!("../migrations/0001_baseline.sql"),
    },
    Migration {
        version: 2,
        name: "handle_registry",
        filename: "0002_handle_registry.sql",
        body: include_str!("../migrations/0002_handle_registry.sql"),
    },
    Migration {
        version: 3,
        name: "identity_authority",
        filename: "0003_identity_authority.sql",
        body: include_str!("../migrations/0003_identity_authority.sql"),
    },
    Migration {
        version: 4,
        name: "contact_personas",
        filename: "0004_contact_personas.sql",
        body: include_str!("../migrations/0004_contact_personas.sql"),
    },
];

fn checksum(body: &str) -> String {
    hex::encode(Sha256::digest(body.as_bytes()))
}

fn validate_plane() -> Result<()> {
    if MIGRATIONS.is_empty() {
        bail!("[migrate] no migrations embedded — the migration plane is empty");
    }
    let mut previous: i64 = 0;
    for m in MIGRATIONS {
        if m.version <= previous {
            bail!(
                "[migrate] migration {} (version {}) does not sort above version {} — \
                 the embedded array must be strictly ascending and versions unique",
                m.filename,
                m.version,
                previous
            );
        }
        if m.name.is_empty() || m.body.trim().is_empty() {
            bail!("[migrate] migration {}: name and body must not be empty", m.filename);
        }
        previous = m.version;
    }
    Ok(())
}

/// Brings the database to the latest schema version. Any failure is fatal: a
/// half-migrated identity database is not one this process may serve from.
pub async fn run(pool: &PgPool) -> Result<()> {
    validate_plane()?;

    // Detached: this connection runs with no statement timeout and holds a
    // session-scoped advisory lock, so it must never return to the pool.
    let mut conn = pool
        .acquire()
        .await
        .context("[migrate] cannot acquire a connection")?
        .detach();

    sqlx::query("SET statement_timeout = 0")
        .execute(&mut conn)
        .await
        .context("[migrate] cannot lift the statement timeout")?;
    sqlx::query("SELECT pg_advisory_lock($1)")
        .bind(ADVISORY_LOCK_KEY)
        .execute(&mut conn)
        .await
        .context("[migrate] cannot acquire migration lock")?;

    let outcome = run_locked(&mut conn).await;

    // The lock is released by closing the session whatever happens here; a
    // failure to release it explicitly is reported, never hidden.
    if let Err(e) = sqlx::query("SELECT pg_advisory_unlock($1)")
        .bind(ADVISORY_LOCK_KEY)
        .execute(&mut conn)
        .await
    {
        tracing::warn!(error = %e, "[migrate] releasing migration lock");
    }
    if let Err(e) = sqlx::Connection::close(conn).await {
        tracing::warn!(error = %e, "[migrate] closing migration connection");
    }
    outcome
}

async fn run_locked(conn: &mut PgConnection) -> Result<()> {
    sqlx::query(
        "CREATE TABLE IF NOT EXISTS schema_migrations (
            version     BIGINT      PRIMARY KEY,
            name        TEXT        NOT NULL,
            checksum    TEXT        NOT NULL,
            applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
            duration_ms BIGINT      NOT NULL DEFAULT 0
        )",
    )
    .execute(&mut *conn)
    .await
    .context("[migrate] cannot create schema_migrations")?;

    let applied: Vec<(i64, String)> =
        sqlx::query_as("SELECT version, checksum FROM schema_migrations")
            .fetch_all(&mut *conn)
            .await
            .context("[migrate] cannot read schema_migrations")?;
    let max_applied = applied.iter().map(|(v, _)| *v).max().unwrap_or(0);

    for (v, _) in &applied {
        if !MIGRATIONS.iter().any(|m| m.version == *v) {
            bail!(
                "[migrate] database records applied migration {v} but no such migration is \
                 embedded — the migration history and the source tree disagree"
            );
        }
    }

    let mut pending: Vec<&Migration> = Vec::new();
    for m in MIGRATIONS {
        let sum = checksum(m.body);
        match applied.iter().find(|(v, _)| *v == m.version) {
            None if m.version < max_applied => bail!(
                "[migrate] migration {} is unapplied but sorts below applied version {} — \
                 migrations are forward-only; renumber it above {}",
                m.filename,
                max_applied,
                max_applied
            ),
            None => pending.push(m),
            Some((_, recorded)) if *recorded != sum => bail!(
                "[migrate] migration {} was already applied with a different checksum \
                 (recorded {}, file {}) — applied migrations are frozen; write a new \
                 higher-numbered migration instead of editing this one",
                m.filename,
                &recorded[..12.min(recorded.len())],
                &sum[..12]
            ),
            Some(_) => {}
        }
    }

    if pending.is_empty() {
        tracing::info!(version = max_applied, "[migrate] schema up to date");
        return Ok(());
    }
    for m in pending {
        apply(&mut *conn, m).await?;
    }
    Ok(())
}

async fn apply(conn: &mut PgConnection, m: &Migration) -> Result<()> {
    tracing::info!(version = m.version, name = m.name, "[migrate] applying");
    let start = Instant::now();
    let mut tx = conn
        .begin()
        .await
        .with_context(|| format!("[migrate] migration {}: cannot begin transaction", m.filename))?;

    sqlx::raw_sql(m.body)
        .execute(&mut *tx)
        .await
        .with_context(|| format!("[migrate] migration {} failed", m.filename))?;

    sqlx::query(
        "INSERT INTO schema_migrations (version, name, checksum, duration_ms)
         VALUES ($1, $2, $3, $4)",
    )
    .bind(m.version)
    .bind(m.name)
    .bind(checksum(m.body))
    .bind(start.elapsed().as_millis() as i64)
    .execute(&mut *tx)
    .await
    .with_context(|| format!("[migrate] migration {}: cannot record version", m.filename))?;

    tx.commit()
        .await
        .with_context(|| format!("[migrate] migration {}: commit failed", m.filename))?;
    tracing::info!(
        version = m.version,
        name = m.name,
        duration_ms = start.elapsed().as_millis() as i64,
        "[migrate] applied"
    );
    Ok(())
}
