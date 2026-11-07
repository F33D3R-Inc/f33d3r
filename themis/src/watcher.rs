use crate::{
    cek, db,
    models::Purchase,
    pial_ref::{require_identity, IdentityError, PialRef},
    AppState,
};
use anyhow::{anyhow, Result};
use base64::{engine::general_purpose::STANDARD as B64, Engine};
use futures_util::{SinkExt, StreamExt};
use serde_json::Value;
use std::collections::HashMap;
use std::sync::Arc;
use tokio::time::{timeout, Duration};
use tokio_tungstenite::{connect_async, tungstenite::Message, WebSocketStream};
use tracing::{error, info, warn};
use uuid::Uuid;

/// How often the chains are read while a purchase is waiting.
const BTC_SWEEP: Duration = Duration::from_secs(30);
const XRP_SWEEP: Duration = Duration::from_secs(10);
const DELIVERY_RETRY_TICK: Duration = Duration::from_secs(60);
const NETWORK_TIMEOUT: Duration = Duration::from_secs(20);
/// Block timestamps may run behind real time by up to about two hours.
const BLOCK_TIME_TOLERANCE_SECS: i64 = 7_200;
/// A busy shop address is read back no further than this many pages per sweep.
const MAX_HISTORY_PAGES: usize = 8;
const ESPLORA_CONFIRMED_PAGE: usize = 25;

/// Reads Bitcoin for every purchase still waiting. The database holds what is
/// being waited for and the chain holds what was paid, so a restart or an outage
/// loses nothing: the next sweep finds the payment.
pub async fn run_btc_watcher(state: Arc<AppState>) {
    let mut tick = tokio::time::interval(BTC_SWEEP);
    loop {
        tick.tick().await;
        if let Err(e) = sweep_btc(&state).await {
            warn!("Bitcoin sweep failed, retrying next tick: {e}");
        }
    }
}

/// Reads the XRP Ledger for every purchase still waiting.
pub async fn run_xrp_watcher(state: Arc<AppState>) {
    let mut tick = tokio::time::interval(XRP_SWEEP);
    loop {
        tick.tick().await;
        if let Err(e) = sweep_xrp(&state).await {
            warn!("XRP sweep failed, retrying next tick: {e}");
        }
    }
}

async fn sweep_btc(state: &Arc<AppState>) -> Result<()> {
    db::purchases_lapse(&state.pool).await?;
    let pending = db::purchases_pending(&state.pool).await?;
    if pending.is_empty() {
        return Ok(());
    }
    let tip = btc_tip_height(state).await?;
    let mut by_address: HashMap<String, Vec<Purchase>> = HashMap::new();
    for p in pending {
        by_address.entry(p.btc_address.clone()).or_default().push(p);
    }
    for (address, purchases) in by_address {
        let oldest = purchases.iter().map(|p| p.created_at.timestamp()).min().unwrap_or(0);
        let txs = btc_address_history(state, &address, oldest).await?;
        for p in purchases {
            let threshold = if p.expected_sats < 50_000 {
                state.cfg.confirmation_threshold_low
            } else {
                state.cfg.confirmation_threshold_high
            };
            if let Some(txid) = btc_settlement(&txs, &address, p.expected_sats, p.created_at.timestamp(), tip, threshold) {
                settle(state, p, txid).await;
            }
        }
    }
    Ok(())
}

async fn btc_tip_height(state: &Arc<AppState>) -> Result<i64> {
    let url = format!("{}/blocks/tip/height", state.cfg.mempool_api_url);
    let body = state.http.get(&url).timeout(NETWORK_TIMEOUT).send().await?.error_for_status()?.text().await?;
    Ok(body.trim().parse()?)
}

