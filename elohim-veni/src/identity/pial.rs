//! Writes to a PIAL: its minting, facts, role, status, capabilities, trust,
//! ban and tombstone. Each function runs inside the caller's transaction and
//! appends the ledger row and the identity.events row for what it changed.
//!
//! The semantics are feed-engine's (internal/db/pial.go, founder.go,
//! csam_queue.go, works.go purge), ported; where this module differs it says so.

use chrono::{DateTime, NaiveDate, Utc};
use serde_json::{json, Value};
use sqlx::PgConnection;
use uuid::Uuid;

use super::{events, ledger, sessions, view, vocab};
use crate::api::{ApiError, ApiResult};

/// Fails with 404 unless the PIAL exists; returns its current status and role.
/// Locks the row, so concurrent writers to one identity are serialised.
pub async fn lock(conn: &mut PgConnection, pial_id: Uuid) -> ApiResult<(String, String)> {
    sqlx::query_as::<_, (String, String)>(
        "SELECT status, role FROM pial_states WHERE pial_id = $1 FOR UPDATE",
    )
    .bind(pial_id)
    .fetch_optional(&mut *conn)
    .await?
    .ok_or_else(|| ApiError::not_found("not_found", "No such PIAL."))
}

/// Grants the default capabilities to a PIAL that has none
/// (GrantDefaultCapabilities). Called wherever a PIAL comes into existence,
/// so no reader ever has to repair an unseeded identity on the read path.
pub async fn seed_default_capabilities(conn: &mut PgConnection, pial_id: Uuid) -> anyhow::Result<()> {
    let defaults: Vec<String> = vocab::DEFAULT_CAPABILITIES.iter().map(|s| s.to_string()).collect();
    sqlx::query(
        "INSERT INTO pial_capabilities (pial_id, capability, state, granted_by)
         SELECT $1, c, 'granted', 'system' FROM unnest($2::text[]) AS c
         ON CONFLICT (pial_id, capability) DO NOTHING",
    )
    .bind(pial_id)
    .bind(&defaults)
    .execute(conn)
    .await?;
    Ok(())
}

/// The identity facts a date of birth implies, computed by the database
/// against its own clock (setPIALBirthdaySQL). Every parameter is cast: the
/// statement must resolve age(date), not age(unknown).
const DOB_FACTS_SQL: &str = "
    dob      = $1::date,
    is_adult = (date_part('year', age($1::date)) >= 18),
    is_minor = (date_part('year', age($1::date)) <  18)";

/// Mints a new PIAL with a self-reported date of birth, as signup does:
/// CreatePIAL + SetPIALBirthday in one row. role is 'user' or, for the first
/// account ever, 'founder' (decided by the caller, inside its transaction).
pub async fn mint(conn: &mut PgConnection, dob: NaiveDate, role: &str) -> anyhow::Result<Uuid> {
    let pial_id = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO pial_states (pial_id, role, dob, kyc_tier, age_verified, is_adult, is_minor)
         VALUES ($2, $3, $1::date, 'basic', TRUE,
                 (date_part('year', age($1::date)) >= 18),
                 (date_part('year', age($1::date)) <  18))",
    )
    .bind(dob)
    .bind(pial_id)
    .bind(role)
    .execute(&mut *conn)
    .await?;
    seed_default_capabilities(conn, pial_id).await?;
    Ok(pial_id)
}

// ── Facts ─────────────────────────────────────────────────────────────────────

pub enum Kyc {
    /// SetPIALVerified, with the requested tier (basic | soft | full).
    Verify(Option<String>),
    /// SetPIALUnverified.
    Unverify,
}

pub struct FactsChange {
    pub dob: Option<NaiveDate>,
    /// The date of birth comes from an identity document (eKYC OCR): it is
    /// recorded even over a locked one, and locks it.
    pub dob_documented: bool,
    pub kyc: Option<Kyc>,
    pub by: String,
    pub account_id: Option<Uuid>,
}

