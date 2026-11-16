//! Accounts: creating one (and the founder rule), adding and linking personas,
//! deactivation, signing in and changing a password.

use chrono::{NaiveDate, Utc};
use serde_json::{json, Value};
use sqlx::{PgConnection, PgPool};
use uuid::Uuid;

use super::credentials::{self, TotpOutcome};
use super::tokens::{claims_for, TokenIssuer};
use super::{events, ledger, pial, sessions, view, vocab};
use crate::api::{ApiError, ApiResult};
use crate::handles;

fn password_refusal(p: vocab::PasswordProblem) -> ApiError {
    match p {
        vocab::PasswordProblem::TooShort => ApiError::bad_request(
            "weak_password",
            format!("Password must be at least {} characters.", vocab::PASSWORD_MIN_CHARS),
        ),
        vocab::PasswordProblem::TooLong => ApiError::bad_request(
            "weak_password",
            format!("Password must be at most {} bytes.", vocab::PASSWORD_MAX_BYTES),
        ),
    }
}

/// A handle as a new claim must have it: valid, normalised.
pub fn claimable_handle(raw: &str) -> ApiResult<String> {
    let handle = vocab::normalize_handle(raw);
    if !vocab::handle_is_valid(&handle) {
        return Err(ApiError::bad_request(
            "invalid_handle",
            "Handle must be 1–30 characters: lowercase letters, numbers and underscore.",
        ));
    }
    Ok(handle)
}

// ── The founder ───────────────────────────────────────────────────────────────

/// Whether the account about to be inserted in this transaction is the first
/// account ever (feed-engine founder.go isFirstAccount, ported). Requires READ
/// COMMITTED, so the recheck after the lock sees what a concurrent signup
/// committed while this one waited.
///
/// Common case: an existing account is found and KEY SHARE-locked, so it
/// cannot vanish before this transaction commits. Only on an empty table is
/// the table locked against every writer and looked at again; concurrent
/// first signups queue on that lock and every one after the first sees the
/// account the first committed. A founder that exists with no account left is
/// still the founder: the next account is ordinary, and the unique index on
/// pial_states would refuse a second grant anyway.
async fn is_first_account(conn: &mut PgConnection) -> anyhow::Result<bool> {
    let any: Option<Uuid> = sqlx::query_scalar("SELECT id FROM accounts LIMIT 1 FOR KEY SHARE")
        .fetch_optional(&mut *conn)
        .await?;
    if any.is_some() {
        return Ok(false);
    }
    sqlx::query("LOCK TABLE accounts IN SHARE ROW EXCLUSIVE MODE")
        .execute(&mut *conn)
        .await?;
    let (account_exists, founder_exists): (bool, bool) = sqlx::query_as(
        "SELECT EXISTS (SELECT 1 FROM accounts),
                EXISTS (SELECT 1 FROM pial_states WHERE role = 'founder')",
    )
    .fetch_one(&mut *conn)
    .await?;
    Ok(!account_exists && !founder_exists)
}

async fn genesis_open(conn: &mut PgConnection) -> anyhow::Result<bool> {
    Ok(sqlx::query_scalar::<_, bool>("SELECT EXISTS (SELECT 1 FROM identity_genesis)")
        .fetch_one(conn)
        .await?)
}

async fn begin_read_committed(pool: &PgPool) -> anyhow::Result<sqlx::Transaction<'static, sqlx::Postgres>> {
    let mut tx = pool.begin().await?;
    sqlx::query("SET TRANSACTION ISOLATION LEVEL READ COMMITTED")
        .execute(&mut *tx)
        .await?;
    Ok(tx)
}

// ── Signup ────────────────────────────────────────────────────────────────────

#[derive(Debug, serde::Deserialize)]
pub struct SignupRequest {
    pub handle: String,
    pub password: String,
    #[serde(default)]
    pub display_name: Option<String>,
    #[serde(default)]
    pub dob: String,
    #[serde(default)]
    pub role_type: String,
    #[serde(default)]
    pub creator_type: String,
    #[serde(default)]
    pub content_setting: String,
    #[serde(default)]
    pub device_name: String,
    #[serde(default)]
    pub ip: String,
}