/// The address's transactions back to the oldest purchase waiting on it.
async fn btc_address_history(state: &Arc<AppState>, address: &str, oldest: i64) -> Result<Vec<Value>> {
    let mut all = Vec::new();
    let mut url = format!("{}/address/{address}/txs", state.cfg.mempool_api_url);
    for _ in 0..MAX_HISTORY_PAGES {
        let page: Vec<Value> = state.http.get(&url).timeout(NETWORK_TIMEOUT).send().await?.error_for_status()?.json().await?;
        let confirmed: Vec<&Value> = page.iter().filter(|t| t["status"]["confirmed"].as_bool() == Some(true)).collect();
        let next = confirmed.last().and_then(|last| {
            let reached = last["status"]["block_time"].as_i64().unwrap_or(0) + BLOCK_TIME_TOLERANCE_SECS < oldest;
            let full = confirmed.len() >= ESPLORA_CONFIRMED_PAGE;
            (full && !reached).then(|| last["txid"].as_str().map(str::to_string)).flatten()
        });
        all.extend(page.iter().cloned());
        match next {
            Some(txid) => url = format!("{}/address/{address}/txs/chain/{txid}", state.cfg.mempool_api_url),
            None => break,
        }
    }
    Ok(all)
}

/// The transaction that pays this purchase: confirmed deeply enough, no older than
/// the purchase, and carrying exactly the amount reserved for it at this address.
fn btc_settlement(txs: &[Value], address: &str, expected_sats: i64, created_at: i64, tip: i64, threshold: u32) -> Option<String> {
    txs.iter().find_map(|tx| {
        let status = &tx["status"];
        if status["confirmed"].as_bool() != Some(true) {
            return None;
        }
        let height = status["block_height"].as_i64()?;
        if tip - height + 1 < i64::from(threshold) {
            return None;
        }
        if status["block_time"].as_i64()? + BLOCK_TIME_TOLERANCE_SECS < created_at {
            return None;
        }
        if sum_vout_to_address(tx, address) != expected_sats {
            return None;
        }
        tx["txid"].as_str().map(str::to_string)
    })
}

fn sum_vout_to_address(tx: &Value, address: &str) -> i64 {
    tx.get("vout")
        .and_then(|v| v.as_array())
        .map(|outputs| {
            outputs
                .iter()
                .filter(|o| o.get("scriptpubkey_address").and_then(|a| a.as_str()) == Some(address))
                .map(|o| o.get("value").and_then(|v| v.as_i64()).unwrap_or(0))
                .sum()
        })
        .unwrap_or(0)
}

async fn sweep_xrp(state: &Arc<AppState>) -> Result<()> {
    db::purchases_lapse(&state.pool).await?;
    let pending = db::purchases_pending_xrp(&state.pool).await?;
    if pending.is_empty() {
        return Ok(());
    }
    let mut by_address: HashMap<String, Vec<Purchase>> = HashMap::new();
    for p in pending {
        by_address.entry(p.xrp_address.clone()).or_default().push(p);
    }
    let (mut ws, _) = timeout(NETWORK_TIMEOUT, connect_async(&state.cfg.xrp_ws_url)).await??;
    for (address, purchases) in by_address {
        let txs = xrp_account_history(&mut ws, &address).await?;
        for p in purchases {
            let Some(tag) = p.payment_tag else { continue };
            if let Some(hash) = xrp_settlement(&txs, &address, tag, p.expected_sats) {
                settle(state, p, hash).await;
            }
        }
    }
    let _ = ws.close(None).await;
    Ok(())
}

async fn xrp_account_history<S>(ws: &mut WebSocketStream<S>, address: &str) -> Result<Vec<Value>>
where
    S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin,
{
    let mut all = Vec::new();
    let mut marker = Value::Null;
    for page in 0..MAX_HISTORY_PAGES {
        let mut req = serde_json::json!({
            "id": page,
            "command": "account_tx",
            "account": address,
            "api_version": 1,
            "ledger_index_min": -1,
            "ledger_index_max": -1,
            "limit": 200,
            "forward": false,
        });
        if !marker.is_null() {
            req["marker"] = marker.clone();
        }
        ws.send(Message::Text(req.to_string())).await?;
        let reply = loop {
            let msg = timeout(NETWORK_TIMEOUT, ws.next()).await?.ok_or_else(|| anyhow!("XRPL connection closed"))??;
            if let Message::Text(text) = msg {
                let v: Value = serde_json::from_str(&text)?;
                if v["id"].as_u64() == Some(page as u64) {
                    break v;
                }
            }
        };
        if reply["status"].as_str() != Some("success") {
            return Err(anyhow!("account_tx refused: {}", reply["error"]));
        }
        if let Some(txs) = reply["result"]["transactions"].as_array() {
            all.extend(txs.iter().cloned());
        }
        marker = reply["result"]["marker"].clone();
        if marker.is_null() {
            break;
        }
    }
    Ok(all)
}

