//! The identity contract over HTTP. Every route here sits behind the
//! internal-key guard (api.rs) except /v1/auth/jwks.

use axum::{
    extract::{Path, Query, State},
    http::{header, StatusCode},
    response::IntoResponse,
    Json,
};
use chrono::{DateTime, NaiveDate, Utc};
use serde::Deserialize;
use serde_json::{json, Value};
use uuid::Uuid;

use super::accounts::{
    self, LinkRequest, LoginRequest, PasswordChange, PasswordReset, PersonaRequest, RecoveryComplete,
    RecoveryVerify, SignupRequest,
};
use super::pial::{self, DirectFacts, FactsChange, Kyc, TrustChange};
use super::tokens::{claims_for, JWKS_MAX_AGE_SECS};
use super::{credentials, events, ledger, sessions, totp, view, vocab};
use crate::api::{ApiError, ApiResult};
use crate::handlers::AppState;

fn bad_json(what: &str) -> ApiError {
    ApiError::bad_request("bad_request", format!("The request body could not be read: {what}"))
}

// ── Accounts ──────────────────────────────────────────────────────────────────

pub async fn create_account(
    State(state): State<AppState>,
    Json(req): Json<SignupRequest>,
) -> ApiResult<impl IntoResponse> {
    let body = accounts::signup(&state.pool, &state.tokens, req).await?;
    Ok((StatusCode::CREATED, Json(body)))
}

pub async fn get_account(State(state): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    let account = view::load_account(&mut conn, id)
        .await?
        .ok_or_else(|| ApiError::not_found("not_found", "No such account."))?;
    Ok(Json(accounts::detail(&mut conn, account).await?))
}

pub async fn get_account_by_handle(State(state): State<AppState>, Path(handle): Path<String>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    let account = view::load_account_by_handle(&mut conn, &vocab::normalize_handle(&handle))
        .await?
        .ok_or_else(|| ApiError::not_found("not_found", "No such account."))?;
    Ok(Json(accounts::detail(&mut conn, account).await?))
}

pub async fn get_founder(State(state): State<AppState>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    let account = accounts::founder(&mut conn)
        .await?
        .ok_or_else(|| ApiError::not_found("not_found", "No founder account exists."))?;
    Ok(Json(accounts::detail(&mut conn, account).await?))
}

pub async fn change_password(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<PasswordChange>,
) -> ApiResult<StatusCode> {
    accounts::change_password(&state.pool, id, req).await?;
    Ok(StatusCode::NO_CONTENT)
}

#[derive(Debug, Deserialize)]
pub struct ReasonBody {
    #[serde(default)]
    pub reason: String,
    #[serde(default)]
    pub by: String,
}

pub async fn deactivate_account(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<ReasonBody>,
) -> ApiResult<StatusCode> {
    accounts::deactivate(&state.pool, id, &req.reason).await?;
    Ok(StatusCode::NO_CONTENT)
}

pub async fn add_persona(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<PersonaRequest>,
) -> ApiResult<impl IntoResponse> {
    Ok((StatusCode::CREATED, Json(accounts::add_persona(&state.pool, pial_id, req).await?)))
}

pub async fn link_account(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<LinkRequest>,
) -> ApiResult<Json<Value>> {
    Ok(Json(accounts::link(&state.pool, pial_id, req).await?))
}

#[derive(Debug, Deserialize)]
pub struct ByBody {
    #[serde(default)]
    pub by: String,
}

pub async fn purge_account(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<ByBody>,
) -> ApiResult<StatusCode> {
    accounts::purge(&state.pool, id, by_or_unknown(&req.by)).await?;
    Ok(StatusCode::NO_CONTENT)
}

pub async fn reset_password(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<PasswordReset>,
) -> ApiResult<StatusCode> {
    accounts::reset_password(&state.pool, id, req).await?;
    Ok(StatusCode::NO_CONTENT)
}