/// Creates an account on a new identity, signed in. The identity, its facts,
/// its capabilities, its handle, the account, its password, the founder
/// decision, the session, the ledger and the identity.events row commit
/// together or not at all.
pub async fn signup(pool: &PgPool, issuer: &TokenIssuer, req: SignupRequest) -> ApiResult<Value> {
    let handle = claimable_handle(&req.handle)?;
    vocab::check_password(&req.password).map_err(password_refusal)?;
    let dob_raw = req.dob.trim();
    if dob_raw.is_empty() {
        return Err(ApiError::bad_request("invalid_date_of_birth", "Date of birth is required."));
    }
    let dob = NaiveDate::parse_from_str(dob_raw, "%Y-%m-%d")
        .map_err(|_| ApiError::bad_request("invalid_date_of_birth", "Date of birth must be YYYY-MM-DD."))?;
    if dob > Utc::now().date_naive() {
        return Err(ApiError::bad_request("invalid_date_of_birth", "A date of birth cannot be in the future."));
    }
    let age = vocab::age_today(dob);
    if age < vocab::MINIMUM_AGE {
        return Err(ApiError::forbidden(
            "too_young",
            format!("You must be at least {} years old to join F33D3R.", vocab::MINIMUM_AGE),
        ));
    }
    let is_minor = age < 18;
    let role_type = vocab::normalize_role_type(&req.role_type);
    let content_setting = vocab::normalize_content_setting(&req.content_setting, is_minor);
    let creator_type = if role_type == "creator" { req.creator_type.trim() } else { "" };
    let display_name = req
        .display_name
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(|s| s.chars().take(100).collect::<String>())
        .unwrap_or_else(|| handle.clone());

    let mut tx = begin_read_committed(pool).await?;
    if !genesis_open(&mut tx).await? {
        return Err(ApiError::new(
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "identity_plane_sealed",
            "Account creation opens once the identity import has run.",
        ));
    }
    handles::ensure_claimable(&mut tx, &handle).await?;

    let founder = is_first_account(&mut tx).await?;
    let role = if founder { vocab::ROLE_FOUNDER } else { vocab::ROLE_USER };
    let pial_id = pial::mint(&mut tx, dob, role).await?;
    handles::insert_claim(&mut tx, &handle, pial_id, "standard", json!({ "source": "signup" })).await?;

    let account_id: Uuid = sqlx::query_scalar(
        "INSERT INTO accounts (pial_id, handle, is_primary) VALUES ($1, $2::citext, TRUE) RETURNING id",
    )
    .bind(pial_id)
    .bind(&handle)
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| handles::map_unique(e, "handle_taken"))?;
    credentials::set_password(&mut tx, account_id, &req.password).await?;

    let onboarding = json!({
        "handle": handle,
        "display_name": display_name,
        "role_type": role_type,
        "creator_type": creator_type,
        "content_setting": content_setting,
    });
    ledger::append(&mut tx, pial_id, Some(account_id), "account_created", onboarding.clone(), "elohim-veni").await?;
    if founder {
        ledger::append(
            &mut tx,
            pial_id,
            Some(account_id),
            "founder_granted",
            json!({ "handle": handle, "reason": "first account created on this platform" }),
            "elohim-veni",
        )
        .await?;
        tracing::info!(handle = %handle, "the first account holds the founder role");
    }
    let session = sessions::create(&mut tx, account_id, pial_id, &req.device_name, &req.ip).await?;
    events::emit_account(&mut tx, account_id, "account.created", json!({ "onboarding": onboarding, "founder": founder })).await?;

    let (account, view) = view::load_account_view(&mut tx, account_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("account {account_id} vanished inside its own transaction"))?;
    let access_token = issuer.issue(&claims_for(&view, &account, session.id))?;
    tx.commit().await?;

    Ok(json!({
        "account_id": account_id,
        "pial_id": pial_id,
        "handle": handle,
        "founder": founder,
        "session_token": session.token,
        "access_token": access_token,
        // Superset: what the caller needs to finish its own side of signup.
        "session_id": session.id,
        "session_expires_at": session.expires_at,
        "role": view.role,
        "caps": view.granted(Utc::now()),
        "facts": view.facts,
        "profile": {
            "display_name": display_name,
            "role_type": role_type,
            "creator_type": creator_type,
            "content_setting": content_setting,
        },
    }))
}