/// The validated payment that settles this purchase. The amount is what the ledger
/// says was delivered, never the Amount field, which a partial payment can inflate
/// to any figure while delivering almost nothing.
fn xrp_settlement(txs: &[Value], address: &str, tag: i64, expected_drops: i64) -> Option<String> {
    txs.iter().find_map(|item| {
        let (tx, meta) = (&item["tx"], &item["meta"]);
        if item["validated"].as_bool() != Some(true)
            || tx["TransactionType"].as_str() != Some("Payment")
            || tx["Destination"].as_str() != Some(address)
            || tx["DestinationTag"].as_i64() != Some(tag)
            || meta["TransactionResult"].as_str() != Some("tesSUCCESS")
        {
            return None;
        }
        // Native XRP is a string of drops; issued currencies are objects and do not count.
        let delivered: i64 = meta["delivered_amount"].as_str()?.parse().ok()?;
        if delivered < expected_drops {
            return None;
        }
        tx["hash"].as_str().map(str::to_string)
    })
}

/// Records the payment, then delivers. Delivery runs only for the attempt that
/// recorded it; a transaction already used for another purchase settles nothing.
async fn settle(state: &Arc<AppState>, purchase: Purchase, tx: String) {
    match db::purchase_settle(&state.pool, purchase.id, &tx).await {
        Ok(true) => {
            info!("Purchase {} settled by {tx}", purchase.id);
            deliver_purchase(state, purchase.id, purchase.buyer_pial, purchase.listing_id).await;
        }
        Ok(false) => warn!("Transaction {tx} did not settle purchase {}: already settled or already used", purchase.id),
        Err(e) => error!("Failed to record settlement of purchase {}: {e}", purchase.id),
    }
}

/// Retries paid purchases that are not yet delivered. A buyer with no key yet, or an
/// outage at the key authority, delays delivery instead of stranding the buyer.
pub async fn run_delivery_retry(state: Arc<AppState>) {
    let mut tick = tokio::time::interval(DELIVERY_RETRY_TICK);
    loop {
        tick.tick().await;
        match db::purchases_undelivered(&state.pool).await {
            Ok(purchases) => {
                for p in purchases {
                    deliver_purchase(&state, p.id, p.buyer_pial, p.listing_id).await;
                }
            }
            Err(e) => error!("Failed to load undelivered purchases: {e}"),
        }
    }
}

