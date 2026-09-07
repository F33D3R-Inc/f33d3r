//! Ordered, versioned, forward-only migrations.
//!
//! manhattan/src/migrate.rs, verbatim in semantics. The rules, identical to
//! feed-engine/internal/db/migrate.go:
//!
//!   * Every schema change is a numbered file in migrations/, embedded here in an
//!     explicitly ordered array. `sqlx::migrate!` is deliberately not used — the
//!     checksum-drift-is-fatal semantics below are the point, not a nicety.
//!   * Files apply in ascending version order, each inside its own transaction.
//!   * An applied file is frozen: its checksum is recorded, and a later edit is a hard
//!     boot failure, never a silent skip.
//!   * Forward-only. There is no down path. To undo, write a higher-numbered migration.
//!
//! Filenames: NNNN_snake_case_name.sql

use std::time::Instant;

use anyhow::{bail, Context, Result};
use sha2::{Digest, Sha256};
use sqlx::{Acquire, PgConnection, PgPool};

/// Serialises migration runs across every process that boots against this database.
/// Arbitrary but fixed — changing it breaks the mutual exclusion it exists to provide.
/// 7331_0001 is feed-engine's, 7331_0002 Manhattan's.
const ADVISORY_LOCK_KEY: i64 = 7331_0003;

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
        name: "frequencies",
        filename: "0001_frequencies.sql",
        body: include_str!("../migrations/0001_frequencies.sql"),
    },
    Migration {
        version: 2,
        name: "participants",
        filename: "0002_participants.sql",
        body: include_str!("../migrations/0002_participants.sql"),
    },
    Migration {
        version: 3,
        name: "events_outbox",
        filename: "0003_events_outbox.sql",
        body: include_str!("../migrations/0003_events_outbox.sql"),
    },
    Migration {
        version: 4,
        name: "manhattan_outbox",
        filename: "0004_manhattan_outbox.sql",
        body: include_str!("../migrations/0004_manhattan_outbox.sql"),
    },
    Migration {
        version: 5,
        name: "recordings",
        filename: "0005_recordings.sql",
        body: include_str!("../migrations/0005_recordings.sql"),
    },
];

fn checksum(body: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(body.as_bytes());
    hex::encode(hasher.finalize())
}

/// Validates the embedded plane itself. A migration plane you cannot fully parse is a
/// migration plane you cannot trust, so every one of these is fatal.
fn validate_plane() -> Result<()> {
    if MIGRATIONS.is_empty() {
        bail!("[migrate] no migrations embedded — the migration plane is empty");
    }
    let mut previous: i64 = 0;
    for m in MIGRATIONS {
        if m.version <= 0 {
            bail!(
                "[migrate] migration {}: version must be a positive integer",
                m.filename
            );
        }
        if m.version <= previous {
            bail!(
                "[migrate] migration {} (version {}) does not sort above version {} — \
                 the embedded array must be strictly ascending and versions unique",
                m.filename,
                m.version,
                previous
            );
        }
        if m.name.is_empty() {
            bail!("[migrate] migration {}: name must not be empty", m.filename);
        }
        if m.body.trim().is_empty() {
            bail!("[migrate] migration {}: body is empty", m.filename);
        }
        previous = m.version;
    }
    Ok(())
}

/// Brings the database to the latest schema version.
///
/// Safe to run on every boot: already-applied versions are skipped, pending ones are
/// applied in order. Any failure is fatal — a half-migrated database is not a database
/// this process is allowed to serve from.
pub async fn run(pool: &PgPool) -> Result<()> {
    validate_plane()?;

    // One migrator at a time, across every process pointed at this database.
    // Detached from the pool: this connection is configured for migrating —
    // no statement timeout — and must not go back into the pool that serves
    // requests. It is closed when this function returns.
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

    let mut pending: Vec<&Migration> = Vec::new();
    for m in MIGRATIONS {
        let sum = checksum(m.body);
        match applied.iter().find(|(v, _)| *v == m.version) {
            None => {
                if m.version < max_applied {
                    bail!(
                        "[migrate] migration {} is unapplied but sorts below applied version {} \
                         — migrations are forward-only; renumber it above {}",
                        m.filename,
                        max_applied,
                        max_applied
                    );
                }
                pending.push(m);
            }
            Some((_, recorded)) if *recorded != sum => {
                bail!(
                    "[migrate] migration {} was already applied with a different checksum \
                     (recorded {}, file {}) — applied migrations are frozen; write a new \
                     higher-numbered migration instead of editing this one",
                    m.filename,
                    &recorded[..12.min(recorded.len())],
                    &sum[..12.min(sum.len())]
                );
            }
            Some(_) => {}
        }
    }

    for (v, _) in &applied {
        if !MIGRATIONS.iter().any(|m| m.version == *v) {
            bail!(
                "[migrate] database records applied migration {} but no such migration is \
                 embedded — the migration history and the source tree disagree",
                v
            );
        }
    }

    if pending.is_empty() {
        tracing::info!(
            version = max_applied,
            applied = applied.len(),
            "[migrate] schema up to date"
        );
        return Ok(());
    }

    let count = pending.len();
    let mut last = max_applied;
    for m in pending {
        apply(&mut *conn, m).await?;
        last = m.version;
    }
    tracing::info!(
        version = last,
        applied_this_boot = count,
        "[migrate] schema migrated"
    );
    Ok(())
}

/// Runs one migration and records it, atomically.
async fn apply(conn: &mut PgConnection, m: &Migration) -> Result<()> {
    tracing::info!(version = m.version, name = m.name, "[migrate] applying");
    let start = Instant::now();

    let mut tx = conn.begin().await.with_context(|| {
        format!(
            "[migrate] migration {}: cannot begin transaction",
            m.filename
        )
    })?;

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

/// Highest applied migration version, or 0 if the plane has not been initialised.
/// Exposed for /health reporting.
pub async fn schema_version(pool: &PgPool) -> Result<i64> {
    let v: Option<i64> = sqlx::query_scalar("SELECT MAX(version) FROM schema_migrations")
        .fetch_one(pool)
        .await
        .context("[migrate] cannot read schema version")?;
    Ok(v.unwrap_or(0))
}