pub async fn set_facts(conn: &mut PgConnection, pial_id: Uuid, change: FactsChange) -> ApiResult<()> {
    let (status, _) = lock(conn, pial_id).await?;
    if vocab::is_gone(&status) {
        return Err(ApiError::conflict("pial_tombstoned", "This identity no longer exists."));
    }
    let mut changed = Vec::new();

    if let Some(dob) = change.dob {
        if dob > Utc::now().date_naive() {
            return Err(ApiError::bad_request("invalid_date_of_birth", "A date of birth cannot be in the future."));
        }
        if change.dob_documented {
            sqlx::query(&format!(
                "UPDATE pial_states SET {DOB_FACTS_SQL}, dob_locked = TRUE, updated_at = NOW()
                  WHERE pial_id = $2::uuid"
            ))
            .bind(dob)
            .bind(pial_id)
            .execute(&mut *conn)
            .await?;
            ledger::append(conn, pial_id, change.account_id, "dob_documented",
                json!({ "granted_by": change.by }), &change.by).await?;
        } else {
            // SetPIALBirthday: refused once dob_locked — a documented-age-verified
            // identity cannot rewrite its date of birth.
            let n = sqlx::query(&format!(
                "UPDATE pial_states
                    SET {DOB_FACTS_SQL},
                        kyc_tier     = CASE WHEN kyc_tier = 'none' THEN 'basic' ELSE kyc_tier END,
                        age_verified = TRUE,
                        updated_at   = NOW()
                  WHERE pial_id = $2::uuid AND dob_locked = FALSE"
            ))
            .bind(dob)
            .bind(pial_id)
            .execute(&mut *conn)
            .await?
            .rows_affected();
            if n == 0 {
                return Err(ApiError::conflict(
                    "dob_locked",
                    "The date of birth is locked after age verification.",
                ));
            }
            ledger::append(conn, pial_id, change.account_id, "dob_recorded",
                json!({ "granted_by": change.by }), &change.by).await?;
        }
        changed.push("dob");
    }

    match change.kyc {
        Some(Kyc::Verify(tier)) => {
            // SetPIALVerified. An unknown tier becomes 'soft', as it did there.
            let tier = match tier.as_deref() {
                Some(t @ ("basic" | "soft" | "full")) => t.to_string(),
                _ => "soft".to_string(),
            };
            sqlx::query(
                "UPDATE pial_states
                    SET kyc_tier = $1, kyc_verified_at = NOW(), age_verified = TRUE,
                        dob_locked = TRUE, is_adult = TRUE, is_minor = FALSE, updated_at = NOW()
                  WHERE pial_id = $2",
            )
            .bind(&tier)
            .bind(pial_id)
            .execute(&mut *conn)
            .await?;
            ledger::append(conn, pial_id, change.account_id, "age_verified",
                json!({ "tier": tier, "granted_by": change.by }), &change.by).await?;
            changed.push("kyc");
        }
        Some(Kyc::Unverify) => {
            // SetPIALUnverified: demote 'full' to 'basic' — the self-reported
            // date of birth is still on file; only the documented claim goes.
            sqlx::query(
                "UPDATE pial_states
                    SET age_verified = FALSE,
                        kyc_tier     = CASE WHEN kyc_tier = 'full' THEN 'basic' ELSE kyc_tier END,
                        updated_at   = NOW()
                  WHERE pial_id = $1",
            )
            .bind(pial_id)
            .execute(&mut *conn)
            .await?;
            ledger::append(conn, pial_id, change.account_id, "age_unverified",
                json!({ "granted_by": change.by }), &change.by).await?;
            changed.push("kyc");
        }
        None => {}
    }

    Ok(finish_facts(conn, pial_id, &change.by, changed).await?)
}

/// The facts as a writer states them (feed-engine's PIAL setters, carried
/// over as one body): any of kyc_tier, age_verified, dob, dob_locked. is_adult
/// and is_minor are never sent; they are worked out here — from the date of
/// birth when one is on file, and as adult for a documented (soft/full)
/// verification without one, as SetPIALVerified did. A date of birth is not
/// rewritten once locked unless the same write locks it again (a documented
/// date replacing a self-reported one).
pub struct DirectFacts {
    pub kyc_tier: Option<String>,
    pub age_verified: Option<bool>,
    pub dob: Option<NaiveDate>,
    pub dob_locked: Option<bool>,
    pub source: String,
    pub account_id: Option<Uuid>,
}

