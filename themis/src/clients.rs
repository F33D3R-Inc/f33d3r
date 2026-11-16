//! Themis's calls to Ain Soph, its private AET book, and to Verity.
//!
//! Every call to Ain Soph carries `X-Internal-Key`: Ain Soph answers nothing
//! without it, and Themis acts as a peer settling for an identity that is not
//! making the call. Every amount is an integer of AET units — the unit Ain Soph
//! owns and publishes at `GET /v1/unit`.

use anyhow::{anyhow, Result};
use reqwest::{Client, StatusCode};
use serde::{Deserialize, Serialize};

/// What a value move came to.
#[derive(Debug)]
pub enum Charge {
    /// The ledger moved the value (now, or on an earlier call with the same
    /// idempotency key). Carries Ain Soph's transaction id.
    Done(String),
    /// The ledger answered and refused: nothing moved and nothing will on a
    /// retry of this request (insufficient balance, an identity it refuses).
    Declined(String),
    /// No answer that says what happened — a transport failure or a 5xx. The
    /// value may or may not have moved; only a retry with the same idempotency
    /// key can say, and the reconciler makes it.
    Unknown(String),
}

#[derive(Serialize)]
struct TransferReq<'a> {
    from_user_id: &'a str,
    to_user_id: &'a str,
    amount_units: i64,
    idempotency_key: &'a str,
    tx_meta: &'a str,
}

#[derive(Serialize)]
struct TipReq<'a> {
    from_pial_id: &'a str,
    to_pial_id: &'a str,
    amount_units: i64,
    idempotency_key: &'a str,
}

#[derive(Serialize)]
struct DepositReq<'a> {
    user_id: &'a str,
    amount_units: i64,
    idempotency_key: &'a str,
}

#[derive(Serialize)]
struct ReattributeReq<'a> {
    shadow_account: &'a str,
    owner_pial: &'a str,
    idempotency_key: &'a str,
}

#[derive(Deserialize)]
struct TxResponse {
    transaction_id: String,
    #[serde(default)]
    amount_units: i64,
}

#[derive(Deserialize)]
struct Refusal {
    #[serde(default)]
    error: String,
    #[serde(default)]
    message: String,
}

pub struct AinSoph<'a> {
    pub http: &'a Client,
    pub url: &'a str,
    pub key: &'a str,
}

impl AinSoph<'_> {
    async fn post<B: Serialize>(&self, path: &str, body: &B) -> (Charge, i64) {
        let resp = match self
            .http
            .post(format!("{}{path}", self.url))
            .header("X-Internal-Key", self.key)
            .json(body)
            .send()
            .await
        {
            Ok(r) => r,
            Err(e) => return (Charge::Unknown(format!("ain-soph unreachable: {e}")), 0),
        };
        let status = resp.status();
        if status.is_success() {
            return match resp.json::<TxResponse>().await {
                Ok(tx) => (Charge::Done(tx.transaction_id), tx.amount_units),
                // The ledger said yes; an unreadable answer leaves the outcome
                // known only to the ledger, so it is retried, not declined.
                Err(e) => (Charge::Unknown(format!("ain-soph answer unreadable: {e}")), 0),
            };
        }
        let body = resp.text().await.unwrap_or_default();
        let why = serde_json::from_str::<Refusal>(&body)
            .map(|r| if r.message.is_empty() { r.error } else { r.message })
            .unwrap_or(body);
        if status.is_server_error() || status == StatusCode::SERVICE_UNAVAILABLE || status == StatusCode::TOO_MANY_REQUESTS {
            (Charge::Unknown(format!("ain-soph {status}: {why}")), 0)
        } else {
            (Charge::Declined(why), 0)
        }
    }

    /// Moves `amount_units` from `from` to `to`.
    pub async fn transfer(&self, from: &str, to: &str, amount_units: i64, idem: &str, meta: &str) -> Charge {
        self.post(
            "/v1/transfer",
            &TransferReq { from_user_id: from, to_user_id: to, amount_units, idempotency_key: idem, tx_meta: meta },
        )
        .await
        .0
    }

    /// A tip: the ledger's lower tip fee.
    pub async fn tip(&self, from: &str, to: &str, amount_units: i64, idem: &str) -> Charge {
        self.post(
            "/v1/tip",
            &TipReq { from_pial_id: from, to_pial_id: to, amount_units, idempotency_key: idem },
        )
        .await
        .0
    }

    /// Mints platform credit. Service-only on Ain Soph's side.
    pub async fn deposit(&self, to: &str, amount_units: i64, idem: &str) -> Charge {
        self.post("/v1/deposit", &DepositReq { user_id: to, amount_units, idempotency_key: idem })
            .await
            .0
    }

    /// Moves a shadow account's whole balance to its owner, fee-free. Returns
    /// the outcome and the amount moved.
    pub async fn reattribute(&self, shadow: &str, owner: &str, idem: &str) -> (Charge, i64) {
        self.post(
            "/v1/accounts/reattribute",
            &ReattributeReq { shadow_account: shadow, owner_pial: owner, idempotency_key: idem },
        )
        .await
    }

    /// An account's balance in units, read as a service.
    pub async fn balance(&self, account: &str) -> Result<i64> {
        #[derive(Deserialize)]
        struct B {
            balance_units: i64,
        }
        let resp = self
            .http
            .get(format!("{}/v1/balance/{account}", self.url))
            .header("X-Internal-Key", self.key)
            .send()
            .await?;
        if !resp.status().is_success() {
            return Err(anyhow!("ain-soph balance {}: {}", resp.status(), resp.text().await.unwrap_or_default()));
        }
        Ok(resp.json::<B>().await?.balance_units)
    }

    /// The AET unit, as Ain Soph owns it.
    pub async fn units_per_aet(&self) -> Result<i64> {
        #[derive(Deserialize)]
        struct U {
            units_per_aet: i64,
        }
        let resp = self
            .http
            .get(format!("{}/v1/unit", self.url))
            .header("X-Internal-Key", self.key)
            .send()
            .await?
            .error_for_status()?;
        let u: U = resp.json().await?;
        if u.units_per_aet <= 0 {
            return Err(anyhow!("ain-soph published a non-positive unit"));
        }
        Ok(u.units_per_aet)
    }
}

/// Formats units as an AET amount ("12.50 AET"), with as many decimals as the
/// unit has.
pub fn format_aet(units: i64, units_per_aet: i64) -> String {
    let decimals = (units_per_aet as f64).log10().round() as usize;
    let sign = if units < 0 { "-" } else { "" };
    let whole = units.abs() / units_per_aet;
    let frac = units.abs() % units_per_aet;
    if decimals == 0 {
        format!("{sign}{whole} AET")
    } else {
        format!("{sign}{whole}.{frac:0decimals$} AET")
    }
}

#[derive(Deserialize)]
struct KycStatus {
    tier: i32,
}

pub async fn kyc_tier(client: &Client, verity_url: &str, pial_id: &str) -> i32 {
    let Ok(resp) = client
        .get(format!("{verity_url}/v1/kyc/{pial_id}"))
        .send()
        .await
    else {
        return 0;
    };
    if !resp.status().is_success() {
        return 0;
    }
    resp.json::<KycStatus>().await.map(|k| k.tier).unwrap_or(0)
}