// ── Personas ──────────────────────────────────────────────────────────────────

#[derive(Debug, serde::Deserialize)]
pub struct PersonaRequest {
    pub handle: String,
    pub password: String,
    #[serde(default)]
    pub display_name: Option<String>,
    #[serde(default)]
    pub device_name: String,
    #[serde(default)]
    pub ip: String,
}

/// Adds an account to an existing identity (BindAdditionalAccount). It shares
/// the identity's facts; it has its own handle and password, and is not
/// primary. No session is opened: the caller switches to it.
pub async fn add_persona(pool: &PgPool, pial_id: Uuid, req: PersonaRequest) -> ApiResult<Value> {
    let handle = claimable_handle(&req.handle)?;
    vocab::check_password(&req.password).map_err(password_refusal)?;
    let mut tx = pool.begin().await?;
    let (status, _) = pial::lock(&mut tx, pial_id).await?;
    if vocab::is_gone(&status) || vocab::is_barred(&status) {
        return Err(ApiError::forbidden("suspended", "This identity cannot add accounts."));
    }
    handles::ensure_claimable(&mut tx, &handle).await?;
    handles::insert_claim(&mut tx, &handle, pial_id, "standard", json!({ "source": "persona" })).await?;
    let account_id: Uuid = sqlx::query_scalar(
        "INSERT INTO accounts (pial_id, handle, is_primary) VALUES ($1, $2::citext, FALSE) RETURNING id",
    )
    .bind(pial_id)
    .bind(&handle)
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| handles::map_unique(e, "handle_taken"))?;
    credentials::set_password(&mut tx, account_id, &req.password).await?;
    let display_name = req
        .display_name
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(|s| s.chars().take(100).collect::<String>())
        .unwrap_or_else(|| handle.clone());
    let detail = json!({
        "persona": true, "handle": handle, "display_name": display_name,
        "device_name": req.device_name, "ip": req.ip,
    });
    ledger::append(&mut tx, pial_id, Some(account_id), "account_added", detail.clone(), "elohim-veni").await?;
    events::emit_account(&mut tx, account_id, "account.created", json!({ "onboarding": detail })).await?;
    tx.commit().await?;
    Ok(json!({ "account_id": account_id, "pial_id": pial_id, "handle": handle, "founder": false }))
}

#[derive(Debug, serde::Deserialize)]
pub struct LinkRequest {
    /// The account to link, by handle (what a person types) or by id.
    #[serde(default)]
    pub handle: Option<String>,
    #[serde(default)]
    pub account_id: Option<Uuid>,
    pub backup_code: String,
}