pub async fn recovery_verify(State(state): State<AppState>, Json(req): Json<RecoveryVerify>) -> ApiResult<Json<Value>> {
    Ok(Json(accounts::recovery_verify(&state.pool, req).await?))
}

pub async fn recovery_complete(State(state): State<AppState>, Json(req): Json<RecoveryComplete>) -> ApiResult<Json<Value>> {
    Ok(Json(accounts::recovery_complete(&state.pool, &state.tokens, req).await?))
}

// ── TOTP and backup codes ─────────────────────────────────────────────────────

async fn account_or_404(conn: &mut sqlx::PgConnection, id: Uuid) -> ApiResult<view::AccountRow> {
    view::load_account(conn, id)
        .await?
        .ok_or_else(|| ApiError::not_found("not_found", "No such account."))
}

pub async fn totp_status(State(state): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    account_or_404(&mut conn, id).await?;
    Ok(Json(json!(credentials::totp_status(&mut conn, id).await?)))
}

pub async fn totp_enroll(State(state): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let mut tx = state.pool.begin().await?;
    let account = account_or_404(&mut tx, id).await?;
    let secret = credentials::totp_enroll(&mut tx, id)
        .await?
        .ok_or_else(|| ApiError::conflict("totp_already_enabled", "2FA is already enabled."))?;
    tx.commit().await?;
    Ok(Json(json!({
        "secret": secret,
        "uri": totp::provisioning_uri(&secret, &account.handle),
    })))
}

#[derive(Debug, Deserialize)]
pub struct CodeBody {
    pub code: String,
}

