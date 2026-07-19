use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sqlx::FromRow;
use uuid::Uuid;

// ── DB row types ───────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize)]
pub struct Shop {
    pub seller_pial: Uuid,
    pub btc_address: String,
    pub xrp_address: String,
    pub is_active:   bool,
    pub created_at:  DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Listing {
    pub id:               Uuid,
    pub shop_pial:        Uuid,
    pub title:            String,
    pub description:      String,
    pub price_sats:       i64,
    pub price_xrp_drops:  i64,
    pub payment_currency: String,   // "btc" | "xrp" | "both"
    pub content_url:      String,
    pub content_hash:     String,
    pub cek_encrypted:    Vec<u8>,  // ECIES-wrapped CEK: JSON {ephemeral_pub, iv, ciphertext} as bytes
    pub phash:            Option<String>,
    pub visibility:       String,   // "public" | "subscribers_only"
    pub is_active:        bool,
    pub created_at:       DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Purchase {
    pub id:             Uuid,
    pub listing_id:     Uuid,
    pub buyer_pial:     Uuid,
    pub btc_address:    String,
    pub expected_sats:  i64,
    pub currency:       String,    // "btc" | "xrp"
    pub xrp_address:    String,
    pub status:         String,    // "pending" | "confirmed" | "delivered" | "expired"
    pub cek_for_buyer:  Option<Vec<u8>>,
    pub expires_at:     DateTime<Utc>,
    pub confirmed_at:   Option<DateTime<Utc>>,
    pub delivered_at:   Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize)]
pub struct MarketplaceSubscription {
    pub id:          Uuid,
    pub buyer_pial:  Uuid,
    pub seller_pial: Uuid,
    pub status:      String,
    pub period_end:  DateTime<Utc>,
    pub created_at:  DateTime<Utc>,
}

// ── Request bodies ─────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct ShopOpenReq {
    pub btc_address: String,
}

#[derive(Debug, Deserialize)]
pub struct ListingCreateReq {
    pub title:            String,
    pub description:      String,
    pub price_sats:       i64,
    pub price_xrp_drops:  Option<i64>,
    pub payment_currency: Option<String>,  // "btc" | "xrp" | "both"
    pub content_url:      String,
    pub content_hash:     String,
    pub cek_encrypted:    String,  // base64
    pub phash:            Option<String>,
    pub visibility:       Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct ListingEditReq {
    pub listing_id:  Uuid,
    pub title:       Option<String>,
    pub description: Option<String>,
    pub price_sats:  Option<i64>,
    pub is_active:   Option<bool>,
}

#[derive(Debug, Deserialize)]
pub struct PurchaseInitiateReq {
    pub listing_id: Uuid,
}

#[derive(Debug, Deserialize)]
pub struct SubscriptionCreateReq {
    pub seller_pial: Uuid,
}

#[derive(Debug, Deserialize)]
pub struct MarketplaceEvent {
    pub event_type: String,
    // shop.open
    pub btc_address:      Option<String>,
    pub xrp_address:      Option<String>,
    // listing.create
    pub title:            Option<String>,
    pub description:      Option<String>,
    pub price_sats:       Option<i64>,
    pub price_xrp_drops:  Option<i64>,
    pub payment_currency: Option<String>,  // "btc" | "xrp" | "both"
    pub content_url:      Option<String>,
    pub content_hash:     Option<String>,
    pub cek_encrypted:    Option<String>,
    pub phash:            Option<String>,
    pub visibility:       Option<String>,
    // listing.edit / listing.delete
    pub listing_id:       Option<Uuid>,
    pub is_active:        Option<bool>,
    // purchase.initiate
    // seller_pial for subscription
    pub seller_pial:      Option<Uuid>,
    // PIAL signing — present on browser-originated high-value mutations
    pub cid:              Option<String>,
    pub pial_sig:         Option<String>,
}

// ── Response types ─────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct PurchaseInitiatedResp {
    pub purchase_id:  Uuid,
    pub btc_address:  String,
    pub amount_sats:  i64,
    pub expires_at:   DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct AccessCheckResp {
    pub has_access: bool,
}

// ── Commerce (Thessalon) models ────────────────────────────────────────────────

pub const AET_UNITS: f64 = 1_000_000.0;
pub fn aet_to_units(aet: f64) -> i64 { (aet * AET_UNITS) as i64 }
pub fn units_to_aet(units: i64) -> f64 { units as f64 / AET_UNITS }

#[derive(Deserialize)]
pub struct EnableCreatorReq {
    pub pial_id: String,
}

#[derive(Deserialize)]
pub struct CreatePlanReq {
    pub creator_pial_id: String,
    pub name:            String,
    pub price_aet:       f64,
    #[serde(default)]
    pub description:     String,
}

#[derive(Deserialize)]
pub struct UpdatePlanReq {
    pub name:        Option<String>,
    pub price_aet:   Option<f64>,
    pub description: Option<String>,
    pub is_active:   Option<bool>,
}

#[derive(Deserialize)]
pub struct SubscribeReq {
    pub subscriber_pial_id: String,
    pub creator_pial_id:    String,
    pub plan_id:            Uuid,
}

#[derive(Deserialize)]
pub struct UnsubscribeReq {
    pub subscriber_pial_id: String,
    pub creator_pial_id:    String,
}

#[derive(Deserialize)]
pub struct CommerceAccessQuery {
    pub subscriber: Option<String>,
    pub creator:    Option<String>,
    pub buyer:      Option<String>,
    pub content_id: Option<String>,
}

#[derive(Deserialize)]
pub struct CreatePpvReq {
    pub creator_pial_id: String,
    pub content_id:      String,
    pub price_aet:       f64,
    #[serde(default)]
    pub title:           String,
}

#[derive(Deserialize)]
pub struct PurchasePpvReq {
    pub buyer_pial_id: String,
}

#[derive(Deserialize)]
pub struct SendTipReq {
    pub sender_pial_id:    String,
    pub recipient_pial_id: String,
    pub amount_aet:        f64,
    #[serde(default)]
    pub message:           String,
    pub content_id:        Option<String>,
}

#[derive(Serialize, FromRow)]
pub struct PlanRow {
    pub id:              Uuid,
    pub creator_pial_id: String,
    pub name:            String,
    pub price_aet:       i64,
    pub description:     String,
    pub is_active:       bool,
    pub created_at:      DateTime<Utc>,
}

#[derive(Serialize, FromRow)]
pub struct SubscriptionRow {
    pub id:                   Uuid,
    pub subscriber_pial_id:   String,
    pub creator_pial_id:      String,
    pub plan_id:              Option<Uuid>,
    pub status:               String,
    pub current_period_start: DateTime<Utc>,
    pub current_period_end:   DateTime<Utc>,
    pub cancelled_at:         Option<DateTime<Utc>>,
    pub created_at:           DateTime<Utc>,
}

#[derive(Serialize, FromRow)]
pub struct PpvItemRow {
    pub id:              Uuid,
    pub creator_pial_id: String,
    pub content_id:      String,
    pub price_aet:       i64,
    pub title:           String,
    pub is_active:       bool,
    pub created_at:      DateTime<Utc>,
}
