//! Passwords, the TOTP second factor and backup codes.
//!
//! Hashes are bcrypt and are computed and checked by pgcrypto, which reads the
//! $2a$ hashes Go's bcrypt wrote and writes ones Go's bcrypt can read:
//!   check:  password_hash = crypt($1, password_hash)
//!   create: crypt($1, gen_salt('bf', 10))       -- Go's bcrypt.DefaultCost
//! (ported from feed-engine/internal/db/auth.go).

use anyhow::Result;
use rand::rngs::OsRng;
use rand::RngCore;
use serde::Serialize;
use sqlx::PgConnection;
use uuid::Uuid;

use super::totp;

const BCRYPT_COST: i32 = 10;
/// GenerateBackupCode count (feed-engine backupCodeCount).
pub const BACKUP_CODE_COUNT: usize = 8;


pub async fn set_password(conn: &mut PgConnection, account_id: Uuid, password: &str) -> Result<()> {
    sqlx::query(
        "INSERT INTO credentials (account_id, password_hash, updated_at)
         VALUES ($1, crypt($2, gen_salt('bf', $3)), NOW())
         ON CONFLICT (account_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, updated_at = NOW()",
    )
    .bind(account_id)
    .bind(password)
    .bind(BCRYPT_COST)
    .execute(conn)
    .await?;
    Ok(())
}

pub async fn has_password(conn: &mut PgConnection, account_id: Uuid) -> Result<bool> {
    Ok(sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM credentials WHERE account_id = $1)")
        .bind(account_id)
        .fetch_one(conn)
        .await?)
}

/// Some(true/false) when the account has a password; None when it has none.
pub async fn check_password(conn: &mut PgConnection, account_id: Uuid, password: &str) -> Result<Option<bool>> {
    Ok(sqlx::query_scalar(
        "SELECT password_hash = crypt($2, password_hash) FROM credentials WHERE account_id = $1",
    )
    .bind(account_id)
    .bind(password)
    .fetch_optional(conn)
    .await?)
}

/// Spends one bcrypt at the cost a real check spends, for a login whose
/// handle does not exist or has no password, so the time a login takes does
/// not say which handles exist.
pub async fn decoy_check(conn: &mut PgConnection, password: &str) -> Result<()> {
    let _hash: String = sqlx::query_scalar("SELECT crypt($1, gen_salt('bf', $2))")
        .bind(password)
        .bind(BCRYPT_COST)
        .fetch_one(conn)
        .await?;
    Ok(())
}

// ── TOTP ──────────────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct TotpStatus {
    pub enrolled: bool,
    pub enabled: bool,
}

pub async fn totp_status(conn: &mut PgConnection, account_id: Uuid) -> Result<TotpStatus> {
    let enabled: Option<bool> = sqlx::query_scalar("SELECT enabled FROM account_totp WHERE account_id = $1")
        .bind(account_id)
        .fetch_optional(conn)
        .await?;
    Ok(TotpStatus {
        enrolled: enabled.is_some(),
        enabled: enabled.unwrap_or(false),
    })
}

/// Mints a fresh unconfirmed secret, replacing any unconfirmed one. None when
/// TOTP is already enabled.
pub async fn totp_enroll(conn: &mut PgConnection, account_id: Uuid) -> Result<Option<String>> {
    let secret = totp::generate_secret();
    let stored: Option<String> = sqlx::query_scalar(
        "INSERT INTO account_totp (account_id, secret, enabled, last_used_step, updated_at)
         VALUES ($1, $2, FALSE, 0, NOW())
         ON CONFLICT (account_id) DO UPDATE
            SET secret = EXCLUDED.secret, last_used_step = 0, updated_at = NOW()
          WHERE account_totp.enabled = FALSE
         RETURNING secret",
    )
    .bind(account_id)
    .bind(&secret)
    .fetch_optional(conn)
    .await?;
    Ok(stored)
}

pub enum TotpOutcome {
    NotEnrolled,
    WrongState,
    Invalid,
    Accepted,
}

/// Checks a code against the account's secret and, when it is right, records
/// its step so it cannot be used again. `want_enabled` is the state the
/// secret must be in for this check to apply.
async fn totp_accept(conn: &mut PgConnection, account_id: Uuid, code: &str, want_enabled: bool) -> Result<TotpOutcome> {
    let row: Option<(String, bool, i64)> = sqlx::query_as(
        "SELECT secret, enabled, last_used_step FROM account_totp WHERE account_id = $1 FOR UPDATE",
    )
    .bind(account_id)
    .fetch_optional(&mut *conn)
    .await?;
    let Some((secret, enabled, last)) = row else {
        return Ok(TotpOutcome::NotEnrolled);
    };
    if enabled != want_enabled {
        return Ok(TotpOutcome::WrongState);
    }
    let Some(step) = totp::matching_step(conn, &secret, code, last).await? else {
        return Ok(TotpOutcome::Invalid);
    };
    sqlx::query("UPDATE account_totp SET last_used_step = $2, updated_at = NOW() WHERE account_id = $1")
        .bind(account_id)
        .bind(step)
        .execute(conn)
        .await?;
    Ok(TotpOutcome::Accepted)
}

