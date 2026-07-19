use anyhow::{anyhow, Result};
use reqwest::Client;
use serde::{Deserialize, Serialize};
use uuid::Uuid;

#[derive(Serialize)]
struct TransferReq {
    from_user_id:    String,
    to_user_id:      String,
    amount_cents:    i64,
    idempotency_key: String,
    tx_meta:         String,
}

#[derive(Serialize)]
struct TipReq {
    from_pial_id:    String,
    to_pial_id:      String,
    amount_aet:      f64,
    message:         String,
    idempotency_key: String,
}

#[derive(Deserialize)]
struct TxResponse {
    transaction_id: Option<String>,
    tx_id:          Option<String>,
    id:             Option<String>,
}

pub async fn transfer(
    client: &Client,
    ain_soph_url: &str,
    from_pial: &str,
    to_pial: &str,
    amount_units: i64,
    meta: &str,
) -> Result<String> {
    let idempotency_key = Uuid::new_v4().to_string();
    let resp = client
        .post(format!("{ain_soph_url}/v1/transfer"))
        .json(&TransferReq {
            from_user_id:    from_pial.into(),
            to_user_id:      to_pial.into(),
            amount_cents:    amount_units,
            idempotency_key,
            tx_meta:         meta.into(),
        })
        .send()
        .await?;
    if !resp.status().is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(anyhow!("ain_soph transfer failed: {body}"));
    }
    let tx: TxResponse = resp.json().await?;
    Ok(tx.transaction_id.or(tx.tx_id).or(tx.id).unwrap_or_default())
}

pub async fn tip(
    client: &Client,
    ain_soph_url: &str,
    from_pial: &str,
    to_pial: &str,
    amount_aet: f64,
    message: &str,
) -> Result<String> {
    let idempotency_key = Uuid::new_v4().to_string();
    let resp = client
        .post(format!("{ain_soph_url}/v1/tip"))
        .json(&TipReq {
            from_pial_id:    from_pial.into(),
            to_pial_id:      to_pial.into(),
            amount_aet,
            message:         message.into(),
            idempotency_key,
        })
        .send()
        .await?;
    if !resp.status().is_success() {
        let body = resp.text().await.unwrap_or_default();
        return Err(anyhow!("ain_soph tip failed: {body}"));
    }
    let tx: TxResponse = resp.json().await?;
    Ok(tx.transaction_id.or(tx.tx_id).or(tx.id).unwrap_or_default())
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