/// Wraps the content key for the buyer and marks the purchase delivered. A purchase
/// left 'confirmed' by any failure here is picked up by [`run_delivery_retry`].
async fn deliver_purchase(state: &Arc<AppState>, purchase_id: Uuid, buyer_pial: PialRef, listing_id: Uuid) {
    let listing = match db::listing_get(&state.pool, listing_id).await {
        Ok(Some(l)) => l,
        Ok(None) => {
            error!("Listing not found for purchase {purchase_id}");
            return;
        }
        Err(e) => {
            error!("DB error fetching listing: {e}");
            return;
        }
    };

    // The content key is about to be wrapped for whoever this PIAL resolves to,
    // so the identity is checked one last time before it is used. The buyer was
    // already resolved when the purchase was created; what this catches is an
    // identity that stopped being active in between.
    //
    // A revoked identity halts delivery: the purchase stays 'confirmed', which
    // is visible and recoverable, rather than having a content key unwrapped
    // for an identity the naming plane no longer vouches for. A plane that is
    // merely unreachable does not halt anything — the payment has already
    // settled on chain, and stranding a paid buyer over a transient outage
    // would be the worse failure.
    match require_identity(&state.manhattan, &buyer_pial).await {
        Ok(()) => {}
        Err(e @ (IdentityError::NotActive | IdentityError::NotAnIdentity(_))) => {
            error!("INCIDENT: CEK delivery halted for purchase {purchase_id}: {e}");
            return;
        }
        Err(e) => warn!(
            "CEK delivery for purchase {purchase_id} proceeding on the identity resolved at purchase time: {e}"
        ),
    }

    let buyer_pubkey_b64 = match fetch_buyer_ecdh_pubkey(state, &buyer_pial.text()).await {
        Ok(k) => k,
        Err(e) => {
            warn!("Purchase {purchase_id} waiting on the buyer's ECDH key: {e}");
            return;
        }
    };
    let buyer_pub = match cek::pubkey_from_b64(&buyer_pubkey_b64) {
        Ok(k) => k,
        Err(e) => {
            error!("Invalid buyer pubkey: {e}");
            return;
        }
    };

    // Unwrap CEK with Themis private key, re-wrap for buyer
    let plaintext_cek = match cek::unwrap_cek(&state.themis_privkey, &listing.cek_encrypted) {
        Ok(k) => k,
        Err(e) => {
            error!("CEK unwrap failed: {e}");
            return;
        }
    };
    let cek_for_buyer = match cek::wrap_cek_for(&buyer_pub, &plaintext_cek) {
        Ok(k) => k,
        Err(e) => {
            error!("CEK rewrap failed: {e}");
            return;
        }
    };
    // plaintext_cek drops here

    let notices = delivery_notices(&purchase_id.to_string(), buyer_pial.text(), listing.shop_pial.text());
    match db::purchase_mark_delivered(&state.pool, purchase_id, &cek_for_buyer, &notices).await {
        Ok(true) => {}
        Ok(false) => return,
        Err(e) => {
            error!("Failed to mark purchase delivered: {e}");
            return;
        }
    }

    // Refreshes the buyer's open library. The notification above is the durable record.
    if let Err(e) = notify_nantar_delivered(
        state,
        purchase_id,
        &buyer_pial.text(),
        &listing.content_url,
        &listing.content_hash,
        &cek_for_buyer,
    )
    .await
    {
        warn!("Failed to notify Nantar of delivery (purchase still marked delivered): {e}");
    }
}

async fn fetch_buyer_ecdh_pubkey(state: &Arc<AppState>, pial_id: &str) -> Result<String> {
    let url = format!(
        "{}/api/internal/pial/{}/ecdh-pubkey",
        state.cfg.nantar_url, pial_id
    );
    let resp = state
        .http
        .get(&url)
        .header("X-Internal-Key", &state.cfg.internal_api_key)
        .send()
        .await?
        .error_for_status()?
        .json::<serde_json::Value>()
        .await?;
    resp.get("public_key_b64")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .ok_or_else(|| anyhow::anyhow!("no public_key_b64 in response"))
}

async fn notify_nantar_delivered(
    state: &Arc<AppState>,
    purchase_id: Uuid,
    buyer_pial: &str,
    content_url: &str,
    content_hash: &str,
    cek_for_buyer: &[u8],
) -> Result<()> {
    let url = format!(
        "{}/api/internal/themis/purchase-delivered",
        state.cfg.nantar_url
    );
    state
        .http
        .post(&url)
        .header("X-Internal-Key", &state.cfg.internal_api_key)
        .json(&serde_json::json!({
            "buyer_pial":    buyer_pial,
            "purchase_id":   purchase_id.to_string(),
            "content_url":   content_url,
            "content_hash":  content_hash,
            "cek_for_buyer": B64.encode(cek_for_buyer),
        }))
        .send()
        .await?
        .error_for_status()?;
    Ok(())
}

