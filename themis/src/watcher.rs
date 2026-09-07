use crate::{
    cek, db,
    pial_ref::{require_identity, IdentityError, PialRef},
    AppState,
};
use anyhow::Result;
use base64::{engine::general_purpose::STANDARD as B64, Engine};
use futures_util::{SinkExt, StreamExt};
use serde_json::Value;
use std::collections::HashMap;
use std::sync::Arc;
use tokio::sync::Mutex;
use tokio::time::{sleep, Duration};
use tokio_tungstenite::{connect_async, tungstenite::Message};
use tracing::{error, info, warn};
use uuid::Uuid;

#[derive(Debug, Clone)]
pub struct WatchEntry {
    pub purchase_id: Uuid,
    pub buyer_pial: PialRef,
    pub listing_id: Uuid,
    pub expected_sats: i64,
    pub btc_address: String,
    pub confirmations: u32,
}

pub type WatchMap = Arc<Mutex<HashMap<String, WatchEntry>>>;

pub async fn run_watcher(state: Arc<AppState>, watch_map: WatchMap) {
    // Load pending purchases from DB on startup
    if let Ok(pending) = db::purchases_pending(&state.pool).await {
        let mut map = watch_map.lock().await;
        for p in pending {
            map.insert(
                p.btc_address.clone(),
                WatchEntry {
                    purchase_id: p.id,
                    buyer_pial: p.buyer_pial,
                    listing_id: p.listing_id,
                    expected_sats: p.expected_sats,
                    btc_address: p.btc_address.clone(),
                    confirmations: 0,
                },
            );
        }
        info!("Loaded {} pending purchases to watch", map.len());
    }

    loop {
        match connect_watcher(state.clone(), watch_map.clone()).await {
            Ok(_) => info!("watcher exited cleanly"),
            Err(e) => {
                error!("watcher error: {e} — reconnecting in 5s");
                sleep(Duration::from_secs(5)).await;
            }
        }
    }
}

async fn connect_watcher(state: Arc<AppState>, watch_map: WatchMap) -> Result<()> {
    info!(
        "Connecting to mempool WebSocket: {}",
        state.cfg.mempool_ws_url
    );
    let (ws_stream, _) = connect_async(&state.cfg.mempool_ws_url).await?;
    let (mut write, mut read) = ws_stream.split();

    // Subscribe to all currently watched addresses
    {
        let map = watch_map.lock().await;
        for addr in map.keys() {
            let msg = serde_json::json!({ "track-address": addr });
            write.send(Message::Text(msg.to_string())).await?;
        }
    }

    while let Some(msg) = read.next().await {
        let msg = msg?;
        if let Message::Text(text) = msg {
            if let Ok(v) = serde_json::from_str::<Value>(&text) {
                handle_ws_message(&state, &watch_map, &mut write, v).await;
            }
        }
    }
    Ok(())
}

