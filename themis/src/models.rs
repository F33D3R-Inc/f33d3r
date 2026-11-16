use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sqlx::FromRow;
use uuid::Uuid;

use crate::pial_ref::PialRef;

// ── DB row types ───────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize)]
pub struct Shop {
    pub seller_pial: PialRef,
    pub btc_address: String,
    pub xrp_address: String,
    pub is_active: bool,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Listing {
    pub id: Uuid,
    pub shop_pial: PialRef,
    pub title: String,
    pub description: String,
    pub price_sats: i64,
    pub price_xrp_drops: i64,
    pub payment_currency: String, // "btc" | "xrp" | "both"
    pub content_url: String,
    pub content_hash: String,
    pub cek_encrypted: Vec<u8>, // ECIES-wrapped CEK: JSON {ephemeral_pub, iv, ciphertext} as bytes
    pub phash: Option<String>,
    pub visibility: String, // "public" | "subscribers_only"
    pub is_active: bool,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize)]
pub struct Purchase {
    pub id: Uuid,
    pub listing_id: Uuid,
    pub buyer_pial: PialRef,
    pub btc_address: String,
    pub expected_sats: i64,
    pub currency: String, // "btc" | "xrp"
    pub xrp_address: String,
    /// XRP: the destination tag. BTC: the sats added to make the amount unique.
    pub payment_tag: Option<i64>,
    /// The chain's height when this purchase was first watched (BTC).
    pub first_tip: Option<i64>,
    pub created_at: DateTime<Utc>,
    pub status: String, // "pending" | "confirmed" | "delivered" | "expired"
    pub cek_for_buyer: Option<Vec<u8>>,
    pub expires_at: DateTime<Utc>,
    pub confirmed_at: Option<DateTime<Utc>>,
    pub delivered_at: Option<DateTime<Utc>>,
    /// The listing as the buyer knows it. Carried only by the library read
    /// (`purchases_by_buyer` joins listings); every other purchase read leaves
    /// them absent, since a purchase row alone does not know its listing.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub listing_title: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub content_url: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub content_hash: Option<String>,
}

// ── Request bodies ─────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct ListingCreateReq {
    pub title: String,
    pub description: String,
    pub price_sats: i64,
    pub price_xrp_drops: Option<i64>,
    pub payment_currency: Option<String>, // "btc" | "xrp" | "both"
    pub content_url: String,
    pub content_hash: String,
    pub cek_encrypted: String, // base64
    pub phash: Option<String>,
    pub visibility: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct MarketplaceEvent {
    pub event_type: String,
    // shop.open
    pub btc_address: Option<String>,
    pub xrp_address: Option<String>,
    // listing.create
    pub title: Option<String>,
    pub description: Option<String>,
    pub price_sats: Option<i64>,
    pub price_xrp_drops: Option<i64>,
    pub payment_currency: Option<String>, // "btc" | "xrp" | "both"
    pub content_url: Option<String>,
    pub content_hash: Option<String>,
    pub cek_encrypted: Option<String>,
    pub phash: Option<String>,
    pub visibility: Option<String>,
    // listing.edit / listing.delete
    pub listing_id: Option<Uuid>,
    pub is_active: Option<bool>,
    // purchase.initiate
    // seller_pial for subscription
    pub seller_pial: Option<PialRef>,
    // PIAL signing — present on browser-originated high-value mutations
    pub cid: Option<String>,
    pub pial_sig: Option<String>,
}

// ── Commerce models ────────────────────────────────────────────────────────────
//
// Every amount is an integer of AET units — Ain Soph's unit, published at its
// GET /v1/unit — and is named `*_units` on the wire. The acting person is never
// in a body: it is the X-Pial-Identity feed-engine sets from the session.

#[derive(Deserialize)]
pub struct CreatePlanReq {
    /// The creator's account, for feed-engine's projection of the plan.
    pub creator_account: Uuid,
    pub name: String,
    pub price_units: i64,
    #[serde(default)]
    pub description: String,
}

#[derive(Deserialize)]
pub struct UpdatePlanReq {
    pub name: Option<String>,
    pub price_units: Option<i64>,
    pub description: Option<String>,
    pub is_active: Option<bool>,
}

#[derive(Deserialize)]
pub struct SubscribeReq {
    pub plan_id: Uuid,
    /// The subscribing account, for feed-engine's projection.
    pub subscriber_account: Uuid,
}

#[derive(Deserialize)]
pub struct UnsubscribeReq {
    pub creator_pial_id: PialRef,
}

#[derive(Deserialize)]
pub struct CommerceAccessQuery {
    pub subscriber: Option<PialRef>,
    pub creator: Option<PialRef>,
    pub buyer: Option<PialRef>,
    pub content_id: Option<String>,
}

#[derive(Deserialize)]
pub struct CreatePpvReq {
    pub content_id: String,
    pub price_units: i64,
    #[serde(default)]
    pub title: String,
}

#[derive(Deserialize)]
pub struct SendTipReq {
    pub recipient_pial_id: PialRef,
    pub amount_units: i64,
    /// The caller's key for this tip: a retry with the same key is the same tip.
    pub idempotency_key: String,
    #[serde(default)]
    pub message: String,
    pub content_id: Option<String>,
    /// Who the recipient's notification names, as persona accounts.
    #[serde(default)]
    pub from_account: Option<String>,
    #[serde(default)]
    pub to_account: Option<String>,
    #[serde(default)]
    pub from_handle: String,
    #[serde(default)]
    pub target_id: String,
    #[serde(default)]
    pub target_type: String,
}

#[derive(Serialize, FromRow)]
pub struct PlanRow {
    pub id: Uuid,
    pub creator_pial_id: PialRef,
    pub name: String,
    #[serde(rename = "price_units")]
    pub price_aet: i64,
    pub description: String,
    pub is_active: bool,
    pub created_at: DateTime<Utc>,
}

#[derive(Serialize, FromRow)]
pub struct SubscriptionRow {
    pub id: Uuid,
    pub subscriber_pial_id: PialRef,
    pub creator_pial_id: PialRef,
    pub plan_id: Option<Uuid>,
    pub status: String,
    pub current_period_start: DateTime<Utc>,
    pub current_period_end: DateTime<Utc>,
    pub cancelled_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
}

#[derive(Serialize, FromRow)]
pub struct PpvItemRow {
    pub id: Uuid,
    pub creator_pial_id: PialRef,
    pub content_id: String,
    #[serde(rename = "price_units")]
    pub price_aet: i64,
    pub title: String,
    pub is_active: bool,
    pub created_at: DateTime<Utc>,
}