/// What the buyer and seller are told when a purchase is delivered.
fn delivery_notices(purchase_id: &str, buyer: String, seller: String) -> Vec<crate::notifications::Event> {
    [
        (buyer, "purchase_delivered", "Your purchase is ready", "/buyer-library"),
        (seller, "sale", "You made a sale", "/wallet"),
    ]
    .into_iter()
    .map(|(who, kind, title, url)| crate::notifications::Event {
        actor_account: None,
        recipient_account: None,
        event_id: format!("themis:{kind}:{purchase_id}"),
        recipient_pial: who,
        kind: kind.into(),
        target_id: purchase_id.to_string(),
        target_type: "commerce".into(),
        payload: serde_json::json!({}),
        push_title: title.into(),
        push_body: String::new(),
        push_url: url.into(),
    })
    .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    const ADDR: &str = "bc1qshop";
    const XRP: &str = "rShop";

    fn btc_tx(txid: &str, sats: i64, height: i64, time: i64) -> Value {
        json!({
            "txid": txid,
            "status": { "confirmed": true, "block_height": height, "block_time": time },
            "vout": [{ "scriptpubkey_address": ADDR, "value": sats }, { "scriptpubkey_address": "other", "value": 999 }],
        })
    }

    fn xrp_tx(hash: &str, tag: i64, amount: &str, delivered: &str, result: &str) -> Value {
        json!({
            "validated": true,
            "meta": { "TransactionResult": result, "delivered_amount": delivered },
            "tx": { "TransactionType": "Payment", "Destination": XRP, "DestinationTag": tag, "Amount": amount, "hash": hash },
        })
    }

    #[test]
    fn btc_needs_the_exact_reserved_amount() {
        let txs = [btc_tx("near", 10_001, 100, 1_000), btc_tx("paid", 10_042, 100, 1_000)];
        assert_eq!(btc_settlement(&txs, ADDR, 10_042, 1_000, 100, 1).as_deref(), Some("paid"));
        assert_eq!(btc_settlement(&txs, ADDR, 10_043, 1_000, 100, 1), None);
    }

    #[test]
    fn btc_counts_real_confirmations() {
        let txs = [btc_tx("paid", 10_042, 100, 1_000)];
        assert_eq!(btc_settlement(&txs, ADDR, 10_042, 1_000, 101, 3), None);
        assert_eq!(btc_settlement(&txs, ADDR, 10_042, 1_000, 102, 3).as_deref(), Some("paid"));
    }

    #[test]
    fn btc_ignores_unconfirmed_and_older_transactions() {
        let unconfirmed = [json!({ "txid": "m", "status": { "confirmed": false }, "vout": [{ "scriptpubkey_address": ADDR, "value": 10_042 }] })];
        assert_eq!(btc_settlement(&unconfirmed, ADDR, 10_042, 1_000, 100, 0), None);
        let old = [btc_tx("old", 10_042, 100, 1_000)];
        assert_eq!(btc_settlement(&old, ADDR, 10_042, 1_000 + BLOCK_TIME_TOLERANCE_SECS + 1, 100, 1), None);
    }

    #[test]
    fn xrp_partial_payment_settles_nothing() {
        let txs = [xrp_tx("partial", 7, "1000000", "1", "tesSUCCESS")];
        assert_eq!(xrp_settlement(&txs, XRP, 7, 1_000_000), None);
    }

    #[test]
    fn xrp_binds_to_the_tag() {
        let txs = [xrp_tx("theirs", 8, "1000000", "1000000", "tesSUCCESS"), xrp_tx("mine", 7, "1000000", "1000000", "tesSUCCESS")];
        assert_eq!(xrp_settlement(&txs, XRP, 7, 1_000_000).as_deref(), Some("mine"));
    }

    #[test]
    fn xrp_ignores_failed_and_issued_currency_payments() {
        let failed = [xrp_tx("failed", 7, "1000000", "1000000", "tecUNFUNDED_PAYMENT")];
        assert_eq!(xrp_settlement(&failed, XRP, 7, 1_000_000), None);
        let mut issued = xrp_tx("iou", 7, "1000000", "1000000", "tesSUCCESS");
        issued["meta"]["delivered_amount"] = json!({ "currency": "USD", "issuer": "rX", "value": "9999999" });
        assert_eq!(xrp_settlement(&[issued], XRP, 7, 1_000_000), None);
    }
}