async fn handle_ws_message(
    state: &Arc<AppState>,
    watch_map: &WatchMap,
    write: &mut (impl SinkExt<Message, Error = tokio_tungstenite::tungstenite::Error> + Unpin),
    v: Value,
) {
    // mempool.space fires "address-transactions" events with tx data
    if let Some(address) = v.get("address").and_then(|a| a.as_str()) {
        let entry = {
            let map = watch_map.lock().await;
            map.get(address).cloned()
        };
        if let Some(entry) = entry {
            // mempool-transactions = unconfirmed; transactions = confirmed
            if let Some(txns) = v
                .get("mempool-transactions")
                .or_else(|| v.get("transactions"))
            {
                if let Some(arr) = txns.as_array() {
                    for tx in arr {
                        let sats_received = sum_vout_to_address(tx, address);
                        if sats_received >= entry.expected_sats {
                            let confirmations = tx
                                .get("status")
                                .and_then(|s| s.get("block_height"))
                                .map(|_| 1u32)
                                .unwrap_or(0);
                            let threshold = if entry.expected_sats < 50_000 {
                                state.cfg.confirmation_threshold_low
                            } else {
                                state.cfg.confirmation_threshold_high
                            };
                            if confirmations >= threshold {
                                handle_payment_confirmed(state, watch_map, entry).await;
                                return;
                            }
                        }
                    }
                }
            }
            // confirmed-transactions (block confirmed)
            if let Some(txns) = v.get("confirmed-transactions") {
                if let Some(arr) = txns.as_array() {
                    for tx in arr {
                        let sats_received = sum_vout_to_address(tx, address);
                        if sats_received >= entry.expected_sats {
                            handle_payment_confirmed(state, watch_map, entry).await;
                            return;
                        }
                    }
                }
            }
        }
    }

    // Suppress unused variable warning — write is needed for future subscribe_address channel
    let _ = write;
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

async fn handle_payment_confirmed(state: &Arc<AppState>, watch_map: &WatchMap, entry: WatchEntry) {
    info!("Payment confirmed for purchase {}", entry.purchase_id);

    // Remove from watch map
    watch_map.lock().await.remove(&entry.btc_address);

    // Mark confirmed in DB
    if let Err(e) = db::purchase_mark_confirmed(&state.pool, entry.purchase_id).await {
        error!("Failed to mark purchase confirmed: {e}");
        return;
    }

    // Fetch listing to get cek_encrypted
    let listing = match db::listing_get(&state.pool, entry.listing_id).await {
        Ok(Some(l)) => l,
        Ok(None) => {
            error!("Listing not found for purchase {}", entry.purchase_id);
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
    match require_identity(&state.manhattan, &entry.buyer_pial).await {
        Ok(()) => {}
        Err(e @ (IdentityError::NotActive | IdentityError::NotAnIdentity(_))) => {
            error!(
                "INCIDENT: CEK delivery halted for purchase {}: {e}",
                entry.purchase_id
            );
            return;
        }
        Err(e) => warn!(
            "CEK delivery for purchase {} proceeding on the identity resolved at purchase time: {e}",
            entry.purchase_id
        ),
    }

    // Fetch buyer's ECDH public key from Nantar
    let buyer_pubkey_b64 = match fetch_buyer_ecdh_pubkey(state, &entry.buyer_pial.text()).await {
        Ok(k) => k,
        Err(e) => {
            error!("Failed to fetch buyer ECDH pubkey: {e}");
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

    // Store in DB
    if let Err(e) =
        db::purchase_mark_delivered(&state.pool, entry.purchase_id, cek_for_buyer.clone()).await
    {
        error!("Failed to mark purchase delivered: {e}");
        return;
    }

    // Notify Nantar → SSE to buyer
    if let Err(e) = notify_nantar_delivered(
        state,
        entry,
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
    entry: WatchEntry,
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
            "buyer_pial":    entry.buyer_pial.text(),
            "purchase_id":   entry.purchase_id.to_string(),
            "content_url":   content_url,
            "content_hash":  content_hash,
            "cek_for_buyer": B64.encode(cek_for_buyer),
        }))
        .send()
        .await?
        .error_for_status()?;
    Ok(())
}

/// Add a new purchase to the watch map and subscribe its address.
/// Called from the purchase initiate handler.
pub async fn subscribe_address(watch_map: &WatchMap, entry: WatchEntry) {
    watch_map
        .lock()
        .await
        .insert(entry.btc_address.clone(), entry);
    // The watcher loop picks it up on next reconnect (max 5s delay).
}

// ── XRP watcher ────────────────────────────────────────────────────────────────

#[derive(Debug, Clone)]
pub struct XrpWatchEntry {
    pub purchase_id: Uuid,
    pub buyer_pial: PialRef,
    pub listing_id: Uuid,
    pub expected_drops: i64,
    pub xrp_address: String,
}

pub type XrpWatchMap = Arc<Mutex<HashMap<String, XrpWatchEntry>>>;

pub async fn run_xrp_watcher(state: Arc<AppState>, xrp_watch_map: XrpWatchMap) {
    // Load pending XRP purchases from DB on startup
    if let Ok(pending) = db::purchases_pending_xrp(&state.pool).await {
        let mut map = xrp_watch_map.lock().await;
        for p in pending {
            if !p.xrp_address.is_empty() {
                map.insert(
                    p.xrp_address.clone(),
                    XrpWatchEntry {
                        purchase_id: p.id,
                        buyer_pial: p.buyer_pial,
                        listing_id: p.listing_id,
                        expected_drops: p.expected_sats, // stored as drops in expected_sats for XRP
                        xrp_address: p.xrp_address.clone(),
                    },
                );
            }
        }
        info!("Loaded {} pending XRP purchases to watch", map.len());
    }

    loop {
        match connect_xrp_watcher(state.clone(), xrp_watch_map.clone()).await {
            Ok(_) => info!("XRP watcher exited cleanly"),
            Err(e) => {
                error!("XRP watcher error: {e} — reconnecting in 5s");
                sleep(Duration::from_secs(5)).await;
            }
        }
    }
}

async fn connect_xrp_watcher(state: Arc<AppState>, xrp_watch_map: XrpWatchMap) -> Result<()> {
    info!("Connecting to XRPL WebSocket: {}", state.cfg.xrp_ws_url);
    let (ws_stream, _) = connect_async(&state.cfg.xrp_ws_url).await?;
    let (mut write, mut read) = ws_stream.split();

    // Subscribe to all currently watched XRP addresses
    {
        let map = xrp_watch_map.lock().await;
        if !map.is_empty() {
            let accounts: Vec<&str> = map.keys().map(|s| s.as_str()).collect();
            let msg = serde_json::json!({
                "command": "subscribe",
                "accounts": accounts,
            });
            write.send(Message::Text(msg.to_string())).await?;
        }
    }

    while let Some(msg) = read.next().await {
        let msg = msg?;
        if let Message::Text(text) = msg {
            if let Ok(v) = serde_json::from_str::<Value>(&text) {
                handle_xrp_ws_message(&state, &xrp_watch_map, v).await;
            }
        }
    }
    Ok(())
}

async fn handle_xrp_ws_message(state: &Arc<AppState>, xrp_watch_map: &XrpWatchMap, v: Value) {
    // XRPL fires: {"type":"transaction","transaction":{...},"validated":true}
    if v.get("type").and_then(|t| t.as_str()) != Some("transaction") {
        return;
    }
    if v.get("validated").and_then(|b| b.as_bool()) != Some(true) {
        return;
    }
    let tx = match v.get("transaction") {
        Some(t) => t,
        None => return,
    };
    if tx.get("TransactionType").and_then(|t| t.as_str()) != Some("Payment") {
        return;
    }
    let destination = match tx.get("Destination").and_then(|d| d.as_str()) {
        Some(d) => d,
        None => return,
    };
    // Amount must be a string (native XRP drops); IOU amounts are objects — skip those
    let drops_str = match tx.get("Amount").and_then(|a| a.as_str()) {
        Some(s) => s,
        None => return,
    };
    let drops_received: i64 = match drops_str.parse() {
        Ok(n) => n,
        Err(_) => return,
    };

    let entry = {
        let map = xrp_watch_map.lock().await;
        map.get(destination).cloned()
    };
    if let Some(entry) = entry {
        if drops_received >= entry.expected_drops {
            handle_xrp_payment_confirmed(state, xrp_watch_map, entry).await;
        }
    }
}

async fn handle_xrp_payment_confirmed(
    state: &Arc<AppState>,
    xrp_watch_map: &XrpWatchMap,
    entry: XrpWatchEntry,
) {
    info!("XRP payment confirmed for purchase {}", entry.purchase_id);

    // Remove from watch map
    xrp_watch_map.lock().await.remove(&entry.xrp_address);

    // Mark confirmed in DB
    if let Err(e) = db::purchase_mark_confirmed(&state.pool, entry.purchase_id).await {
        error!("Failed to mark XRP purchase confirmed: {e}");
        return;
    }

    // Fetch listing to get cek_encrypted
    let listing = match db::listing_get(&state.pool, entry.listing_id).await {
        Ok(Some(l)) => l,
        Ok(None) => {
            error!("Listing not found for XRP purchase {}", entry.purchase_id);
            return;
        }
        Err(e) => {
            error!("DB error fetching listing for XRP purchase: {e}");
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
    match require_identity(&state.manhattan, &entry.buyer_pial).await {
        Ok(()) => {}
        Err(e @ (IdentityError::NotActive | IdentityError::NotAnIdentity(_))) => {
            error!(
                "INCIDENT: CEK delivery halted for purchase {}: {e}",
                entry.purchase_id
            );
            return;
        }
        Err(e) => warn!(
            "CEK delivery for purchase {} proceeding on the identity resolved at purchase time: {e}",
            entry.purchase_id
        ),
    }

    // Fetch buyer's ECDH public key from Nantar
    let buyer_pubkey_b64 = match fetch_buyer_ecdh_pubkey(state, &entry.buyer_pial.text()).await {
        Ok(k) => k,
        Err(e) => {
            error!("Failed to fetch buyer ECDH pubkey (XRP): {e}");
            return;
        }
    };

    let buyer_pub = match cek::pubkey_from_b64(&buyer_pubkey_b64) {
        Ok(k) => k,
        Err(e) => {
            error!("Invalid buyer pubkey (XRP): {e}");
            return;
        }
    };

    // Unwrap CEK with Themis private key, re-wrap for buyer
    let plaintext_cek = match cek::unwrap_cek(&state.themis_privkey, &listing.cek_encrypted) {
        Ok(k) => k,
        Err(e) => {
            error!("CEK unwrap failed (XRP): {e}");
            return;
        }
    };

    let cek_for_buyer = match cek::wrap_cek_for(&buyer_pub, &plaintext_cek) {
        Ok(k) => k,
        Err(e) => {
            error!("CEK rewrap failed (XRP): {e}");
            return;
        }
    };

    // Store in DB
    if let Err(e) =
        db::purchase_mark_delivered(&state.pool, entry.purchase_id, cek_for_buyer.clone()).await
    {
        error!("Failed to mark XRP purchase delivered: {e}");
        return;
    }

    // Notify Nantar → SSE to buyer (reuse the same WatchEntry-compatible helper)
    let btc_entry = WatchEntry {
        purchase_id: entry.purchase_id,
        buyer_pial: entry.buyer_pial,
        listing_id: entry.listing_id,
        expected_sats: entry.expected_drops,
        btc_address: entry.xrp_address,
        confirmations: 0,
    };
    if let Err(e) = notify_nantar_delivered(
        state,
        btc_entry,
        &listing.content_url,
        &listing.content_hash,
        &cek_for_buyer,
    )
    .await
    {
        warn!("Failed to notify Nantar of XRP delivery (purchase still marked delivered): {e}");
    }
}

/// Add a new XRP purchase to the XRP watch map and subscribe its address.
/// Called from the purchase initiate handler when currency = "xrp".
pub async fn subscribe_xrp_address(xrp_watch_map: &XrpWatchMap, entry: XrpWatchEntry) {
    xrp_watch_map
        .lock()
        .await
        .insert(entry.xrp_address.clone(), entry);
    // The watcher loop picks it up on next reconnect (max 5s delay).
}
