use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── Constants ─────────────────────────────────────────────────────────────────
// 1 AET = 1_000_000 micro-AET. All balances stored as micro-AET.
pub const MICRO_AET: i64 = 1_000_000;

// Fee basis points (10000 = 100%)
pub const FEE_DEPOSIT:      i64 = 250;  // 2.5%
pub const FEE_TRANSFER:     i64 = 300;  // 3.0%
pub const FEE_TIP:          i64 = 100;  // 1.0%
pub const FEE_SUBSCRIPTION: i64 = 150;  // 1.5%
pub const FEE_BOOST:        i64 = 0;    // no fee — boost is a full burn

// AET/AC conversion rate: 1 Aethyr Credit = 100 micro-AET (0.0001 AET)
pub const AC_TO_UAET: i64 = 100;

// ── Request types ─────────────────────────────────────────────────────────────

/// Fund a wallet via an external rail (fiat → Aethyr Credit → AET mint).
#[derive(Debug, Deserialize)]
pub struct FundReq {
    pub pial_id:        Uuid,
    pub rail:           String,  // apple_iap | gift_card | card | google_play | crypto | internal
    pub fiat_amount:    i64,     // cents
    pub fiat_currency:  Option<String>,
    pub external_ref:   Option<String>,  // Apple receipt hash, gift card hash, etc.
    pub metadata:       Option<serde_json::Value>,
}

/// Transfer AET between PIALs.
#[derive(Debug, Deserialize)]
pub struct TransferReq {
    pub from_pial:       Uuid,
    pub to_pial:         Uuid,
    pub amount_uaet:     i64,
    pub event_type:      String,  // TRANSFER | TIP | SUBSCRIPTION | CREATOR_PAYOUT
    pub glyph_sig:       Option<String>,
    pub idempotency_key: Option<String>,
    pub metadata:        Option<serde_json::Value>,
}

/// Relay node reward — minted from platform reserves to a node operator.
#[derive(Debug, Deserialize)]
pub struct RelayRewardReq {
    pub node_pial:       Uuid,
    pub amount_uaet:     i64,
    pub idempotency_key: Option<String>,
    pub metadata:        Option<serde_json::Value>,
}

/// Burn AET (boost purchase, penalty, etc.)
#[derive(Debug, Deserialize)]
pub struct BurnReq {
    pub from_pial:       Uuid,
    pub amount_uaet:     i64,
    pub reason:          String,  // boost | penalty | governance_slash
    pub idempotency_key: Option<String>,
}

/// Admin mint (bootstrap, grants, ecosystem fund).
#[derive(Debug, Deserialize)]
pub struct MintReq {
    pub to_pial:         Uuid,
    pub amount_uaet:     i64,
    pub reason:          String,
    pub admin_id:        String,
}

// ── Response types ────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct BalanceResp {
    pub pial_id:        Uuid,
    pub balance_uaet:   i64,
    pub balance_aet:    f64,
    pub credit_balance: i64,
    pub total_earned:   i64,
    pub total_spent:    i64,
}

#[derive(Debug, Serialize)]
pub struct EventResp {
    pub event_id:    Uuid,
    pub event_type:  String,
    pub from_pial:   Option<Uuid>,
    pub to_pial:     Option<Uuid>,
    pub amount_uaet: i64,
    pub amount_aet:  f64,
    pub fee_uaet:    i64,
    pub status:      String,
    pub created_at:  DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct BlockResp {
    pub block_number:  i64,
    pub block_hash:    String,
    pub prev_hash:     String,
    pub events_root:   String,
    pub event_count:   i32,
    pub total_volume:  i64,
    pub fee_collected: i64,
    pub sealed_at:     DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct SupplyResp {
    pub total_supply_uaet: i64,
    pub total_supply_aet:  f64,
    pub circulating_uaet:  i64,
    pub circulating_aet:   f64,
    pub total_minted:      i64,
    pub total_burned:      i64,
}

#[derive(Debug, Serialize)]
pub struct FundResp {
    pub funding_id:    Uuid,
    pub credit_amount: i64,
    pub aet_minted:    i64,
    pub new_balance:   i64,
}

// ── DB row types ──────────────────────────────────────────────────────────────

#[derive(Debug, sqlx::FromRow)]
pub struct AccountRow {
    pub pial_id:        Uuid,
    pub balance_uaet:   i64,
    pub credit_balance: i64,
    pub total_earned:   i64,
    pub total_spent:    i64,
    pub total_minted:   i64,
    pub total_burned:   i64,
    pub sequence_no:    i64,
    pub created_at:     DateTime<Utc>,
    pub updated_at:     DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
pub struct EventRow {
    pub id:              Uuid,
    pub block_id:        Option<Uuid>,
    pub event_type:      String,
    pub from_pial:       Option<Uuid>,
    pub to_pial:         Option<Uuid>,
    pub amount_uaet:     i64,
    pub fee_uaet:        i64,
    pub seq_from:        Option<i64>,
    pub glyph_sig:       Option<String>,
    pub idempotency_key: Option<String>,
    pub status:          String,
    pub metadata:        serde_json::Value,
    pub created_at:      DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
pub struct BlockRow {
    pub id:           Uuid,
    pub block_number: i64,
    pub prev_hash:    String,
    pub block_hash:   String,
    pub events_root:  String,
    pub event_count:  i32,
    pub total_volume: i64,
    pub fee_collected: i64,
    pub sealed_at:    DateTime<Utc>,
}

/// Compute fee in micro-AET given basis points.
pub fn fee_for(amount: i64, basis_points: i64) -> i64 {
    (amount * basis_points) / 10_000
}

pub fn uaet_to_aet(uaet: i64) -> f64 {
    uaet as f64 / MICRO_AET as f64
}