/// Moves an independently created account onto this identity
/// (LinkExistingAccount). Proof of control is one of the target account's
/// backup codes, which is spent. The target must be alone on its identity.
/// Its handle moves with it, its sessions end (they name the old identity),
/// and it joins as a non-primary account.
pub async fn link(pool: &PgPool, pial_id: Uuid, req: LinkRequest) -> ApiResult<Value> {
    let mut tx = pool.begin().await?;
    let (status, _) = pial::lock(&mut tx, pial_id).await?;
    if vocab::is_gone(&status) || vocab::is_barred(&status) {
        return Err(ApiError::forbidden("suspended", "This identity cannot link accounts."));
    }
    let target = match (req.handle.as_deref(), req.account_id) {
        (Some(h), _) => view::load_account_by_handle(&mut tx, &vocab::normalize_handle(h)).await?,
        (None, Some(id)) => view::load_account(&mut tx, id).await?,
        (None, None) => return Err(ApiError::bad_request("bad_request", "Send handle or account_id.")),
    }
    .filter(|a| a.purged_at.is_none())
    .ok_or_else(|| ApiError::not_found("not_found", "No such account."))?;
    if target.pial_id == pial_id {
        return Err(ApiError::conflict("already_linked", "That account is already on this identity."));
    }
    pial::lock(&mut tx, target.pial_id).await?;
    let others: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM accounts WHERE pial_id = $1 AND id <> $2")
        .bind(target.pial_id)
        .bind(target.account_id)
        .fetch_one(&mut *tx)
        .await?;
    if others > 0 {
        return Err(ApiError::conflict("account_not_solo", "That account already has linked accounts."));
    }
    if !credentials::consume_backup_code(&mut tx, target.account_id, &req.backup_code).await? {
        return Err(ApiError::unauthorized("backup_code_invalid", "Invalid backup code."));
    }
    let from = target.pial_id;
    sqlx::query("UPDATE accounts SET pial_id = $1, is_primary = FALSE, bound_at = NOW() WHERE id = $2")
        .bind(pial_id)
        .bind(target.account_id)
        .execute(&mut *tx)
        .await?;
    sqlx::query("UPDATE handles SET pial_id = $1, last_bound_at = NOW(), updated_at = NOW() WHERE handle = $2::citext")
        .bind(pial_id)
        .bind(&target.handle)
        .execute(&mut *tx)
        .await?;
    handles::log_event(&mut tx, &target.handle, "transfer", Some(from), Some(pial_id),
        json!({ "reason": "account linked to another identity", "account_id": target.account_id })).await?;
    sessions::revoke_account(&mut tx, target.account_id, None, "account linked to another identity").await?;
    let detail = json!({ "account_id": target.account_id, "from_pial": from, "to_pial": pial_id });
    ledger::append(&mut tx, pial_id, Some(target.account_id), "account_linked", detail.clone(), "elohim-veni").await?;
    ledger::append(&mut tx, from, Some(target.account_id), "account_unlinked", detail.clone(), "elohim-veni").await?;
    events::emit_account(&mut tx, target.account_id, "account.linked", detail.clone()).await?;
    events::emit_pial(&mut tx, from, "pial.account_unlinked", detail).await?;
    tx.commit().await?;
    Ok(json!({ "account_id": target.account_id, "pial_id": pial_id, "handle": target.handle }))
}

/// A live (not purged) account, locked for the rest of the transaction.
async fn live_account(conn: &mut PgConnection, account_id: Uuid) -> ApiResult<view::AccountRow> {
    let account = view::load_account(conn, account_id)
        .await?
        .filter(|a| a.purged_at.is_none())
        .ok_or_else(|| ApiError::not_found("not_found", "No such account."))?;
    sqlx::query("SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE")
        .bind(account_id)
        .execute(&mut *conn)
        .await?;
    Ok(account)
}

/// DeactivateUser: the account steps away and every session it has ends.
/// Signing in again reactivates it.
pub async fn deactivate(pool: &PgPool, account_id: Uuid, reason: &str) -> ApiResult<()> {
    let mut tx = pool.begin().await?;
    let account = live_account(&mut tx, account_id).await?;
    sqlx::query("UPDATE accounts SET deactivated_at = NOW(), deactivated_reason = $2 WHERE id = $1")
        .bind(account_id)
        .bind(reason)
        .execute(&mut *tx)
        .await?;
    sessions::revoke_account(&mut tx, account_id, None, "account deactivated").await?;
    ledger::append(&mut tx, account.pial_id, Some(account_id), "account_deactivated", json!({ "reason": reason }), "elohim-veni").await?;
    events::emit_account(&mut tx, account_id, "account.deactivated", json!({ "reason": reason })).await?;
    tx.commit().await?;
    Ok(())
}