pub async fn totp_confirm(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<CodeBody>,
) -> ApiResult<StatusCode> {
    if totp::normalize_code(&req.code).is_none() {
        return Err(ApiError::bad_request("totp_invalid", "Enter a 6-digit code."));
    }
    let mut tx = state.pool.begin().await?;
    let account = account_or_404(&mut tx, id).await?;
    let outcome = credentials::totp_confirm(&mut tx, id, &req.code).await?;
    if let Some(refusal) = accounts::totp_refusal(outcome, true) {
        return Err(refusal);
    }
    ledger::append(&mut tx, account.pial_id, Some(id), "totp_enabled", json!({}), "elohim-veni").await?;
    events::emit_account(&mut tx, id, "account.credentials_changed", json!({ "totp_enabled": true })).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

pub async fn totp_disable(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<CodeBody>,
) -> ApiResult<StatusCode> {
    let mut tx = state.pool.begin().await?;
    let account = account_or_404(&mut tx, id).await?;
    let outcome = credentials::totp_disable(&mut tx, id, &req.code).await?;
    if let Some(refusal) = accounts::totp_refusal(outcome, false) {
        return Err(refusal);
    }
    ledger::append(&mut tx, account.pial_id, Some(id), "totp_disabled", json!({}), "elohim-veni").await?;
    events::emit_account(&mut tx, id, "account.credentials_changed", json!({ "totp_enabled": false })).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// Generates a fresh set of backup codes, replacing the old ones. The
/// plaintext codes are in this response and nowhere else, ever.
pub async fn generate_backup_codes(State(state): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let mut tx = state.pool.begin().await?;
    let account = account_or_404(&mut tx, id).await?;
    let codes = credentials::regenerate_backup_codes(&mut tx, id).await?;
    ledger::append(&mut tx, account.pial_id, Some(id), "backup_codes_generated",
        json!({ "count": codes.len() }), "elohim-veni").await?;
    tx.commit().await?;
    Ok(Json(json!({ "codes": codes })))
}

pub async fn backup_code_status(State(state): State<AppState>, Path(id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    account_or_404(&mut conn, id).await?;
    let remaining = credentials::unused_backup_codes(&mut conn, id).await?;
    Ok(Json(json!({ "remaining": remaining, "needs_backup_codes": remaining == 0 })))
}

/// Superset: spends one backup code of an account, for flows that prove
/// control with one outside sign-in.
pub async fn consume_backup_code(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Json(req): Json<CodeBody>,
) -> ApiResult<StatusCode> {
    let mut tx = state.pool.begin().await?;
    let account = account_or_404(&mut tx, id).await?;
    if !credentials::consume_backup_code(&mut tx, id, &req.code).await? {
        return Err(ApiError::unauthorized("backup_code_invalid", "Invalid backup code."));
    }
    ledger::append(&mut tx, account.pial_id, Some(id), "backup_code_used", json!({}), "elohim-veni").await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

// ── Sign in, sessions, tokens ─────────────────────────────────────────────────

pub async fn login(State(state): State<AppState>, Json(req): Json<LoginRequest>) -> ApiResult<Json<Value>> {
    Ok(Json(accounts::login(&state.pool, &state.tokens, req).await?))
}

pub async fn jwks(State(state): State<AppState>) -> ApiResult<impl IntoResponse> {
    Ok((
        [(header::CACHE_CONTROL, format!("public, max-age={JWKS_MAX_AGE_SECS}"))],
        Json(state.tokens.jwks()?),
    ))
}

#[derive(Debug, Deserialize)]
pub struct TokenBody {
    pub session_token: String,
}

fn no_session() -> ApiError {
    ApiError::unauthorized("invalid_session", "The session is unknown, expired or revoked.")
}

/// Resolves a session token to who it is, with a fresh assertion token.
pub async fn resolve_session(State(state): State<AppState>, Json(req): Json<TokenBody>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    let Some(session) = sessions::find_live(&mut conn, req.session_token.trim()).await? else {
        return Err(no_session());
    };
    let (account, pial_view) = view::load_account_view(&mut conn, session.account_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("session {} names a missing account", session.id))?;
    if account.deactivated_at.is_some() || vocab::is_gone(&pial_view.status) {
        return Err(no_session());
    }
    let access_token = state.tokens.issue(&claims_for(&pial_view, &account, session.id))?;
    let f = &pial_view.facts;
    let mut facts = json!({
        "age_verified": f.age_verified,
        "is_adult": f.is_adult,
        "is_minor": f.is_minor,
        "kyc_tier": f.kyc_tier,
        // Superset of the contract's facts.
        "dob_locked": f.dob_locked,
        "kyc_verified_at": f.kyc_verified_at,
    });
    if let Some(dob) = f.dob {
        facts["dob"] = json!(dob);
    }
    Ok(Json(json!({
        "account_id": account.account_id,
        "pial_id": account.pial_id,
        "handle": account.handle,
        "role": pial_view.role,
        "caps": pial_view.granted(Utc::now()),
        "facts": facts,
        "status": vocab::account_status(&pial_view.status, false, false),
        "access_token": access_token,
        "session_created_at": session.created_at,
        "session_expires_at": session.expires_at,
        // Superset.
        "session_id": session.id,
        "login_account_id": session.login_account_id,
        "is_primary": account.is_primary,
        "tier": pial_view.tier,
    })))
}

#[derive(Debug, Deserialize)]
pub struct RevokeBody {
    #[serde(default)]
    pub session_token: Option<String>,
    #[serde(default)]
    pub account_id: Option<Uuid>,
    #[serde(default)]
    pub pial_id: Option<Uuid>,
    #[serde(default)]
    pub all: bool,
    #[serde(default)]
    pub keep_session_token: Option<String>,
}

/// {session_token} ends one session; {account_id, all:true, keep_session_token?}
/// ends an account's sessions but the kept one; {pial_id, all:true} (superset)
/// ends every session of an identity.
pub async fn revoke_sessions(State(state): State<AppState>, Json(req): Json<RevokeBody>) -> ApiResult<StatusCode> {
    let mut conn = state.pool.acquire().await?;
    match (req.session_token.as_deref(), req.account_id, req.pial_id, req.all) {
        (Some(token), None, None, false) => {
            sessions::revoke_token(&mut conn, token.trim(), "signed out").await?;
        }
        (None, Some(account), None, true) => {
            sessions::revoke_account(&mut conn, account, req.keep_session_token.as_deref(), "all sessions ended").await?;
        }
        (None, None, Some(pial_id), true) => {
            sessions::revoke_pial(&mut conn, pial_id, "all sessions ended").await?;
        }
        _ => {
            return Err(ApiError::bad_request(
                "revoke_target_invalid",
                "Send {session_token}, {account_id, all:true} or {pial_id, all:true}.",
            ))
        }
    }
    Ok(StatusCode::NO_CONTENT)
}

#[derive(Debug, Deserialize)]
pub struct AccountQuery {
    pub account_id: Uuid,
}

pub async fn list_sessions(State(state): State<AppState>, Query(q): Query<AccountQuery>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    let list = sessions::list(&mut conn, q.account_id).await?;
    Ok(Json(json!({ "sessions": list })))
}

pub async fn delete_session(
    State(state): State<AppState>,
    Path(id): Path<Uuid>,
    Query(q): Query<AccountQuery>,
) -> ApiResult<StatusCode> {
    let mut conn = state.pool.acquire().await?;
    if sessions::revoke_one(&mut conn, id, q.account_id, "revoked by owner").await? == 0 {
        return Err(ApiError::not_found("not_found", "No such live session on this account."));
    }
    Ok(StatusCode::NO_CONTENT)
}

#[derive(Debug, Deserialize)]
pub struct SwitchBody {
    pub session_token: String,
    pub account_id: Uuid,
}

/// Superset: the session acts as another account of the same identity.
pub async fn switch_session(State(state): State<AppState>, Json(req): Json<SwitchBody>) -> ApiResult<StatusCode> {
    let mut conn = state.pool.acquire().await?;
    if !sessions::switch(&mut conn, req.session_token.trim(), req.account_id).await? {
        return Err(ApiError::forbidden(
            "account_not_switchable",
            "That account is not an active account of this session's identity.",
        ));
    }
    Ok(StatusCode::NO_CONTENT)
}

// ── PIALs ─────────────────────────────────────────────────────────────────────

pub async fn get_pial(State(state): State<AppState>, Path(pial_id): Path<Uuid>) -> ApiResult<Json<Value>> {
    let mut conn = state.pool.acquire().await?;
    let v = view::load_pial(&mut conn, pial_id)
        .await?
        .ok_or_else(|| ApiError::not_found("not_found", "No such PIAL."))?;
    Ok(Json(v.to_json(Utc::now())))
}

#[derive(Debug, Deserialize)]
pub struct KycBody {
    pub verified: bool,
    #[serde(default)]
    pub tier: Option<String>,
}

fn parse_dob(raw: Option<&str>) -> ApiResult<Option<NaiveDate>> {
    match raw.map(str::trim) {
        None | Some("") => Ok(None),
        Some(s) => NaiveDate::parse_from_str(s, "%Y-%m-%d")
            .map(Some)
            .map_err(|_| ApiError::bad_request("invalid_date_of_birth", "dob must be YYYY-MM-DD.")),
    }
}

/// Two body shapes, one meaning each:
///
/// The stated facts — {kyc_tier?, age_verified?, dob?, dob_locked?, source,
/// account_id?}: each field present is set; is_adult / is_minor are worked out
/// here from the date of birth (pial::set_facts_direct).
///
/// The operations — {dob?, dob_documented?, kyc?: {verified, tier?}, by,
/// account_id?}: SetPIALBirthday (refused once locked), the eKYC documented
/// date, SetPIALVerified(tier) and SetPIALUnverified.
///
/// A body carrying `kyc` or `dob_documented` is the second shape; any other is
/// the first. Answers with the identity as it now stands.
pub async fn put_facts(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(body): Json<Value>,
) -> ApiResult<Json<Value>> {
    let account_id = match body.get("account_id") {
        None | Some(Value::Null) => None,
        Some(v) => Some(serde_json::from_value::<Uuid>(v.clone()).map_err(|_| bad_json("account_id must be a UUID"))?),
    };
    let dob = parse_dob(body.get("dob").and_then(Value::as_str))?;
    let operations = body.get("kyc").is_some() || body.get("dob_documented").is_some();
    let mut tx = state.pool.begin().await?;
    if operations {
        let kyc = match body.get("kyc") {
            None | Some(Value::Null) => None,
            Some(k) => {
                let k: KycBody = serde_json::from_value(k.clone()).map_err(|_| bad_json("kyc is {verified, tier?}"))?;
                Some(if k.verified { Kyc::Verify(k.tier) } else { Kyc::Unverify })
            }
        };
        let by = body["by"].as_str().or(body["source"].as_str()).unwrap_or_default().trim().to_string();
        if by.is_empty() {
            return Err(ApiError::bad_request("bad_request", "by names who made the change."));
        }
        if dob.is_none() && kyc.is_none() {
            return Err(ApiError::bad_request("bad_request", "Send dob, kyc, or both."));
        }
        pial::set_facts(&mut tx, pial_id, FactsChange {
            dob,
            dob_documented: body["dob_documented"].as_bool().unwrap_or(false),
            kyc,
            by,
            account_id,
        })
        .await?;
    } else {
        let bool_field = |key: &str| -> ApiResult<Option<bool>> {
            match body.get(key) {
                None | Some(Value::Null) => Ok(None),
                Some(Value::Bool(b)) => Ok(Some(*b)),
                Some(_) => Err(bad_json(&format!("{key} must be a boolean"))),
            }
        };
        let kyc_tier = match body.get("kyc_tier") {
            None | Some(Value::Null) => None,
            Some(Value::String(t)) => Some(t.clone()),
            Some(_) => return Err(bad_json("kyc_tier must be a string")),
        };
        let facts = DirectFacts {
            kyc_tier,
            age_verified: bool_field("age_verified")?,
            dob,
            dob_locked: bool_field("dob_locked")?,
            source: body["source"].as_str().or(body["by"].as_str()).map(str::trim).filter(|s| !s.is_empty())
                .unwrap_or("unspecified").to_string(),
            account_id,
        };
        if facts.kyc_tier.is_none() && facts.age_verified.is_none() && facts.dob.is_none() && facts.dob_locked.is_none() {
            return Err(ApiError::bad_request("bad_request", "Send at least one of kyc_tier, age_verified, dob, dob_locked."));
        }
        pial::set_facts_direct(&mut tx, pial_id, facts).await?;
    }
    let v = view::load_pial(&mut tx, pial_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("PIAL {pial_id} vanished inside its own transaction"))?;
    tx.commit().await?;
    Ok(Json(v.to_json(Utc::now())))
}

#[derive(Debug, Deserialize)]
pub struct RoleBody {
    pub role: String,
    #[serde(default)]
    pub by: String,
}

pub async fn put_role(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<RoleBody>,
) -> ApiResult<StatusCode> {
    let mut tx = state.pool.begin().await?;
    pial::set_role(&mut tx, pial_id, &req.role, by_or_unknown(&req.by)).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

#[derive(Debug, Deserialize)]
pub struct StatusBody {
    pub status: String,
    #[serde(default)]
    pub reason: String,
    pub by: String,
}

pub async fn put_status(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<StatusBody>,
) -> ApiResult<StatusCode> {
    let mut tx = state.pool.begin().await?;
    pial::set_status(&mut tx, pial_id, &req.status.to_lowercase(), &req.reason, &req.by).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

#[derive(Debug, Deserialize)]
pub struct CapabilityBody {
    pub state: String,
    #[serde(default)]
    pub reason: String,
    pub by: String,
    #[serde(default)]
    pub expires_at: Option<DateTime<Utc>>,
}

pub async fn put_capability(
    State(state): State<AppState>,
    Path((pial_id, cap)): Path<(Uuid, String)>,
    Json(req): Json<CapabilityBody>,
) -> ApiResult<StatusCode> {
    let mut tx = state.pool.begin().await?;
    pial::set_capability(&mut tx, pial_id, &cap, &req.state, &req.reason, &req.by, req.expires_at).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// {trust_level?, nsfw_tier?, enforcement_state?, cooldown_until?, record_violation?, reason, by}
/// A field left out is unchanged; cooldown_until: null clears it.
pub async fn put_trust(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(body): Json<Value>,
) -> ApiResult<StatusCode> {
    fn int(body: &Value, key: &str) -> ApiResult<Option<i32>> {
        match body.get(key) {
            None | Some(Value::Null) => Ok(None),
            Some(v) => v
                .as_i64()
                .and_then(|n| i32::try_from(n).ok())
                .map(Some)
                .ok_or_else(|| bad_json(&format!("{key} must be an integer"))),
        }
    }
    let cooldown_until = match body.get("cooldown_until") {
        None => None,
        Some(Value::Null) => Some(None),
        Some(v) => Some(Some(
            serde_json::from_value::<DateTime<Utc>>(v.clone())
                .map_err(|_| bad_json("cooldown_until must be an RFC 3339 time"))?,
        )),
    };
    let enforcement_state = match body.get("enforcement_state") {
        None | Some(Value::Null) => None,
        Some(Value::String(s)) => Some(s.clone()),
        Some(_) => return Err(bad_json("enforcement_state must be a string")),
    };
    let by = body["by"].as_str().unwrap_or_default().trim().to_string();
    if by.is_empty() {
        return Err(ApiError::bad_request("by_required", "by names who made the change."));
    }
    let change = TrustChange {
        trust_level: int(&body, "trust_level")?,
        nsfw_tier: int(&body, "nsfw_tier")?,
        enforcement_state,
        cooldown_until,
        record_violation: body["record_violation"].as_bool().unwrap_or(false),
        reason: body["reason"].as_str().unwrap_or_default().to_string(),
        by,
    };
    let mut tx = state.pool.begin().await?;
    pial::set_trust(&mut tx, pial_id, change).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

pub async fn ban(State(state): State<AppState>, Path(pial_id): Path<Uuid>, Json(req): Json<ReasonBody>) -> ApiResult<StatusCode> {
    let mut tx = state.pool.begin().await?;
    pial::ban(&mut tx, pial_id, &req.reason, by_or_unknown(&req.by)).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

pub async fn unban(State(state): State<AppState>, Path(pial_id): Path<Uuid>, Json(req): Json<ReasonBody>) -> ApiResult<StatusCode> {
    let mut tx = state.pool.begin().await?;
    pial::unban(&mut tx, pial_id, &req.reason, by_or_unknown(&req.by)).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

#[derive(Debug, Deserialize)]
pub struct TombstoneBody {
    #[serde(default)]
    pub purge: bool,
    #[serde(default)]
    pub reason: String,
    pub by: String,
}

pub async fn tombstone(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<TombstoneBody>,
) -> ApiResult<StatusCode> {
    let mut tx = state.pool.begin().await?;
    pial::tombstone(&mut tx, pial_id, req.purge, &req.reason, &req.by).await?;
    tx.commit().await?;
    Ok(StatusCode::NO_CONTENT)
}

/// The author of an enforcement action, as recorded. An action whose caller
/// did not say who it was is recorded as exactly that.
fn by_or_unknown(by: &str) -> &str {
    let by = by.trim();
    if by.is_empty() {
        "unspecified"
    } else {
        by
    }
}
