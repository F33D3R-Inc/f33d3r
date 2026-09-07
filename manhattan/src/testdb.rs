//! A throwaway Postgres per test, for the SQL in `db.rs`.
//!
//! Placement, the never-reissued rule and the self-reclaim waiver are all pure
//! SQL, and none of it can be proved by a unit test that never talks to the
//! database: the properties at stake are what Postgres does under two
//! concurrent transactions, against its own clock. So every test here runs
//! against a real server, in a database created for that test and dropped
//! after it.
//!
//! The server is named by `MANHATTAN_TEST_DATABASE_URL` — a connection string
//! for a role allowed to `CREATE DATABASE`, pointed at any existing database
//! on that server (the `postgres` maintenance database is the usual choice).
//! When the variable is unset the tests report themselves skipped and pass;
//! they never fake a database. Same convention as feed-engine's
//! `PURGE_TEST_DATABASE_URL`.
//!
//!     MANHATTAN_TEST_DATABASE_URL=postgres://f33d3r:f33d3rdev@localhost:5432/postgres \
//!         cargo test
//!
//! A test that panics leaves its database behind (a drop cannot run from a
//! panic in an async test). They are all named `manhattan_test_*` and can be
//! removed with
//!
//!     SELECT 'DROP DATABASE ' || datname FROM pg_database
//!      WHERE datname LIKE 'manhattan_test_%' \gexec

use std::str::FromStr;

use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::PgPool;
use uuid::Uuid;

pub const ENV: &str = "MANHATTAN_TEST_DATABASE_URL";

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
    let name = format!("manhattan_test_{}", Uuid::new_v4().simple());

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
    // Enough connections that a concurrency test's tasks each hold one and
    // still leave one for the assertions that follow them.
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
    ///
    /// Dropped WITH (FORCE): closing the pool asks every connection to hang
    /// up, but the server may still be tearing a session down when the drop
    /// arrives, and a listener task (see `cache.rs`) never hangs up at all.
    /// This is a throwaway database; whoever is still in it is done.
    pub async fn finish(self) {
        self.pool.close().await;
        sqlx::query(&format!("DROP DATABASE {} WITH (FORCE)", self.name))
            .execute(&self.admin)
            .await
            .unwrap_or_else(|e| panic!("dropping {}: {e}", self.name));
    }

    /// A node of the given kind, owned by the brain the authority map names.
    pub async fn node(&self, kind: &str) -> Uuid {
        sqlx::query_scalar(
            "INSERT INTO nodes (kind, owner)
             VALUES ($1, COALESCE((SELECT owner FROM node_kind_owners WHERE kind = $1), 'test'))
             RETURNING node_id",
        )
        .bind(kind)
        .fetch_one(&self.pool)
        .await
        .expect("insert node")
    }

    /// A node bound to `name` in `namespace`, as its primary name.
    pub async fn named(&self, kind: &str, namespace: &str, name: &str) -> Uuid {
        let id = self.node(kind).await;
        sqlx::query(
            "INSERT INTO names (name, node_id, namespace, is_primary)
             VALUES ($1::citext, $2, $3, TRUE)",
        )
        .bind(name)
        .bind(id)
        .bind(namespace)
        .execute(&self.pool)
        .await
        .expect("insert name");
        id
    }

    /// Retires the active binding of `name`, back-dating the revocation by
    /// `ago_seconds` so a quarantine can be observed from both sides.
    pub async fn revoke(&self, name: &str, ago_seconds: i64) {
        let n = sqlx::query(
            "UPDATE names
                SET status = 'revoked', is_primary = FALSE,
                    revoked_at = NOW() - ($2 * INTERVAL '1 second')
              WHERE name = $1::citext AND status = 'active'",
        )
        .bind(name)
        .bind(ago_seconds)
        .execute(&self.pool)
        .await
        .expect("revoke name")
        .rows_affected();
        assert_eq!(n, 1, "revoking {name}: expected one active row");
    }
}