pub async fn set_facts_direct(conn: &mut PgConnection, pial_id: Uuid, f: DirectFacts) -> ApiResult<()> {
    let (status, _) = lock(conn, pial_id).await?;
    if vocab::is_gone(&status) {
        return Err(ApiError::conflict("pial_tombstoned", "This identity no longer exists."));
    }
    if let Some(tier) = &f.kyc_tier {
        if !matches!(tier.as_str(), "none" | "basic" | "soft" | "full") {
            return Err(ApiError::bad_request("invalid_kyc_tier", "kyc_tier is none, basic, soft or full."));
        }
    }
    if let Some(dob) = f.dob {
        if dob > Utc::now().date_naive() {
            return Err(ApiError::bad_request("invalid_date_of_birth", "A date of birth cannot be in the future."));
        }
        let (current, locked): (Option<NaiveDate>, bool) =
            sqlx::query_as("SELECT dob, dob_locked FROM pial_states WHERE pial_id = $1")
                .bind(pial_id)
                .fetch_one(&mut *conn)
                .await?;
        if locked && current != Some(dob) && f.dob_locked != Some(true) {
            return Err(ApiError::conflict("dob_locked", "The date of birth is locked after age verification."));
        }
    }
    sqlx::query(
        "UPDATE pial_states SET
             dob             = COALESCE($2::date, dob),
             dob_locked      = COALESCE($3, dob_locked),
             kyc_verified_at = CASE WHEN $4::text IN ('soft','full') AND $4::text IS DISTINCT FROM kyc_tier
                                    THEN NOW() ELSE kyc_verified_at END,
             kyc_tier        = COALESCE($4, kyc_tier),
             age_verified    = COALESCE($5, age_verified),
             updated_at      = NOW()
          WHERE pial_id = $1",
    )
    .bind(pial_id)
    .bind(f.dob)
    .bind(f.dob_locked)
    .bind(&f.kyc_tier)
    .bind(f.age_verified)
    .execute(&mut *conn)
    .await?;
    sqlx::query(
        "UPDATE pial_states SET
             is_adult = CASE WHEN dob IS NOT NULL THEN date_part('year', age(dob)) >= 18
                             WHEN age_verified AND kyc_tier IN ('soft','full') THEN TRUE
                             ELSE is_adult END,
             is_minor = CASE WHEN dob IS NOT NULL THEN date_part('year', age(dob)) < 18
                             WHEN age_verified AND kyc_tier IN ('soft','full') THEN FALSE
                             ELSE is_minor END
          WHERE pial_id = $1",
    )
    .bind(pial_id)
    .execute(&mut *conn)
    .await?;
    let detail = json!({
        "kyc_tier": f.kyc_tier, "age_verified": f.age_verified,
        "dob_set": f.dob.is_some(), "dob_locked": f.dob_locked, "source": f.source,
    });
    ledger::append(conn, pial_id, f.account_id, "facts_set", detail.clone(), &f.source).await?;
    events::emit_pial(conn, pial_id, "pial.facts_changed", detail).await?;
    Ok(())
}

async fn finish_facts(conn: &mut PgConnection, pial_id: Uuid, by: &str, changed: Vec<&str>) -> anyhow::Result<()> {
    if changed.is_empty() {
        return Ok(());
    }
    events::emit_pial(conn, pial_id, "pial.facts_changed", json!({ "changed": changed, "by": by })).await
}

// ── Role ──────────────────────────────────────────────────────────────────────

/// SetPIALRole: user | admin. Founder is never assigned or removed here; the
/// database trigger refuses it too, so no path can.
pub async fn set_role(conn: &mut PgConnection, pial_id: Uuid, role: &str, by: &str) -> ApiResult<()> {
    match role {
        vocab::ROLE_USER | vocab::ROLE_ADMIN => {}
        vocab::ROLE_FOUNDER => return Err(founder_permanent()),
        _ => return Err(ApiError::bad_request("role_invalid", "role must be user or admin.")),
    }
    let (_, current) = lock(conn, pial_id).await?;
    if current == vocab::ROLE_FOUNDER {
        return Err(founder_permanent());
    }
    if current == role {
        return Ok(());
    }
    sqlx::query("UPDATE pial_states SET role = $1, updated_at = NOW() WHERE pial_id = $2")
        .bind(role)
        .bind(pial_id)
        .execute(&mut *conn)
        .await?;
    ledger::append(conn, pial_id, None, "role_changed", json!({ "from": current, "to": role, "by": by }), by).await?;
    events::emit_pial(conn, pial_id, "pial.role_changed", json!({ "from": current, "to": role, "by": by })).await?;
    Ok(())
}

fn founder_permanent() -> ApiError {
    ApiError::conflict(
        "founder_role_permanent",
        "The founder role is granted only to the first account and is permanent.",
    )
}

