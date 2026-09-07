use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── AET token constants ───────────────────────────────────────────────────────
pub const TOKEN_NAME: &str = "ETHRA";
pub const TOKEN_TICKER: &str = "AET";
/// 1 AET = 100 micro-AET (storage unit). Display: balance / 100.
pub const AET_DECIMALS: i64 = 100;

// ── Fee schedule (basis points: 1 bps = 0.01%) ────────────────────────────────
pub const FEE_DEPOSIT_BPS: i64 = 250; // 2.50%
pub const FEE_TRANSFER_BPS: i64 = 300; // 3.00%
pub const FEE_TIP_BPS: i64 = 100; // 1.00% (lower fee for tips, encourages tipping)
pub const FEE_WITHDRAWAL_BPS: i64 = 200; // 2.00%

/// Compute fee in storage units (floor).
pub fn compute_fee(amount: i64, bps: i64) -> i64 {
    amount * bps / 10_000
}

/// Format a storage amount as AET string (e.g. "12.50 AET").
pub fn format_aet(units: i64) -> String {
    let whole = units / AET_DECIMALS;
    let frac = units.abs() % AET_DECIMALS;
    format!("{}.{:02} AET", whole, frac)
}

// ── Request types ─────────────────────────────────────────────────────────────

/// The identity a request acts as is NEVER read from these structs as the
/// source of truth. `auth::Caller::acting_as` derives it from the authenticated
/// context and uses the optional body field only to detect a caller trying to
/// act as somebody else. The fields stay accepted so existing internal callers
/// — which must name their subject, having no session — keep working, and so a
/// mismatch can be refused loudly instead of silently substituted.

#[derive(Debug, Deserialize)]
pub struct DepositRequest {
    /// Advisory only — see the note above. Required from a service caller.
    #[serde(default)]
    pub user_id: Option<String>,
    pub amount_cents: i64,
    pub idempotency_key: String,
}

#[derive(Debug, Deserialize)]
pub struct TransferRequest {
    /// The payer. Advisory only — see the note above.
    #[serde(default)]
    pub from_user_id: Option<String>,
    pub to_user_id: String,
    pub amount_cents: i64,
    pub idempotency_key: String,
    /// Optional metadata — "tip" or "subscription" signals AethyrRank feedback.
    #[serde(default)]
    pub tx_meta: String,
}

#[derive(Debug, Deserialize)]
pub struct TipRequest {
    /// The payer. Advisory only — see the note above.
    #[serde(default)]
    pub from_pial_id: Option<String>,
    pub to_pial_id: String,
    /// Amount in AET (whole units). Internally converted to storage units.
    pub amount_aet: f64,
    /// Optional message shown alongside the tip notification.
    #[serde(default)]
    pub message: String,
    /// Auto-generated if omitted.
    #[serde(default)]
    pub idempotency_key: String,
}

#[derive(Debug, Deserialize)]
pub struct WithdrawRequest {
    /// The account debited. Advisory only — see the note above.
    #[serde(default)]
    pub user_id: Option<String>,
    pub amount_cents: i64,
    pub idempotency_key: String,
}

#[derive(Debug, Deserialize)]
pub struct CreateProposalRequest {
    /// The author. Advisory only — see the note above.
    #[serde(default)]
    pub creator_id: Option<String>,
    pub title: String,
    #[serde(default)]
    pub description: String,
    /// "token" | "identity" | "equal"
    #[serde(default = "default_weight_model")]
    pub weight_model: String,
    /// Voting options (default: ["yes", "no"])
    #[serde(default = "default_options")]
    pub options: Vec<String>,
    /// ISO-8601 datetime when voting ends.
    pub ends_at: DateTime<Utc>,
}
fn default_weight_model() -> String {
    "identity".into()
}
fn default_options() -> Vec<String> {
    vec!["yes".into(), "no".into()]
}

#[derive(Debug, Deserialize)]
pub struct VoteRequest {
    /// The voter. Advisory only — see the note above.
    #[serde(default)]
    pub voter_id: Option<String>,
    pub option: String,
}

