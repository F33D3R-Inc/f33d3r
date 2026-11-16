//! The resolved view of an identity: one PIAL, its facts, its capabilities, its
//! trust and its accounts. Every answer this brain gives about a person — the
//! HTTP reads, the assertion token, the identity.events snapshot — is built from
//! this one view, so they cannot disagree with each other.

use std::collections::BTreeMap;

use anyhow::Result;
use chrono::{DateTime, NaiveDate, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sqlx::PgConnection;
use uuid::Uuid;

use super::vocab;

#[derive(Debug, Clone, Serialize)]
pub struct Facts {
    pub dob: Option<NaiveDate>,
    pub dob_locked: bool,
    pub age_verified: bool,
    pub is_adult: bool,
    pub is_minor: bool,
    pub kyc_tier: String,
    pub kyc_verified_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct CapRow {
    pub capability: String,
    pub state: String,
    pub expires_at: Option<DateTime<Utc>>,
    pub reason: String,
    pub granted_by: String,
    pub updated_at: DateTime<Utc>,
}

impl CapRow {
    /// model.PIALCapability.IsGranted: granted and not expired, or a cooldown
    /// that has ended.
    pub fn is_granted(&self, now: DateTime<Utc>) -> bool {
        match self.state.as_str() {
            vocab::STATE_COOLDOWN => self.expires_at.is_some_and(|e| now > e),
            vocab::STATE_GRANTED => self.expires_at.map_or(true, |e| now <= e),
            _ => false,
        }
    }
}

#[derive(Debug, Clone, Serialize, sqlx::FromRow)]
pub struct Trust {
    pub trust_level: i32,
    pub nsfw_tier: i32,
    pub enforcement_state: String,
    pub violation_count: i32,
    pub last_violation_at: Option<DateTime<Utc>>,
    pub cooldown_until: Option<DateTime<Utc>>,
    pub score: f64,
    pub anomaly_score: f64,
    pub last_decision: Option<String>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, sqlx::FromRow)]
pub struct AccountRow {
    pub account_id: Uuid,
    pub pial_id: Uuid,
    pub handle: String,
    pub is_primary: bool,
    pub bound_at: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
    pub deactivated_at: Option<DateTime<Utc>>,
    pub deactivated_reason: String,
    pub purged_at: Option<DateTime<Utc>>,
}

/// Selects an AccountRow; append a WHERE clause.
pub const ACCOUNT_SELECT: &str = "SELECT id AS account_id, pial_id, handle::text AS handle, is_primary, \
     bound_at, created_at, deactivated_at, deactivated_reason, purged_at FROM accounts";

#[derive(Debug, Clone, sqlx::FromRow)]
struct PialRow {
    pial_id: Uuid,
    status: String,
    tier: String,
    role: String,
    dob: Option<NaiveDate>,
    dob_locked: bool,
    age_verified: bool,
    is_adult: bool,
    is_minor: bool,
    kyc_tier: String,
    kyc_verified_at: Option<DateTime<Utc>>,
    created_at: DateTime<Utc>,
    tombstoned_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone)]
pub struct PialView {
    pub pial_id: Uuid,
    pub status: String,
    pub tier: String,
    pub role: String,
    pub facts: Facts,
    pub caps: BTreeMap<String, CapRow>,
    pub trust: Trust,
    pub accounts: Vec<AccountRow>,
    pub created_at: DateTime<Utc>,
    pub tombstoned_at: Option<DateTime<Utc>>,
}

impl PialView {
    /// The capabilities currently held, sorted.
    pub fn granted(&self, now: DateTime<Utc>) -> Vec<String> {
        // BTreeMap iterates in key order, so the list is sorted by construction.
        self.caps
            .values()
            .filter(|c| c.is_granted(now))
            .map(|c| c.capability.clone())
            .collect()
    }

    /// IsAccountStopped (feed-engine csam_queue.go): access has been ended when
    /// both posting and messaging are revoked. Revoked, not merely missing.
    pub fn is_stopped(&self) -> bool {
        let revoked = |cap: &str| {
            self.caps
                .get(cap)
                .is_some_and(|c| c.state == vocab::STATE_REVOKED)
        };
        revoked(vocab::CAP_POSTING) && revoked(vocab::CAP_MESSAGING)
    }

    pub fn cap_states_json(&self) -> Value {
        let map: serde_json::Map<String, Value> = self
            .caps
            .values()
            .map(|c| {
                (
                    c.capability.clone(),
                    json!({
                        "state": c.state,
                        "expires_at": c.expires_at,
                        "reason": c.reason,
                        "granted_by": c.granted_by,
                        "updated_at": c.updated_at,
                    }),
                )
            })
            .collect();
        Value::Object(map)
    }