// ── Status ────────────────────────────────────────────────────────────────────

/// Moves an identity between active and suspended. A ban (revoked) and a
/// tombstone have their own operations, because each carries more than a
/// status.
pub async fn set_status(conn: &mut PgConnection, pial_id: Uuid, status: &str, reason: &str, by: &str) -> ApiResult<()> {
    if status != vocab::STATUS_ACTIVE && status != vocab::STATUS_SUSPENDED {
        return Err(ApiError::bad_request(
            "status_invalid",
            "status must be active or suspended; use ban/unban and tombstone for the others.",
        ));
    }
    let (current, _) = lock(conn, pial_id).await?;
    if vocab::is_gone(&current) {
        return Err(ApiError::conflict("pial_tombstoned", "This identity no longer exists."));
    }
    if current == vocab::STATUS_BANNED {
        return Err(ApiError::conflict("pial_banned", "This identity is banned; unban it instead."));
    }
    if current == status {
        return Ok(());
    }
    sqlx::query("UPDATE pial_states SET status = $1, updated_at = NOW() WHERE pial_id = $2")
        .bind(status)
        .bind(pial_id)
        .execute(&mut *conn)
        .await?;
    let detail = json!({ "from": current, "to": status, "reason": reason, "by": by });
    ledger::append(conn, pial_id, None, "status_changed", detail.clone(), by).await?;
    decision_log_status(conn, pial_id, status, reason, by).await?;
    if status == vocab::STATUS_SUSPENDED {
        sessions::revoke_pial(conn, pial_id, "identity suspended").await?;
    }
    events::emit_pial(conn, pial_id, "pial.status_changed", detail).await?;
    Ok(())
}

/// Status changes stay in the decision log too, where the decision engine's
/// audit trail has always recorded them.
async fn decision_log_status(conn: &mut PgConnection, pial_id: Uuid, status: &str, reason: &str, by: &str) -> anyhow::Result<()> {
    let decision = if status == vocab::STATUS_ACTIVE { "ALLOW" } else { "DENY" };
    sqlx::query(
        "INSERT INTO decision_log (pial_id, action, decision, confidence, reason)
         VALUES ($1, 'status_change', $2, 1.0, $3)",
    )
    .bind(pial_id)
    .bind(decision)
    .bind(format!("status set to {status}: {reason} (by {by})"))
    .execute(conn)
    .await?;
    Ok(())
}

// ── Capabilities ──────────────────────────────────────────────────────────────

/// SetCapability: grants, restricts, revokes or cools down one capability.
pub async fn set_capability(
    conn: &mut PgConnection,
    pial_id: Uuid,
    capability: &str,
    state: &str,
    reason: &str,
    by: &str,
    expires_at: Option<DateTime<Utc>>,
) -> ApiResult<()> {
    if !vocab::is_capability(capability) {
        return Err(ApiError::bad_request("capability_unknown", format!("{capability} is not a capability.")));
    }
    if !vocab::CAP_STATES.contains(&state) {
        return Err(ApiError::bad_request(
            "capability_state_invalid",
            "state must be granted, restricted, revoked or cooldown.",
        ));
    }
    if state == vocab::STATE_COOLDOWN && expires_at.is_none() {
        return Err(ApiError::bad_request("cooldown_needs_end", "A cooldown needs expires_at."));
    }
    let (status, _) = lock(conn, pial_id).await?;
    if vocab::is_gone(&status) {
        return Err(ApiError::conflict("pial_tombstoned", "This identity no longer exists."));
    }
    // Idempotent: setting what is already set writes nothing and announces
    // nothing.
    let current: Option<(String, Option<DateTime<Utc>>, String)> = sqlx::query_as(
        "SELECT state, expires_at, reason FROM pial_capabilities WHERE pial_id = $1 AND capability = $2",
    )
    .bind(pial_id)
    .bind(capability)
    .fetch_optional(&mut *conn)
    .await?;
    if current.as_ref().is_some_and(|(s, e, r)| s == state && *e == expires_at && r == reason) {
        return Ok(());
    }
    write_capability(conn, pial_id, capability, state, reason, by, expires_at).await?;
    let detail = json!({ "capability": capability, "state": state, "reason": reason, "by": by, "expires_at": expires_at });
    ledger::append(conn, pial_id, None, "capability_set", detail.clone(), by).await?;
    events::emit_pial(conn, pial_id, "pial.capability_changed", detail).await?;
    Ok(())
}