/// Checkpoint payload sent by Aethyr Ledger after sealing a block.
#[derive(Debug, Deserialize)]
pub struct LedgerCheckpointRequest {
    /// The ledger's block_number (incrementing integer).
    pub block_id: i64,
    /// SHA-256 Merkle root of all event IDs in this block (lowercase hex, 64 chars).
    pub events_root: String,
    /// Number of ledger events in this block.
    pub tx_count: i64,
    /// When the ledger sealed this block (ISO-8601 / RFC-3339).
    pub sealed_at: DateTime<Utc>,
}

#[derive(Debug, Deserialize)]
pub struct TxHistoryQuery {
    #[serde(default = "default_limit")]
    pub limit: i64,
    pub before: Option<DateTime<Utc>>,
}
fn default_limit() -> i64 {
    50
}

// ── Response types ────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct BalanceResponse {
    pub user_id: String,
    pub balance_units: i64,
    /// Human-readable AET amount (e.g. "12.50 AET").
    pub balance_aet: String,
    pub token: &'static str,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct TxResponse {
    pub transaction_id: Uuid,
    pub tx_type: String,
    pub amount_units: i64,
    pub amount_aet: String,
    pub fee_units: i64,
    pub net_units: i64,
    pub net_aet: String,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub idempotent_hit: bool,
}

#[derive(Debug, Serialize)]
pub struct TxHistoryEntry {
    pub id: Uuid,
    pub tx_type: String,
    pub direction: String, // "in" | "out" | "self"
    pub counterparty: Option<String>,
    pub amount_units: i64,
    pub amount_aet: String,
    pub net_units: i64,
    pub net_aet: String,
    pub status: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct TxHistoryResponse {
    pub user_id: String,
    pub entries: Vec<TxHistoryEntry>,
    pub count: usize,
}

#[derive(Debug, Serialize)]
pub struct ProposalSummary {
    pub id: Uuid,
    pub title: String,
    pub description: String,
    pub creator_id: String,
    pub weight_model: String,
    pub options: Vec<String>,
    pub ends_at: DateTime<Utc>,
    pub status: String,
    pub total_votes: i64,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct OptionResult {
    pub option: String,
    pub vote_count: i64,
    pub total_weight: i64,
    pub pct: f64,
}

#[derive(Debug, Serialize)]
pub struct ProposalDetailResponse {
    pub proposal: ProposalSummary,
    pub results: Vec<OptionResult>,
    pub total_weight: i64,
    pub leading: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct VoteResponse {
    pub proposal_id: Uuid,
    pub voter_id: String,
    pub option: String,
    pub weight: i64,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Serialize)]
pub struct SupplyResponse {
    pub token: &'static str,
    pub ticker: &'static str,
    pub total_balance_units: i64,
    pub total_balance_aet: String,
    pub fee_pool_units: i64,
    pub fee_pool_aet: String,
    pub account_count: i64,
    pub decimals: i64,
}

// ── DB row types ──────────────────────────────────────────────────────────────

#[derive(Debug, sqlx::FromRow)]
pub struct AccountRow {
    pub user_id: String,
    pub balance: i64,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
pub struct TxRow {
    pub id: Uuid,
    pub tx_type: String,
    pub from_user_id: Option<String>,
    pub to_user_id: Option<String>,
    pub amount: i64,
    pub fee: i64,
    pub net_amount: i64,
    pub status: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
pub struct TxRowSimple {
    pub id: Uuid,
    pub tx_type: String,
    pub amount: i64,
    pub fee: i64,
    pub net_amount: i64,
    pub status: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
pub struct ProposalRow {
    pub id: Uuid,
    pub title: String,
    pub description: String,
    pub creator_id: String,
    pub weight_model: String,
    pub options: Vec<String>,
    pub ends_at: DateTime<Utc>,
    pub status: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, sqlx::FromRow)]
pub struct VoteResultRow {
    pub option: String,
    pub vote_count: i64,
    pub total_weight: i64,
}

#[derive(Debug, sqlx::FromRow)]
pub struct VoteRow {
    pub id: Uuid,
    pub proposal_id: Uuid,
    pub voter_id: String,
    pub option: String,
    pub weight: i64,
    pub created_at: DateTime<Utc>,
}