/// Purges an account for good (feed-engine's hard delete): credentials,
/// second factor, backup codes and recovery tokens are deleted, every session
/// ends, and it can never sign in or be reactivated. The row stays, because
/// content and the ledger name it. When it was the identity's last live
/// account, the identity is purged with it (facts erased, PIAL left opaque).
/// Purging a purged account is a no-op.
pub async fn purge(pool: &PgPool, account_id: Uuid, by: &str) -> ApiResult<()> {
    let mut tx = pool.begin().await?;
    let Some(account) = view::load_account(&mut tx, account_id).await? else {
        return Err(ApiError::not_found("not_found", "No such account."));
    };
    if account.purged_at.is_some() {
        return Ok(());
    }
    pial::lock(&mut tx, account.pial_id).await?;
    sqlx::query(
        "UPDATE accounts SET purged_at = NOW(), deactivated_at = COALESCE(deactivated_at, NOW()),
                             deactivated_reason = 'purged'
          WHERE id = $1",
    )
    .bind(account_id)
    .execute(&mut *tx)
    .await?;
    for table in ["credentials", "account_totp", "backup_codes", "recovery_tokens"] {
        sqlx::query(&format!("DELETE FROM {table} WHERE account_id = $1"))
            .bind(account_id)
            .execute(&mut *tx)
            .await?;
    }
    sessions::revoke_account(&mut tx, account_id, None, "account purged").await?;
    ledger::append(&mut tx, account.pial_id, Some(account_id), "account_purged", json!({ "by": by }), by).await?;
    let others: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM accounts WHERE pial_id = $1 AND purged_at IS NULL")
        .bind(account.pial_id)
        .fetch_one(&mut *tx)
        .await?;
    if others == 0 {
        // Emits the identity's own snapshot, this account included.
        pial::tombstone(&mut tx, account.pial_id, true, "last account purged", by).await?;
    } else {
        events::emit_account(&mut tx, account_id, "account.purged", json!({ "by": by })).await?;
    }
    tx.commit().await?;
    Ok(())
}

// ── Sign in ───────────────────────────────────────────────────────────────────

#[derive(Debug, serde::Deserialize)]
pub struct LoginRequest {
    pub handle: String,
    pub password: String,
    #[serde(default)]
    pub totp: Option<String>,
    #[serde(default)]
    pub backup_code: Option<String>,
    #[serde(default)]
    pub device_name: String,
    #[serde(default)]
    pub ip: String,
}

fn invalid_credentials() -> ApiError {
    ApiError::unauthorized("invalid_credentials", "Invalid handle or password.")
}

fn suspended() -> ApiError {
    ApiError::forbidden(
        "suspended",
        "This account has been suspended. Contact support if you believe this is an error.",
    )
}

