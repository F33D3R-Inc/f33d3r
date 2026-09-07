//! A throwaway Postgres per test, for the SQL in `repository::postgres` and the
//! operations in `api::ops`.
//!
//! The compare-and-set, the one-open-per-host index, the outbox row landing
//! in the same transaction as the state change — none of it can be proved by
//! a unit test that never talks to the database. So every test here runs
//! against a real server, in a database created for that test and dropped
//! after it.
//!
//! The server is named by `AURALIS_TEST_DATABASE_URL` — a connection string
//! for a role allowed to `CREATE DATABASE`, pointed at any existing database
//! on that server. When the variable is unset the tests report themselves
//! skipped and pass; they never fake a database. Same convention as
//! Manhattan's `MANHATTAN_TEST_DATABASE_URL`.
//!
//!     AURALIS_TEST_DATABASE_URL=postgres://f33d3r:f33d3rdev@localhost:5432/postgres \
//!         cargo test
//!
//! A test that panics leaves its database behind. They are all named
//! `auralis_test_*` and can be removed with
//!
//!     SELECT 'DROP DATABASE ' || datname FROM pg_database
//!      WHERE datname LIKE 'auralis_test_%' \gexec

use std::str::FromStr;

use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::PgPool;
use uuid::Uuid;

pub const ENV: &str = "AURALIS_TEST_DATABASE_URL";

pub struct TestDb {
    pub pool: PgPool,
    admin: PgPool,
    name: String,
}

/// A fresh database with every migration applied, or `None` when no server is
/// configured. Callers spell the skip out so it is visible in the test output:
///
///     let Some(db) = testdb::fresh().await else { return };
pub async fn fresh() -> Option<TestDb> {
    let url = match std::env::var(ENV) {
        Ok(u) if !u.trim().is_empty() => u,
        _ => {
            eprintln!("{ENV} not set — database-backed test skipped");
            return None;
        }
    };
    let name = format!("auralis_test_{}", Uuid::new_v4().simple());

    let admin = PgPoolOptions::new()
        .max_connections(1)
        .connect(&url)
        .await
        .unwrap_or_else(|e| panic!("connecting to {ENV}: {e}"));
    sqlx::query(&format!("CREATE DATABASE {name}"))
        .execute(&admin)
        .await
        .unwrap_or_else(|e| panic!("creating {name}: {e}"));

    let options = PgConnectOptions::from_str(&url)
        .unwrap_or_else(|e| panic!("parsing {ENV}: {e}"))
        .database(&name);
    let pool = PgPoolOptions::new()
        .max_connections(24)
        .connect_with(options)
        .await
        .unwrap_or_else(|e| panic!("connecting to {name}: {e}"));
    crate::migrate::run(&pool)
        .await
        .unwrap_or_else(|e| panic!("migrating {name}: {e}"));

    Some(TestDb { pool, admin, name })
}

impl TestDb {
    /// Drops the database. Called at the end of every test that got one.
    pub async fn finish(self) {
        self.pool.close().await;
        sqlx::query(&format!("DROP DATABASE {} WITH (FORCE)", self.name))
            .execute(&self.admin)
            .await
            .unwrap_or_else(|e| panic!("dropping {}: {e}", self.name));
    }
}
