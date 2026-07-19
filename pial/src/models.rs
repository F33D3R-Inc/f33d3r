// F33D3R PIAL — Core Identity Models
//
// PIAL (Persistent Identity + Access Layer) is the Apple ID of F33D3R.
// This crate defines the canonical types shared across all brains.
// Brains import this crate as a library dependency — they do not
// redefine these types independently.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// The immutable identity anchor. Created once per user, never changed.
/// Accounts are ephemeral (handles change). PIALRoot is permanent.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PIALRoot {
    pub pial_id:       Uuid,
    pub public_key:    String,        // Ed25519 public key (base64)
    pub state_hash:    String,        // SHA-256 of capability tree at last change
    pub created_at:    DateTime<Utc>,
    pub is_tombstoned: bool,          // permanent ban — immutable once set true
    pub deleted_at:    Option<DateTime<Utc>>,
}

/// One capability node in the PIAL capability tree.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PIALCapability {
    pub pial_id:    Uuid,
    pub capability: CapabilityKind,
    pub state:      CapabilityState,
    pub expires_at: Option<DateTime<Utc>>,
    pub reason:     String,
    pub granted_by: String,           // "system" | "admin:<handle>" | "elohim-veni"
    pub updated_at: DateTime<Utc>,
}

impl PIALCapability {
    /// Returns true if this capability is currently active.
    pub fn is_active(&self) -> bool {
        if self.state != CapabilityState::Granted {
            return false;
        }
        if let Some(exp) = self.expires_at {
            return Utc::now() < exp;
        }
        true
    }
}

/// All capability types. PIAL is the single system of record and sole authority for
/// capabilities; only the user or an admin may change them. Moderation brains (abraxas,
/// zodacare, verity, Elohim Veni) are pipelines that propose changes by writing PIAL —
/// none owns auth.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum CapabilityKind {
    Posting,
    Messaging,
    MusicUpload,
    RealmProgression,
    NsfwAccess,
    Monetization,
    NewAccountTrust,
}

impl CapabilityKind {
    /// Default capabilities granted to every new PIAL at onboarding.
    pub fn defaults() -> Vec<Self> {
        vec![
            Self::Posting,
            Self::Messaging,
            Self::MusicUpload,
            Self::RealmProgression,
            Self::NewAccountTrust,
        ]
    }
}

impl std::fmt::Display for CapabilityKind {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let s = serde_json::to_value(self).unwrap();
        write!(f, "{}", s.as_str().unwrap_or("UNKNOWN"))
    }
}

/// Capability state values.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CapabilityState {
    Granted,
    Restricted,
    Revoked,
    Cooldown,
}

/// Full capability map for one PIAL — read from the PIAL record (feed-engine).
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct PIALCapabilityMap(pub Vec<PIALCapability>);

impl PIALCapabilityMap {
    pub fn can(&self, kind: &CapabilityKind) -> bool {
        self.0.iter().any(|c| &c.capability == kind && c.is_active())
    }
}

/// Signed capability token for offline verification — minted from the PIAL record.
/// Brains validate the signature locally without calling back on every request.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PIALToken {
    pub pial_id:      Uuid,
    pub capabilities: Vec<CapabilityKind>,
    pub issued_at:    DateTime<Utc>,
    pub expires_at:   DateTime<Utc>,
    pub signature:    String,         // Ed25519 signature of (pial_id + capabilities + expiry)
}

impl PIALToken {
    /// Token is valid if: not expired AND signature valid.
    /// Signature verification requires the Elohim Veni public key.
    pub fn is_expired(&self) -> bool {
        Utc::now() >= self.expires_at
    }
}

/// PIAL enforcement decision — result of a capability check request.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PIALDecision {
    pub decision_id:  Uuid,
    pub pial_id:      Uuid,
    pub capability:   CapabilityKind,
    pub action:       DecisionAction,
    pub reason:       String,
    pub decided_at:   DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum DecisionAction {
    Allow,
    AllowRestricted,
    Quarantine,
    Deny,
}

/// Trust score for a PIAL — a moderation signal written into the PIAL record.
/// Advisory input to capability decisions; it does not own auth.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PIALTrustScore {
    pub pial_id:           Uuid,
    pub trust_level:       u8,        // 0–100
    pub enforcement_state: EnforcementState,
    pub violation_count:   u32,
    pub last_violation_at: Option<DateTime<Utc>>,
    pub cooldown_until:    Option<DateTime<Utc>>,
    pub updated_at:        DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EnforcementState {
    None,
    Warning,
    Restricted,
    Suspended,
    Terminated,
}