/// Signs in (feed-engine apiV1Login, ported). One answer for an unknown handle
/// and a wrong password, at one cost. The second factor is asked for only after
/// the password is right, so a stranger cannot learn which handles have it on.
pub async fn login(pool: &PgPool, issuer: &TokenIssuer, req: LoginRequest) -> ApiResult<Value> {
    let handle = vocab::normalize_handle(&req.handle);
    let mut tx = pool.begin().await?;
    if handle.is_empty() || req.password.is_empty() {
        return Err(invalid_credentials());
    }
    let Some(account) = view::load_account_by_handle(&mut tx, &handle)
        .await?
        .filter(|a| a.purged_at.is_none())
    else {
        credentials::decoy_check(&mut tx, &req.password).await?;
        return Err(invalid_credentials());
    };
    let (status, _) = pial::lock(&mut tx, account.pial_id).await?;
    if vocab::is_gone(&status) {
        credentials::decoy_check(&mut tx, &req.password).await?;
        return Err(invalid_credentials());
    }
    match credentials::check_password(&mut tx, account.account_id, &req.password).await? {
        None => {
            // An account with no password is not claimed by whoever types one
            // first: that rule let anyone who knew a handle take the account.
            credentials::decoy_check(&mut tx, &req.password).await?;
            return Err(ApiError::forbidden(
                "password_not_set",
                "This account has no password. Recover it with a backup code.",
            ));
        }
        Some(false) => return Err(invalid_credentials()),
        Some(true) => {}
    }

    let second_factor = credentials::totp_status(&mut tx, account.account_id).await?;
    if second_factor.enabled {
        let totp = req.totp.as_deref().map(str::trim).filter(|s| !s.is_empty());
        let backup = req.backup_code.as_deref().map(str::trim).filter(|s| !s.is_empty());
        if totp.is_none() && backup.is_none() {
            return Err(ApiError::conflict("totp_required", "Enter the code from your authenticator app."));
        }
        let mut passed = false;
        if let Some(code) = totp {
            passed = credentials::totp_verify(&mut tx, account.account_id, code).await?
                // A backup code typed into the code field is accepted, as before.
                || credentials::consume_backup_code(&mut tx, account.account_id, code).await?;
        }
        if !passed {
            if let Some(code) = backup {
                passed = credentials::consume_backup_code(&mut tx, account.account_id, code).await?;
            }
        }
        if !passed {
            return Err(ApiError::unauthorized("totp_invalid", "That code is not right. Try again."));
        }
    }

    let body = open_session(&mut tx, issuer, &account, &status, &req.device_name, &req.ip, "sign-in").await?;
    tx.commit().await?;
    Ok(body)
}

/// The end of every way in: refuse a barred or stopped identity, reactivate a
/// deactivated account, open the session and sign the assertion. A refusal
/// rolls back the caller's transaction, so a code spent on it stays unspent.
async fn open_session(
    tx: &mut PgConnection,
    issuer: &TokenIssuer,
    account: &view::AccountRow,
    status: &str,
    device_name: &str,
    ip: &str,
    via: &str,
) -> ApiResult<Value> {
    let pial_view = view::load_pial(tx, account.pial_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("account {} points at missing PIAL", account.account_id))?;
    if vocab::is_barred(status) || pial_view.is_stopped() {
        return Err(suspended());
    }
    if account.deactivated_at.is_some() {
        sqlx::query("UPDATE accounts SET deactivated_at = NULL, deactivated_reason = '' WHERE id = $1")
            .bind(account.account_id)
            .execute(&mut *tx)
            .await?;
        ledger::append(tx, account.pial_id, Some(account.account_id), "account_reactivated",
            json!({ "by": via }), "elohim-veni").await?;
        events::emit_account(tx, account.account_id, "account.reactivated", json!({ "by": via })).await?;
    }
    let session = sessions::create(tx, account.account_id, account.pial_id, device_name, ip).await?;
    let needs_backup_codes = credentials::unused_backup_codes(tx, account.account_id).await? == 0;
    let (account, pial_view) = view::load_account_view(tx, account.account_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("account vanished while opening a session"))?;
    let access_token = issuer.issue(&claims_for(&pial_view, &account, session.id))?;
    Ok(json!({
        "account_id": account.account_id,
        "pial_id": account.pial_id,
        "session_token": session.token,
        "access_token": access_token,
        "needs_backup_codes": needs_backup_codes,
        // Superset.
        "handle": account.handle,
        "session_id": session.id,
        "session_expires_at": session.expires_at,
    }))
}

// ── Recovery ──────────────────────────────────────────────────────────────────

#[derive(Debug, serde::Deserialize)]
pub struct RecoveryVerify {
    pub handle: String,
    pub backup_code: String,
}