async fn write_capability(
    conn: &mut PgConnection,
    pial_id: Uuid,
    capability: &str,
    state: &str,
    reason: &str,
    by: &str,
    expires_at: Option<DateTime<Utc>>,
) -> anyhow::Result<()> {
    sqlx::query(
        "INSERT INTO pial_capabilities (pial_id, capability, state, expires_at, reason, granted_by, updated_at)
         VALUES ($1, $2, $3, $4, $5, $6, NOW())
         ON CONFLICT (pial_id, capability) DO UPDATE
            SET state = EXCLUDED.state, expires_at = EXCLUDED.expires_at, reason = EXCLUDED.reason,
                granted_by = EXCLUDED.granted_by, updated_at = NOW(), expiry_announced_at = NULL",
    )
    .bind(pial_id)
    .bind(capability)
    .bind(state)
    .bind(expires_at)
    .bind(reason)
    .bind(by)
    .execute(conn)
    .await?;
    Ok(())
}

// ── Ban ───────────────────────────────────────────────────────────────────────

/// A ban: status banned, every capability revoked, every session ended. The
/// capabilities held before the ban are written into the ledger row, and unban
/// restores exactly those. A ban of an identity already banned re-asserts the
/// revocations without overwriting what the first ban recorded.
pub async fn ban(conn: &mut PgConnection, pial_id: Uuid, reason: &str, by: &str) -> ApiResult<()> {
    let (status, _) = lock(conn, pial_id).await?;
    if vocab::is_gone(&status) {
        return Err(ApiError::conflict("pial_tombstoned", "This identity no longer exists."));
    }
    let already = last_ban_event(conn, pial_id).await? == Some("pial_banned".to_string());

    let prior = sqlx::query_as::<_, view::CapRow>(
        "SELECT capability, state, expires_at, reason, granted_by, updated_at
           FROM pial_capabilities WHERE pial_id = $1",
    )
    .bind(pial_id)
    .fetch_all(&mut *conn)
    .await?;

    sqlx::query("UPDATE pial_states SET status = 'banned', updated_at = NOW() WHERE pial_id = $1")
        .bind(pial_id)
        .execute(&mut *conn)
        .await?;
    let ban_reason = format!("ban: {reason}");
    for cap in vocab::CAPABILITIES {
        write_capability(conn, pial_id, cap, vocab::STATE_REVOKED, &ban_reason, by, None).await?;
    }
    sessions::revoke_pial(conn, pial_id, "identity banned").await?;

    let event_type = if already { "pial_ban_reasserted" } else { "pial_banned" };
    let mut payload = json!({ "reason": reason, "by": by, "prior_status": status });
    if !already {
        payload["prior_capabilities"] = json!(prior);
    }
    ledger::append(conn, pial_id, None, event_type, payload, by).await?;
    sqlx::query(
        "INSERT INTO decision_log (pial_id, action, decision, confidence, reason)
         VALUES ($1, 'ban', 'DENY', 1.0, $2)",
    )
    .bind(pial_id)
    .bind(reason)
    .execute(&mut *conn)
    .await?;
    events::emit_pial(conn, pial_id, "pial.banned", json!({ "reason": reason, "by": by })).await?;
    Ok(())
}

async fn last_ban_event(conn: &mut PgConnection, pial_id: Uuid) -> anyhow::Result<Option<String>> {
    Ok(sqlx::query_scalar(
        "SELECT event_type FROM pial_event_ledger
          WHERE pial_id = $1 AND event_type IN ('pial_banned','pial_unbanned')
          ORDER BY created_at DESC LIMIT 1",
    )
    .bind(pial_id)
    .fetch_optional(conn)
    .await?)
}

