use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sqlx::FromRow;
use uuid::Uuid;

// ── Request bodies ─────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct EnableCreatorReq {
    pub pial_id: String,
}

#[derive(Deserialize)]
pub struct CreatePlanReq {
    pub creator_pial_id: String,
    pub name:            String,
    /// AET in whole units (e.g. 5.0 = 5 AET). Stored as micro-units (×1_000_000).
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
pub struct AccessQuery {
    pub subscriber: Option<String>,
    pub creator:    Option<String>,
    pub buyer:      Option<String>,
    pub content_id: Option<String>,
}

#[derive(Deserialize)]
pub struct CreatePpvReq {
    pub creator_pial_id: String,
    pub content_id:      String,
    /// AET in whole units.
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
    /// AET in whole units.
    pub amount_aet:        f64,
    #[serde(default)]
    pub message:           String,
    pub content_id:        Option<String>,
}

// ── Response types ─────────────────────────────────────────────────────────────

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

#[derive(Serialize)]
pub struct EarningsSummary {
    pub total_aet:         i64,
    pub subscriptions_aet: i64,
    pub ppv_aet:           i64,
    pub tips_aet:          i64,
    pub tx_count:          i64,
}

/// AET units per whole AET token. All prices stored in micro-units.
pub const AET_UNITS: f64 = 1_000_000.0;

pub fn aet_to_units(aet: f64) -> i64 {
    (aet * AET_UNITS) as i64
}

pub fn units_to_aet(units: i64) -> f64 {
    units as f64 / AET_UNITS
}