/// Spends a backup code for a recovery token: the way back in for a person who
/// lost their password. Unknown handle and wrong code are one answer.
pub async fn recovery_verify(pool: &PgPool, req: RecoveryVerify) -> ApiResult<Value> {
    let refused = || ApiError::unauthorized("backup_code_invalid", "Invalid handle or backup code.");
    let mut tx = pool.begin().await?;
    let Some(account) = view::load_account_by_handle(&mut tx, &vocab::normalize_handle(&req.handle))
        .await?
        .filter(|a| a.purged_at.is_none())
    else {
        return Err(refused());
    };
    let (status, _) = pial::lock(&mut tx, account.pial_id).await?;
    if vocab::is_gone(&status) {
        return Err(refused());
    }
    if !credentials::consume_backup_code(&mut tx, account.account_id, &req.backup_code).await? {
        return Err(refused());
    }
    let token = sessions::new_secret();
    let expires_at: chrono::DateTime<Utc> = sqlx::query_scalar(
        "INSERT INTO recovery_tokens (token_sha256, account_id) VALUES ($1, $2) RETURNING expires_at",
    )
    .bind(sessions::hash_token(&token))
    .bind(account.account_id)
    .fetch_one(&mut *tx)
    .await?;
    ledger::append(&mut tx, account.pial_id, Some(account.account_id), "recovery_started", json!({}), "elohim-veni").await?;
    tx.commit().await?;
    Ok(json!({ "recovery_token": token, "account_id": account.account_id, "expires_at": expires_at }))
}

#[derive(Debug, serde::Deserialize)]
pub struct RecoveryComplete {
    pub recovery_token: String,
    pub new_password: String,
    #[serde(default)]
    pub device_name: String,
    #[serde(default)]
    pub ip: String,
}

/// Spends the recovery token: sets the new password, ends every session the
/// account had, and signs in — the sign-in response, exactly.
pub async fn recovery_complete(pool: &PgPool, issuer: &TokenIssuer, req: RecoveryComplete) -> ApiResult<Value> {
    vocab::check_password(&req.new_password).map_err(password_refusal)?;
    let mut tx = pool.begin().await?;
    let account_id: Uuid = sqlx::query_scalar(
        "UPDATE recovery_tokens SET used_at = NOW()
          WHERE token_sha256 = $1 AND used_at IS NULL AND expires_at > NOW()
         RETURNING account_id",
    )
    .bind(sessions::hash_token(req.recovery_token.trim()))
    .fetch_optional(&mut *tx)
    .await?
    .ok_or_else(|| ApiError::unauthorized("recovery_expired", "This recovery link has expired or was used."))?;
    let account = live_account(&mut tx, account_id).await?;
    let (status, _) = pial::lock(&mut tx, account.pial_id).await?;
    if vocab::is_gone(&status) {
        return Err(ApiError::unauthorized("recovery_expired", "This recovery link has expired or was used."));
    }
    credentials::set_password(&mut tx, account_id, &req.new_password).await?;
    let ended = sessions::revoke_account(&mut tx, account_id, None, "password recovered").await?;
    ledger::append(&mut tx, account.pial_id, Some(account_id), "password_recovered",
        json!({ "sessions_ended": ended }), "elohim-veni").await?;
    let body = open_session(&mut tx, issuer, &account, &status, &req.device_name, &req.ip, "recovery").await?;
    tx.commit().await?;
    Ok(body)
}

// ── Password ──────────────────────────────────────────────────────────────────

#[derive(Debug, serde::Deserialize)]
pub struct PasswordChange {
    #[serde(default)]
    pub current: String,
    pub new: String,
    #[serde(default)]
    pub keep_session_token: Option<String>,
}

/// Changes the password and ends every other session of the account. An
/// account that has a password must present it; one that has none sets its
/// first through a session it already holds.
pub async fn change_password(pool: &PgPool, account_id: Uuid, req: PasswordChange) -> ApiResult<()> {
    vocab::check_password(&req.new).map_err(password_refusal)?;
    let mut tx = pool.begin().await?;
    let account = live_account(&mut tx, account_id).await?;
    if let Some(ok) = credentials::check_password(&mut tx, account_id, &req.current).await? {
        if !ok {
            return Err(ApiError::forbidden("wrong_password", "Current password is incorrect."));
        }
    }
    credentials::set_password(&mut tx, account_id, &req.new).await?;
    let ended = sessions::revoke_account(&mut tx, account_id, req.keep_session_token.as_deref(), "password changed").await?;
    ledger::append(&mut tx, account.pial_id, Some(account_id), "password_changed",
        json!({ "sessions_ended": ended }), "elohim-veni").await?;
    tx.commit().await?;
    Ok(())
}