/// Lifts a ban: status active, capabilities restored to what the ban
/// recorded. An identity banned with no recorded prior state (banned before
/// this brain kept one, e.g. imported as stopped) gets the defaults back.
pub async fn unban(conn: &mut PgConnection, pial_id: Uuid, reason: &str, by: &str) -> ApiResult<()> {
    let (status, _) = lock(conn, pial_id).await?;
    if vocab::is_gone(&status) {
        return Err(ApiError::conflict("pial_tombstoned", "This identity no longer exists."));
    }
    // Lifting a suspension is a status change, not a ban's undoing: the
    // capabilities were never taken, so they are not touched. Unbanning an
    // identity that is not barred is a no-op.
    match status.as_str() {
        vocab::STATUS_ACTIVE => return Ok(()),
        vocab::STATUS_SUSPENDED => return set_status(conn, pial_id, vocab::STATUS_ACTIVE, reason, by).await,
        _ => {}
    }
    let prior: Option<Value> = sqlx::query_scalar(
        "SELECT payload->'prior_capabilities' FROM pial_event_ledger
          WHERE pial_id = $1 AND event_type = 'pial_banned'
            AND NOT EXISTS (SELECT 1 FROM pial_event_ledger u
                             WHERE u.pial_id = $1 AND u.event_type = 'pial_unbanned'
                               AND u.created_at > pial_event_ledger.created_at)
          ORDER BY created_at DESC LIMIT 1",
    )
    .bind(pial_id)
    .fetch_optional(&mut *conn)
    .await?
    .flatten();

    sqlx::query("DELETE FROM pial_capabilities WHERE pial_id = $1")
        .bind(pial_id)
        .execute(&mut *conn)
        .await?;
    match prior {
        Some(Value::Array(rows)) => {
            for row in rows {
                let cap: view::CapRow = serde_json::from_value(row)
                    .map_err(|e| anyhow::anyhow!("ledger prior_capabilities unreadable: {e}"))?;
                write_capability(conn, pial_id, &cap.capability, &cap.state, &cap.reason, &cap.granted_by, cap.expires_at).await?;
            }
        }
        _ => seed_default_capabilities(conn, pial_id).await?,
    }
    sqlx::query("UPDATE pial_states SET status = 'active', updated_at = NOW() WHERE pial_id = $1")
        .bind(pial_id)
        .execute(&mut *conn)
        .await?;
    let detail = json!({ "reason": reason, "by": by, "prior_status": status });
    ledger::append(conn, pial_id, None, "pial_unbanned", detail.clone(), by).await?;
    decision_log_status(conn, pial_id, vocab::STATUS_ACTIVE, reason, by).await?;
    events::emit_pial(conn, pial_id, "pial.unbanned", detail).await?;
    Ok(())
}

// ── Trust ─────────────────────────────────────────────────────────────────────

/// A change to the enforcement half of trust (SetTrustScore,
/// SetEnforcementState and the admin suspension's cooldown). A field left None
/// is unchanged; cooldown_until is Some(None) to clear it.
pub struct TrustChange {
    pub trust_level: Option<i32>,
    pub nsfw_tier: Option<i32>,
    pub enforcement_state: Option<String>,
    pub cooldown_until: Option<Option<DateTime<Utc>>>,
    pub record_violation: bool,
    pub reason: String,
    pub by: String,
}

pub async fn set_trust(conn: &mut PgConnection, pial_id: Uuid, c: TrustChange) -> ApiResult<()> {
    if c.trust_level.is_some_and(|v| !(0..=100).contains(&v)) {
        return Err(ApiError::bad_request("trust_level_invalid", "trust_level is 0–100."));
    }
    if c.nsfw_tier.is_some_and(|v| !(0..=4).contains(&v)) {
        return Err(ApiError::bad_request("nsfw_tier_invalid", "nsfw_tier is 0–4."));
    }
    if c.enforcement_state.as_deref().is_some_and(|s| !vocab::ENFORCEMENT_STATES.contains(&s)) {
        return Err(ApiError::bad_request(
            "enforcement_state_invalid",
            "enforcement_state is none, warning, restricted, suspended or terminated.",
        ));
    }
    lock(conn, pial_id).await?;
    sqlx::query(
        "UPDATE trust_scores
            SET trust_level       = COALESCE($2, trust_level),
                nsfw_tier         = COALESCE($3, nsfw_tier),
                enforcement_state = COALESCE($4, enforcement_state),
                cooldown_until    = CASE WHEN $5 THEN $6 ELSE cooldown_until END,
                violation_count   = violation_count + CASE WHEN $7 THEN 1 ELSE 0 END,
                last_violation_at = CASE WHEN $7 THEN NOW() ELSE last_violation_at END,
                updated_at        = NOW()
          WHERE pial_id = $1",
    )
    .bind(pial_id)
    .bind(c.trust_level)
    .bind(c.nsfw_tier)
    .bind(&c.enforcement_state)
    .bind(c.cooldown_until.is_some())
    .bind(c.cooldown_until.flatten())
    .bind(c.record_violation)
    .execute(&mut *conn)
    .await?;
    let detail = json!({
        "trust_level": c.trust_level, "nsfw_tier": c.nsfw_tier,
        "enforcement_state": c.enforcement_state, "cooldown_until": c.cooldown_until,
        "record_violation": c.record_violation, "reason": c.reason, "by": c.by,
    });
    ledger::append(conn, pial_id, None, "trust_changed", detail.clone(), &c.by).await?;
    events::emit_pial(conn, pial_id, "pial.trust_changed", detail).await?;
    Ok(())
}

