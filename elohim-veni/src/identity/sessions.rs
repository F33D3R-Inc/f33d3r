//! Sessions: an opaque bearer token held by the client, a row here.
//!
//! The token is 32 random bytes, base64url, and only its SHA-256 is stored, so
//! nothing that reads this database can present a session. A session lasts 30
//! days from login and ends early only by revocation; revoked and expired rows
//! are kept for a while as evidence and then reaped (maintenance.rs).

use anyhow::Result;
use base64::engine::general_purpose::URL_SAFE_NO_PAD as B64URL;
use base64::Engine;
use chrono::{DateTime, Utc};
use rand::rngs::OsRng;
use rand::RngCore;
use serde::Serialize;
use sha2::{Digest, Sha256};
use sqlx::PgConnection;
use uuid::Uuid;

/// last_seen_at is written at most this often per session, so resolving a
/// session on every request is a read, not a write.
const SEEN_GRANULARITY_SECS: i64 = 60;

/// A fresh opaque bearer secret: 32 random bytes, base64url.
pub fn new_secret() -> String {
    let mut raw = [0u8; 32];
    OsRng.fill_bytes(&mut raw);
    B64URL.encode(raw)
}

pub fn hash_token(token: &str) -> Vec<u8> {
    Sha256::digest(token.as_bytes()).to_vec()
}

pub struct NewSession {
    pub id: Uuid,
    pub token: String,
    pub expires_at: DateTime<Utc>,
}

/// Opens a session for `account_id`, acting as that account.
pub async fn create(
    conn: &mut PgConnection,
    account_id: Uuid,
    pial_id: Uuid,
    device_name: &str,
    ip: &str,
) -> Result<NewSession> {
    let token = new_secret();
    let device: String = device_name.trim().chars().take(100).collect();
    let ip: String = ip.trim().chars().take(64).collect();
    let (id, expires_at): (Uuid, DateTime<Utc>) = sqlx::query_as(
        "INSERT INTO sessions (token_sha256, account_id, login_account_id, pial_id, device_name, ip)
         VALUES ($1, $2, $2, $3, $4, $5)
         RETURNING id, expires_at",
    )
    .bind(hash_token(&token))
    .bind(account_id)
    .bind(pial_id)
    .bind(device)
    .bind(ip)
    .fetch_one(conn)
    .await?;
    Ok(NewSession { id, token, expires_at })
}

#[derive(Debug, sqlx::FromRow)]
pub struct LiveSession {
    pub id: Uuid,
    pub account_id: Uuid,
    pub login_account_id: Uuid,
    pub created_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
}

/// The live session a token opens, or None: unknown, revoked and expired are
/// one answer.
pub async fn find_live(conn: &mut PgConnection, token: &str) -> Result<Option<LiveSession>> {
    let found = sqlx::query_as::<_, LiveSession>(
        "SELECT id, account_id, login_account_id, created_at, expires_at, last_seen_at
           FROM sessions
          WHERE token_sha256 = $1 AND revoked_at IS NULL AND expires_at > NOW()",
    )
    .bind(hash_token(token))
    .fetch_optional(&mut *conn)
    .await?;
    if let Some(s) = &found {
        if (Utc::now() - s.last_seen_at).num_seconds() >= SEEN_GRANULARITY_SECS {
            sqlx::query("UPDATE sessions SET last_seen_at = NOW() WHERE id = $1")
                .bind(s.id)
                .execute(&mut *conn)
                .await?;
        }
    }
    Ok(found)
}

async fn revoke_where(conn: &mut PgConnection, predicate: &str, reason: &str, binds: Binds<'_>) -> Result<u64> {
    let sql = format!(
        "UPDATE sessions SET revoked_at = NOW(), revoked_reason = $1
          WHERE revoked_at IS NULL AND ({predicate})"
    );
    let mut q = sqlx::query(&sql).bind(reason);
    q = match binds {
        Binds::Hash(h) => q.bind(h),
        Binds::Id(id) => q.bind(id),
        Binds::IdId(a, b) => q.bind(a).bind(b),
        Binds::IdHash(a, h) => q.bind(a).bind(h),
    };
    Ok(q.execute(conn).await?.rows_affected())
}

enum Binds<'a> {
    Hash(&'a [u8]),
    Id(Uuid),
    IdId(Uuid, Uuid),
    IdHash(Uuid, Option<&'a [u8]>),
}

/// Ends the session a token opens. Revoking an unknown token is not an error:
/// the outcome the caller wants — that token does not work — holds.
pub async fn revoke_token(conn: &mut PgConnection, token: &str, reason: &str) -> Result<u64> {
    let h = hash_token(token);
    revoke_where(conn, "token_sha256 = $2", reason, Binds::Hash(&h)).await
}

/// Ends every session of an account — opened with its credentials or acting
/// as it — except the one `keep_token` opens, when given.
pub async fn revoke_account(
    conn: &mut PgConnection,
    account_id: Uuid,
    keep_token: Option<&str>,
    reason: &str,
) -> Result<u64> {
    let keep = keep_token.map(hash_token);
    revoke_where(
        conn,
        "(account_id = $2 OR login_account_id = $2) AND ($3::bytea IS NULL OR token_sha256 <> $3)",
        reason,
        Binds::IdHash(account_id, keep.as_deref()),
    )
    .await
}

/// Ends every session of an identity.
pub async fn revoke_pial(conn: &mut PgConnection, pial_id: Uuid, reason: &str) -> Result<u64> {
    revoke_where(conn, "pial_id = $2", reason, Binds::Id(pial_id)).await
}

/// Ends one session by id, only when it belongs to `account_id`.
pub async fn revoke_one(conn: &mut PgConnection, session_id: Uuid, account_id: Uuid, reason: &str) -> Result<u64> {
    revoke_where(
        conn,
        "id = $2 AND (account_id = $3 OR login_account_id = $3)",
        reason,
        Binds::IdId(session_id, account_id),
    )
    .await
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct SessionInfo {
    pub id: Uuid,
    pub account_id: Uuid,
    pub device_name: String,
    pub ip: String,
    pub created_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
}

/// The live sessions of an account, most recently used first.
pub async fn list(conn: &mut PgConnection, account_id: Uuid) -> Result<Vec<SessionInfo>> {
    Ok(sqlx::query_as::<_, SessionInfo>(
        "SELECT id, account_id, device_name, ip, created_at, last_seen_at, expires_at
           FROM sessions
          WHERE (account_id = $1 OR login_account_id = $1)
            AND revoked_at IS NULL AND expires_at > NOW()
          ORDER BY last_seen_at DESC",
    )
    .bind(account_id)
    .fetch_all(conn)
    .await?)
}

/// Points a live session at another account of the same identity
/// (SwitchActiveAccount). Returns false when the session is not live or the
/// account is not an active account of the session's identity.
pub async fn switch(conn: &mut PgConnection, token: &str, account_id: Uuid) -> Result<bool> {
    let n = sqlx::query(
        "UPDATE sessions s SET account_id = $2
           FROM accounts a
          WHERE s.token_sha256 = $1 AND s.revoked_at IS NULL AND s.expires_at > NOW()
            AND a.id = $2 AND a.pial_id = s.pial_id AND a.deactivated_at IS NULL AND a.purged_at IS NULL",
    )
    .bind(hash_token(token))
    .bind(account_id)
    .execute(conn)
    .await?
    .rows_affected();
    Ok(n == 1)
}