#[derive(Debug, serde::Deserialize)]
pub struct PasswordReset {
    pub new: String,
    #[serde(default)]
    pub by: String,
}

/// An operator sets an account's password; every session it has ends.
pub async fn reset_password(pool: &PgPool, account_id: Uuid, req: PasswordReset) -> ApiResult<()> {
    vocab::check_password(&req.new).map_err(password_refusal)?;
    let by = if req.by.trim().is_empty() { "unspecified" } else { req.by.trim() };
    let mut tx = pool.begin().await?;
    let account = live_account(&mut tx, account_id).await?;
    credentials::set_password(&mut tx, account_id, &req.new).await?;
    let ended = sessions::revoke_account(&mut tx, account_id, None, "password reset").await?;
    ledger::append(&mut tx, account.pial_id, Some(account_id), "password_reset",
        json!({ "by": by, "sessions_ended": ended }), by).await?;
    tx.commit().await?;
    Ok(())
}

// ── Reads ─────────────────────────────────────────────────────────────────────

/// An account with its identity folded on and its credential standing.
pub async fn detail(conn: &mut PgConnection, account: view::AccountRow) -> ApiResult<Value> {
    let pial_view = view::load_pial(conn, account.pial_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("account {} points at missing PIAL", account.account_id))?;
    let has_password = credentials::has_password(conn, account.account_id).await?;
    let totp = credentials::totp_status(conn, account.account_id).await?;
    let unused = credentials::unused_backup_codes(conn, account.account_id).await?;
    let mut out = view::account_json(&account);
    out["role"] = json!(pial_view.role);
    out["status"] = json!(vocab::account_status(
        &pial_view.status,
        account.deactivated_at.is_some(),
        account.purged_at.is_some(),
    ));
    out["pial_status"] = json!(pial_view.status);
    out["tier"] = json!(pial_view.tier);
    out["caps"] = json!(pial_view.granted(Utc::now()));
    out["cap_states"] = pial_view.cap_states_json();
    out["facts"] = json!(pial_view.facts);
    out["trust"] = json!(pial_view.trust);
    out["has_password"] = json!(has_password);
    out["totp_enabled"] = json!(totp.enabled);
    out["backup_codes_remaining"] = json!(unused);
    Ok(out)
}

/// The founder's account: the earliest account on the one identity whose role
/// is founder (GetFounder). None when there is none.
pub async fn founder(conn: &mut PgConnection) -> anyhow::Result<Option<view::AccountRow>> {
    Ok(sqlx::query_as::<_, view::AccountRow>(&format!(
        "{} WHERE pial_id = (SELECT pial_id FROM pial_states WHERE role = 'founder') ORDER BY created_at LIMIT 1",
        view::ACCOUNT_SELECT
    ))
    .fetch_optional(conn)
    .await?)
}

/// Whether an outcome of a TOTP check is one the HTTP layer refuses, and how.
pub fn totp_refusal(outcome: TotpOutcome, enabling: bool) -> Option<ApiError> {
    match outcome {
        TotpOutcome::Accepted => None,
        TotpOutcome::Invalid => Some(ApiError::unauthorized("totp_invalid", "Invalid code.")),
        TotpOutcome::NotEnrolled if enabling => Some(ApiError::bad_request("totp_not_enrolled", "Set up 2FA first.")),
        TotpOutcome::NotEnrolled => Some(ApiError::bad_request("totp_not_enabled", "2FA is not enabled.")),
        TotpOutcome::WrongState if enabling => Some(ApiError::conflict("totp_already_enabled", "2FA is already enabled.")),
        TotpOutcome::WrongState => Some(ApiError::bad_request("totp_not_enabled", "2FA is not enabled.")),
    }
}