// ── Tombstone ─────────────────────────────────────────────────────────────────

/// Ends an identity. `purge` is the hard delete feed-engine performs
/// (works.go): every identity fact is erased and the PIAL is left as an opaque
/// UUID marked purged. Without it the identity is tombstoned with its facts
/// retained. Either way every session ends and every account is deactivated.
/// The founder role is permanent and survives both.
pub async fn tombstone(conn: &mut PgConnection, pial_id: Uuid, purge: bool, reason: &str, by: &str) -> ApiResult<()> {
    let (status, _) = lock(conn, pial_id).await?;
    if status == vocab::STATUS_PURGED || (status == vocab::STATUS_TOMBSTONED && !purge) {
        return Ok(());
    }
    if purge {
        sqlx::query(
            "UPDATE pial_states
                SET dob = NULL, dob_locked = FALSE, kyc_tier = 'none', kyc_verified_at = NULL,
                    age_verified = FALSE, is_adult = FALSE, is_minor = FALSE,
                    status = 'purged', tombstoned_at = COALESCE(tombstoned_at, NOW()), updated_at = NOW()
              WHERE pial_id = $1",
        )
    } else {
        sqlx::query(
            "UPDATE pial_states SET status = 'tombstoned', tombstoned_at = NOW(), updated_at = NOW()
              WHERE pial_id = $1",
        )
    }
    .bind(pial_id)
    .execute(&mut *conn)
    .await?;
    sqlx::query(
        "UPDATE accounts SET deactivated_at = COALESCE(deactivated_at, NOW()), deactivated_reason = $2,
                             purged_at = CASE WHEN $3 THEN COALESCE(purged_at, NOW()) ELSE purged_at END
          WHERE pial_id = $1",
    )
    .bind(pial_id)
    .bind(if purge { "purged" } else { "tombstoned" })
    .bind(purge)
    .execute(&mut *conn)
    .await?;
    if purge {
        for table in ["credentials", "account_totp", "backup_codes", "recovery_tokens"] {
            sqlx::query(&format!(
                "DELETE FROM {table} WHERE account_id IN (SELECT id FROM accounts WHERE pial_id = $1)"
            ))
            .bind(pial_id)
            .execute(&mut *conn)
            .await?;
        }
    }
    sessions::revoke_pial(conn, pial_id, "identity ended").await?;
    let event_type = if purge { "pial_purged" } else { "pial_tombstoned" };
    let detail = json!({ "reason": reason, "by": by, "prior_status": status });
    ledger::append(conn, pial_id, None, event_type, detail.clone(), by).await?;
    events::emit_pial(conn, pial_id, &format!("pial.{}", if purge { "purged" } else { "tombstoned" }), detail).await?;
    Ok(())
}

// ── Bootstrap (legacy assertion) ──────────────────────────────────────────────

/// POST /v1/pial/bootstrap: asserts a PIAL minted elsewhere. Idempotent, and
/// it can neither resurrect a revoked PIAL nor change the tier of a live one.
/// A newly asserted PIAL is seeded and announced like any other.
pub async fn bootstrap(conn: &mut PgConnection, pial_id: Uuid, tier: &str) -> anyhow::Result<bool> {
    let created: bool = sqlx::query_scalar(
        "INSERT INTO pial_states (pial_id, tier) VALUES ($1, $2)
         ON CONFLICT (pial_id) DO UPDATE SET updated_at = NOW()
         RETURNING (xmax = 0)",
    )
    .bind(pial_id)
    .bind(tier)
    .fetch_one(&mut *conn)
    .await?;
    if created {
        seed_default_capabilities(conn, pial_id).await?;
        ledger::append(conn, pial_id, None, "pial_asserted", json!({ "tier": tier }), "bootstrap").await?;
        events::emit_pial(conn, pial_id, "pial.asserted", json!({ "tier": tier })).await?;
    }
    Ok(created)
}
