//! The connection to f33d3r_media.
//!
//! postgres-init/01-create-databases.sql creates the database on a fresh
//! cluster, but initdb scripts run exactly once, on an empty data volume. A
//! cluster that existed before caeor had a database never runs them again, so
//! caeor creates its own database when it finds it missing. Without that, the
//! first deploy of this brain onto a live cluster would be a boot loop.

use std::str::FromStr;
use std::time::Duration;

use anyhow::{bail, Context, Result};
use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::{ConnectOptions, Connection, PgPool};

/// SQLSTATE invalid_catalog_name: the database named in the URL does not exist.
const NO_SUCH_DATABASE: &str = "3D000";
/// SQLSTATE duplicate_database: someone else created it first; that is success.
const DUPLICATE_DATABASE: &str = "42P04";

pub async fn connect(database_url: &str, max_connections: u32) -> Result<PgPool> {
    let opts = PgConnectOptions::from_str(database_url).context("DATABASE_URL does not parse")?;
    ensure_database(&opts).await?;
    let pool = PgPoolOptions::new()
        .max_connections(max_connections)
        .acquire_timeout(Duration::from_secs(10))
        .connect_with(opts)
        .await
        .context("opening the f33d3r_media pool")?;
    Ok(pool)
}

async fn ensure_database(opts: &PgConnectOptions) -> Result<()> {
    let name = opts
        .get_database()
        .context("DATABASE_URL names no database")?
        .to_string();
    match opts.connect().await {
        Ok(conn) => {
            conn.close().await.context("closing the probe connection")?;
            return Ok(());
        }
        Err(sqlx::Error::Database(e)) if e.code().as_deref() == Some(NO_SUCH_DATABASE) => {}
        Err(e) => return Err(e).context("connecting to the media database"),
    }

    if !name.chars().all(|c| c.is_ascii_alphanumeric() || c == '_') {
        bail!("database name {name:?} is not a plain identifier; refusing to create it");
    }
    tracing::warn!(database = %name, "media database does not exist; creating it");
    let mut admin = opts
        .clone()
        .database("postgres")
        .connect()
        .await
        .context("connecting to the maintenance database to create the media database")?;
    let created = sqlx::query(&format!("CREATE DATABASE \"{name}\""))
        .execute(&mut admin)
        .await;
    admin.close().await.context("closing the maintenance connection")?;
    match created {
        Ok(_) => {
            tracing::info!(database = %name, "media database created");
            Ok(())
        }
        Err(sqlx::Error::Database(e)) if e.code().as_deref() == Some(DUPLICATE_DATABASE) => Ok(()),
        Err(e) => Err(e).context("creating the media database"),
    }
}

/// True when a sqlx error is a unique violation on the named constraint/index.
pub fn is_unique_violation(err: &sqlx::Error, constraint: &str) -> bool {
    match err {
        sqlx::Error::Database(e) => {
            e.code().as_deref() == Some("23505") && e.constraint() == Some(constraint)
        }
        _ => false,
    }
}