    /// The identity, everything this brain asserts about it, in one object.
    pub fn to_json(&self, now: DateTime<Utc>) -> Value {
        json!({
            "pial_id": self.pial_id,
            "status": self.status,
            "tier": self.tier,
            "role": self.role,
            "facts": self.facts,
            "caps": self.granted(now),
            "cap_states": self.cap_states_json(),
            "trust": self.trust,
            "accounts": self.accounts.iter().map(account_json).collect::<Vec<_>>(),
            "account_ids": self.accounts.iter().map(|a| a.account_id).collect::<Vec<_>>(),
            "created_at": self.created_at,
            "tombstoned_at": self.tombstoned_at,
        })
    }
}

pub fn account_json(a: &AccountRow) -> Value {
    json!({
        "account_id": a.account_id,
        "pial_id": a.pial_id,
        "handle": a.handle,
        "is_primary": a.is_primary,
        "bound_at": a.bound_at,
        "created_at": a.created_at,
        "deactivated_at": a.deactivated_at,
        "deactivated_reason": a.deactivated_reason,
        "purged_at": a.purged_at,
    })
}

/// Loads the whole view of one PIAL, or None when no such PIAL exists. Every
/// query error is returned: an unreadable capability table is never an empty
/// one.
pub async fn load_pial(conn: &mut PgConnection, pial_id: Uuid) -> Result<Option<PialView>> {
    let Some(p) = sqlx::query_as::<_, PialRow>(
        "SELECT pial_id, status, tier, role, dob, dob_locked, age_verified, is_adult, is_minor,
                kyc_tier, kyc_verified_at, created_at, tombstoned_at
           FROM pial_states WHERE pial_id = $1",
    )
    .bind(pial_id)
    .fetch_optional(&mut *conn)
    .await?
    else {
        return Ok(None);
    };

    let caps = sqlx::query_as::<_, CapRow>(
        "SELECT capability, state, expires_at, reason, granted_by, updated_at
           FROM pial_capabilities WHERE pial_id = $1",
    )
    .bind(pial_id)
    .fetch_all(&mut *conn)
    .await?
    .into_iter()
    .map(|c| (c.capability.clone(), c))
    .collect();

    // trg_pial_init_trust guarantees the row; its absence is a broken
    // invariant, reported as one.
    let trust = sqlx::query_as::<_, Trust>(
        "SELECT trust_level, nsfw_tier, enforcement_state, violation_count, last_violation_at,
                cooldown_until, score, anomaly_score, last_decision, updated_at
           FROM trust_scores WHERE pial_id = $1",
    )
    .bind(pial_id)
    .fetch_optional(&mut *conn)
    .await?
    .ok_or_else(|| anyhow::anyhow!("PIAL {pial_id} has no trust_scores row"))?;

    let accounts = sqlx::query_as::<_, AccountRow>(&format!(
        "{ACCOUNT_SELECT} WHERE pial_id = $1 ORDER BY is_primary DESC, bound_at, created_at"
    ))
    .bind(pial_id)
    .fetch_all(&mut *conn)
    .await?;

    Ok(Some(PialView {
        pial_id: p.pial_id,
        status: p.status,
        tier: p.tier,
        role: p.role,
        facts: Facts {
            dob: p.dob,
            dob_locked: p.dob_locked,
            age_verified: p.age_verified,
            is_adult: p.is_adult,
            is_minor: p.is_minor,
            kyc_tier: p.kyc_tier,
            kyc_verified_at: p.kyc_verified_at,
        },
        caps,
        trust,
        accounts,
        created_at: p.created_at,
        tombstoned_at: p.tombstoned_at,
    }))
}

pub async fn load_account(conn: &mut PgConnection, account_id: Uuid) -> Result<Option<AccountRow>> {
    Ok(
        sqlx::query_as::<_, AccountRow>(&format!("{ACCOUNT_SELECT} WHERE id = $1"))
            .bind(account_id)
            .fetch_optional(&mut *conn)
            .await?,
    )
}

pub async fn load_account_by_handle(
    conn: &mut PgConnection,
    handle: &str,
) -> Result<Option<AccountRow>> {
    Ok(
        sqlx::query_as::<_, AccountRow>(&format!("{ACCOUNT_SELECT} WHERE handle = $1::citext"))
            .bind(handle)
            .fetch_optional(&mut *conn)
            .await?,
    )
}

/// The account and the identity it belongs to, or None when either is missing.
pub async fn load_account_view(
    conn: &mut PgConnection,
    account_id: Uuid,
) -> Result<Option<(AccountRow, PialView)>> {
    let Some(account) = load_account(conn, account_id).await? else {
        return Ok(None);
    };
    let pial = load_pial(conn, account.pial_id)
        .await?
        .ok_or_else(|| anyhow::anyhow!("account {account_id} points at missing PIAL {}", account.pial_id))?;
    Ok(Some((account, pial)))
}
