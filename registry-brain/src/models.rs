use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── Handle status / tier enums ─────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize, Clone, PartialEq)]
#[serde(rename_all = "lowercase")]
pub enum HandleStatus {
    Active,
    Inactive,
    Frozen,
    Quarantined,
    Auction,
    Reserved,
}
impl std::fmt::Display for HandleStatus {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            HandleStatus::Active      => write!(f, "active"),
            HandleStatus::Inactive    => write!(f, "inactive"),
            HandleStatus::Frozen      => write!(f, "frozen"),
            HandleStatus::Quarantined => write!(f, "quarantined"),
            HandleStatus::Auction     => write!(f, "auction"),
            HandleStatus::Reserved    => write!(f, "reserved"),
        }
    }
}

// ── Request types ─────────────────────────────────────────────────────────────

/// Register a new handle and bind it to a PIAL immediately.
#[derive(Debug, Deserialize)]
pub struct RegisterHandleReq {
    pub handle:  String,   // desired handle (1–30 chars, [a-zA-Z0-9_])
    pub pial_id: Uuid,     // PIAL to bind to
    /// Optional: tier requested. Standard = free. Premium/Elite require payment (V2).
    #[serde(default = "default_tier")]
    pub tier:    String,
}
fn default_tier() -> String { "standard".into() }

/// Bind an existing handle (status=inactive/quarantined) to a new PIAL.
#[derive(Debug, Deserialize)]
pub struct BindHandleReq {
    pub handle:      String,
    pub pial_id:     Uuid,
    /// The requesting PIAL (must be the current owner or have admin capability).
    pub requester:   Uuid,
}

/// Unbind a handle from its current PIAL (puts handle into inactive).
#[derive(Debug, Deserialize)]
pub struct UnbindHandleReq {
    pub handle:    String,
    pub requester: Uuid,   // must be the current owner
}

/// Transfer a handle from one PIAL to another.
/// IMPORTANT: followers, reputation, wallet stay with the source PIAL.
#[derive(Debug, Deserialize)]
pub struct TransferHandleReq {
    pub handle:     String,
    pub from_pial:  Uuid,
    pub to_pial:    Uuid,
}

/// Create an auction for a handle.
#[derive(Debug, Deserialize)]
pub struct CreateAuctionReq {
    pub handle:              String,
    pub duration_hours:      u32,    // auction duration
    pub starting_price_aet:  i32,
    pub requester:           Uuid,
}

/// Place a bid on an auction.
#[derive(Debug, Deserialize)]
pub struct PlaceBidReq {
    pub bidder_pial: Uuid,
    pub amount_aet:  i32,
}

/// Admin: freeze / quarantine a handle (called by Zodacare safety brain).
#[derive(Debug, Deserialize)]
pub struct AdminActionReq {
    pub action:   String,   // freeze | unfreeze | quarantine | reclaim
    pub reason:   String,
    pub admin_id: String,   // audit trail
}

// ── Response types ────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct HandleResp {
    pub id:             Uuid,
    pub handle:         String,
    pub pial_id:        Uuid,
    pub status:         String,
    pub tier:           String,
    pub created_at:     DateTime<Utc>,
    pub last_bound_at:  DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct ResolveResp {
    pub handle:     String,
    pub pial_id:    Uuid,
    pub status:     String,
    pub tier:       String,
    pub cached:     bool,
}

#[derive(Debug, Serialize)]
pub struct AuctionResp {
    pub id:                   Uuid,
    pub handle:               String,
    pub start_time:           DateTime<Utc>,
    pub end_time:             DateTime<Utc>,
    pub starting_price_aet:   i32,
    pub current_price_aet:    i32,
    pub highest_bidder_pial:  Option<Uuid>,
    pub status:               String,
    pub bid_count:            i64,
}

#[derive(Debug, Serialize)]
pub struct SearchResult {
    pub handle:   String,
    pub status:   String,
    pub tier:     String,
    pub available: bool,
}

// ── DB row types ──────────────────────────────────────────────────────────────

#[derive(Debug, sqlx::FromRow)]
pub struct HandleRow {
    pub id:             Uuid,
    pub handle:         String,
    pub pial_id:        Uuid,
    pub status:         String,
    pub tier:           String,
    pub created_at:     DateTime<Utc>,
    pub updated_at:     DateTime<Utc>,
    pub last_bound_at:  DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
pub struct AuctionRow {
    pub id:                   Uuid,
    pub handle_id:            Uuid,
    pub start_time:           DateTime<Utc>,
    pub end_time:             DateTime<Utc>,
    pub starting_price_aet:   i32,
    pub current_price_aet:    i32,
    pub highest_bidder_pial:  Option<Uuid>,
    pub status:               String,
}