/// The first verified code turns the second factor on.
pub async fn totp_confirm(conn: &mut PgConnection, account_id: Uuid, code: &str) -> Result<TotpOutcome> {
    let outcome = totp_accept(conn, account_id, code, false).await?;
    if let TotpOutcome::Accepted = outcome {
        sqlx::query("UPDATE account_totp SET enabled = TRUE, updated_at = NOW() WHERE account_id = $1")
            .bind(account_id)
            .execute(conn)
            .await?;
    }
    Ok(outcome)
}

/// A current code turns it off, deleting the secret.
pub async fn totp_disable(conn: &mut PgConnection, account_id: Uuid, code: &str) -> Result<TotpOutcome> {
    let outcome = totp_accept(conn, account_id, code, true).await?;
    if let TotpOutcome::Accepted = outcome {
        sqlx::query("DELETE FROM account_totp WHERE account_id = $1")
            .bind(account_id)
            .execute(conn)
            .await?;
    }
    Ok(outcome)
}

/// The login check of an enabled second factor.
pub async fn totp_verify(conn: &mut PgConnection, account_id: Uuid, code: &str) -> Result<bool> {
    Ok(matches!(totp_accept(conn, account_id, code, true).await?, TotpOutcome::Accepted))
}

// ── Backup codes ──────────────────────────────────────────────────────────────

/// XXXX-XXXX-XXXX from 60 random bits (GenerateBackupCode).
pub fn generate_backup_code() -> String {
    let mut b = [0u8; 8];
    OsRng.fill_bytes(&mut b);
    let s = &totp::base32_encode(&b)[..12];
    format!("{}-{}-{}", &s[0..4], &s[4..8], &s[8..12])
}

/// normalizeCode: dashes and spaces removed, upper-cased.
pub fn normalize_backup_code(code: &str) -> String {
    code.chars()
        .filter(|c| *c != '-' && !c.is_whitespace())
        .collect::<String>()
        .to_uppercase()
}

/// Replaces every backup code of the account with a fresh set and returns the
/// plaintext codes. This is the only time they exist outside a hash.
pub async fn regenerate_backup_codes(conn: &mut PgConnection, account_id: Uuid) -> Result<Vec<String>> {
    sqlx::query("DELETE FROM backup_codes WHERE account_id = $1")
        .bind(account_id)
        .execute(&mut *conn)
        .await?;
    let codes: Vec<String> = (0..BACKUP_CODE_COUNT).map(|_| generate_backup_code()).collect();
    let normalized: Vec<String> = codes.iter().map(|c| normalize_backup_code(c)).collect();
    sqlx::query(
        "INSERT INTO backup_codes (account_id, code_hash)
         SELECT $1, crypt(c, gen_salt('bf', $3)) FROM unnest($2::text[]) AS c",
    )
    .bind(account_id)
    .bind(&normalized)
    .bind(BCRYPT_COST)
    .execute(conn)
    .await?;
    Ok(codes)
}

pub async fn unused_backup_codes(conn: &mut PgConnection, account_id: Uuid) -> Result<i64> {
    Ok(sqlx::query_scalar("SELECT COUNT(*) FROM backup_codes WHERE account_id = $1 AND NOT used")
        .bind(account_id)
        .fetch_one(conn)
        .await?)
}

/// VerifyAndConsumeBackupCode: spends the matching unused code, atomically.
pub async fn consume_backup_code(conn: &mut PgConnection, account_id: Uuid, code: &str) -> Result<bool> {
    let normalized = normalize_backup_code(code);
    if normalized.is_empty() {
        return Ok(false);
    }
    let spent: Option<Uuid> = sqlx::query_scalar(
        "UPDATE backup_codes SET used = TRUE, used_at = NOW()
          WHERE id = (SELECT id FROM backup_codes
                       WHERE account_id = $1 AND NOT used AND code_hash = crypt($2, code_hash)
                       LIMIT 1 FOR UPDATE)
         RETURNING id",
    )
    .bind(account_id)
    .bind(&normalized)
    .fetch_optional(conn)
    .await?;
    Ok(spent.is_some())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backup_codes_have_the_printed_shape() {
        let c = generate_backup_code();
        assert_eq!(c.len(), 14);
        assert_eq!(normalize_backup_code(&c).len(), 12);
        assert_eq!(normalize_backup_code(" abcd-efgh ijkl"), "ABCDEFGHIJKL");
    }
}
