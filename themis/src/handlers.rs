use crate::{db, models::*, watcher, AppState};
use axum::{
    extract::{Path, Query, State},
    http::{HeaderMap, StatusCode},
    response::IntoResponse,
    Json,
};
use base64::{engine::general_purpose::{STANDARD as B64, URL_SAFE_NO_PAD as B64URL}, Engine};
use p256::ecdsa::{signature::Verifier, Signature, VerifyingKey};
use p256::pkcs8::DecodePublicKey;
use serde::Deserialize;
use std::sync::Arc;
use uuid::Uuid;

/// Fetch the PIAL signing public key from Elohim-veni and verify a marketplace event signature.
/// cid: the "sha256:..." content ID string that was signed
/// sig_b64url: base64url-encoded ECDSA-P256 signature (IEEE P1363 r||s format, 64 bytes)
/// Returns Ok(()) if valid, Err(()) if invalid or key not found.
async fn verify_pial_signature(
    http: &reqwest::Client,
    elohim_veni_url: &str,
    internal_key: &str,
    seller_pial: &Uuid,
    cid: &str,
    sig_b64url: &str,
) -> Result<(), ()> {
    // Fetch signing pubkey from the Elohim-veni key service (key custody only)
    let url = format!("{}/v1/pial/{}/signing-pubkey", elohim_veni_url, seller_pial);
    let resp = http.get(&url)
        .header("X-Internal-Key", internal_key)
        .send().await
        .map_err(|_| ())?;
    if !resp.status().is_success() {
        return Err(());
    }
    let body: serde_json::Value = resp.json().await.map_err(|_| ())?;
    let pub_key_b64 = body["public_key_b64"].as_str().ok_or(())?;

    // Decode SPKI-DER public key (standard base64, as stored by Malkuth)
    let pub_key_bytes = B64.decode(pub_key_b64).map_err(|_| ())?;
    let public_key = p256::PublicKey::from_public_key_der(&pub_key_bytes).map_err(|_| ())?;
    let verifying_key = VerifyingKey::from(&public_key);

    // Decode base64url signature (WebCrypto ECDSA returns IEEE P1363 r||s, 64 bytes)
    let sig_bytes = B64URL.decode(sig_b64url).map_err(|_| ())?;
    let signature = Signature::from_slice(&sig_bytes).map_err(|_| ())?;

    // Verify: Malkuth signs the CID string bytes (UTF-8), WebCrypto hashes internally with SHA-256
    let data = cid.as_bytes();
    verifying_key.verify(data, &signature).map_err(|_| ())
}

/// Validates a Bitcoin address format (does not check checksum — just format).
/// Accepts:
///   P2PKH (Legacy):    starts with '1', length 25–34, Base58 chars
///   P2SH:              starts with '3', length 25–34, Base58 chars
///   Bech32 SegWit v0:  starts with 'bc1q', length 42 (P2WPKH) or 62 (P2WSH)
///   Bech32m Taproot:   starts with 'bc1p', length 62
/// Base58 alphabet: 123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz
fn validate_btc_address(addr: &str) -> bool {
    if addr.len() < 25 || addr.len() > 90 {
        return false;
    }
    if addr.starts_with("bc1") {
        // Bech32 / Bech32m: all lowercase after the HRP separator
        let lower = addr.to_lowercase();
        if lower != addr {
            return false; // mixed case invalid for bech32
        }
        // Only valid bech32 chars: 0-9, a-z (excluding 'b', 'i', 'o', '1' in data part —
        // but the HRP 'bc' uses them, so just check the charset broadly)
        let valid_chars: bool = addr.chars().all(|c| c.is_ascii_alphanumeric());
        if !valid_chars {
            return false;
        }
        // bc1q (P2WPKH = 42 chars) or bc1q (P2WSH = 62 chars) or bc1p (Taproot = 62 chars)
        let len = addr.len();
        return (addr.starts_with("bc1q") || addr.starts_with("bc1p")) && (len == 42 || len == 62);
    }
    // Legacy P2PKH (starts with '1') or P2SH (starts with '3')
    if !addr.starts_with('1') && !addr.starts_with('3') {
        return false;
    }
    // Base58 charset check: no 0 (zero), O (capital oh), I (capital eye), l (lowercase L)
    let base58_chars: bool = addr.chars().all(|c| {
        matches!(c,
            '1'..='9' |
            'A'..='H' | 'J'..='N' | 'P'..='Z' |
            'a'..='k' | 'm'..='z'
        )
    });
    base58_chars && addr.len() >= 25 && addr.len() <= 34
}

/// Validates an XRP address format: starts with 'r', 25–34 chars, base58 alphabet.
fn validate_xrp_address(addr: &str) -> bool {
    if !addr.starts_with('r') {
        return false;
    }
    let len = addr.len();
    if len < 25 || len > 34 {
        return false;
    }
    // Base58 alphabet (same as Bitcoin, excludes 0, O, I, l)
    addr.chars().all(|c| {
        matches!(c,
            '1'..='9' |
            'A'..='H' | 'J'..='N' | 'P'..='Z' |
            'a'..='k' | 'm'..='z'
        )
    })
}

fn pial_from_headers(headers: &HeaderMap) -> Option<Uuid> {
    headers.get("X-Pial-Identity")
        .and_then(|v| v.to_str().ok())
        .and_then(|s| Uuid::parse_str(s).ok())
}

fn internal_key_ok(headers: &HeaderMap, cfg_key: &str) -> bool {
    headers.get("X-Internal-Key")
        .and_then(|v| v.to_str().ok())
        .map(|k| !cfg_key.is_empty() && k == cfg_key)
        .unwrap_or(false)
}

// GET /health
pub async fn health() -> impl IntoResponse {
    Json(serde_json::json!({"ok": true}))
}

// GET /v1/pubkey — Themis ECDH public key for browser to encrypt CEK
pub async fn pubkey(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    Json(serde_json::json!({ "public_key_b64": state.themis_pubkey_b64 }))
}

// POST /v1/events — receives marketplace.* events from Nantar
pub async fn events(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(evt): Json<MarketplaceEvent>,
) -> impl IntoResponse {
    let seller_pial = match pial_from_headers(&headers) {
        Some(p) => p,
        None => return (StatusCode::UNAUTHORIZED, Json(serde_json::json!({"error":"pial_required"}))).into_response(),
    };

    // Verify PIAL signature for high-value mutations.
    // shop.open uses session auth only — PIAL session already proves creator identity.
    // listing.create and listing.delete require device-bound signing (encrypted content).
    let requires_sig = matches!(evt.event_type.as_str(),
        "marketplace.listing.create" | "marketplace.listing.delete"
    );
    if requires_sig {
        match (&evt.cid, &evt.pial_sig) {
            (Some(cid), Some(sig)) => {
                if verify_pial_signature(
                    &state.http,
                    &state.cfg.elohim_veni_url,
                    &state.cfg.internal_api_key,
                    &seller_pial,
                    cid,
                    sig,
                ).await.is_err() {
                    return (StatusCode::UNAUTHORIZED, Json(serde_json::json!({
                        "error": "invalid_signature",
                        "message": "PIAL signature verification failed"
                    }))).into_response();
                }
            }
            _ => {
                return (StatusCode::UNAUTHORIZED, Json(serde_json::json!({
                    "error": "signature_required",
                    "message": "PIAL signature required for this operation"
                }))).into_response();
            }
        }
    }

    match evt.event_type.as_str() {
        "marketplace.shop.open" => {
            let btc = match evt.btc_address {
                Some(ref a) if !a.is_empty() => a.clone(),
                _ => return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"btc_address required"}))).into_response(),
            };
            if !validate_btc_address(&btc) {
                return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"invalid_btc_address","message":"That doesn't look like a valid Bitcoin address."}))).into_response();
            }
            // Reject if another active shop already claims this BTC address
            if let Ok(Some(existing)) = db::shop_get_by_btc(&state.pool, &btc).await {
                if existing.seller_pial != seller_pial {
                    return (StatusCode::CONFLICT, Json(serde_json::json!({"error":"btc_address_taken","message":"This Bitcoin address is already registered to another shop."}))).into_response();
                }
            }
            // Optional XRP address
            let xrp = match evt.xrp_address {
                Some(ref a) if !a.is_empty() => {
                    if !validate_xrp_address(a) {
                        return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"invalid_xrp_address","message":"That doesn't look like a valid XRP address."}))).into_response();
                    }
                    // Reject if another active shop already claims this XRP address
                    if let Ok(Some(existing)) = db::shop_get_by_xrp(&state.pool, a).await {
                        if existing.seller_pial != seller_pial {
                            return (StatusCode::CONFLICT, Json(serde_json::json!({"error":"xrp_address_taken","message":"This XRP address is already registered to another shop."}))).into_response();
                        }
                    }
                    a.clone()
                }
                _ => String::new(),
            };
            match db::shop_open(&state.pool, seller_pial, &btc, &xrp).await {
                Ok(_) => Json(serde_json::json!({"ok":true})).into_response(),
                Err(e) => {
                    tracing::error!("shop_open: {e}");
                    (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
                }
            }
        }

        "marketplace.shop.close" => {
            let _ = db::shop_close(&state.pool, seller_pial).await;
            Json(serde_json::json!({"ok":true})).into_response()
        }

        "marketplace.listing.create" => {
            let req = ListingCreateReq {
                title:            evt.title.unwrap_or_default(),
                description:      evt.description.unwrap_or_default(),
                price_sats:       evt.price_sats.unwrap_or(0),
                price_xrp_drops:  evt.price_xrp_drops,
                payment_currency: evt.payment_currency.clone(),
                content_url:      evt.content_url.unwrap_or_default(),
                content_hash:     evt.content_hash.unwrap_or_default(),
                cek_encrypted:    evt.cek_encrypted.unwrap_or_default(),
                phash:            evt.phash,
                visibility:       evt.visibility,
            };
            if req.title.is_empty() || req.content_url.is_empty() || req.cek_encrypted.is_empty() {
                return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"missing fields"}))).into_response();
            }
            if req.price_sats < 1000 {
                return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"price_too_low","message":"Minimum price is 1,000 sats"}))).into_response();
            }
            let cek_bytes = match B64.decode(&req.cek_encrypted) {
                Ok(b) => b,
                Err(_) => return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"invalid cek_encrypted"}))).into_response(),
            };
            // Count active listings — max 12
            match db::listings_for_shop(&state.pool, seller_pial, true).await {
                Ok(existing) if existing.len() >= 12 => {
                    return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"max_listings_reached"}))).into_response();
                }
                _ => {}
            }
            match db::listing_create(&state.pool, seller_pial, &req, cek_bytes).await {
                Ok(l) => Json(serde_json::json!({"ok":true, "listing_id": l.id})).into_response(),
                Err(e) => {
                    tracing::error!("listing_create: {e}");
                    (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
                }
            }
        }

        "marketplace.listing.edit" => {
            let listing_id = match evt.listing_id {
                Some(id) => id,
                None => return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"listing_id required"}))).into_response(),
            };
            // Verify ownership
            match db::listing_get(&state.pool, listing_id).await {
                Ok(Some(l)) if l.shop_pial == seller_pial => {}
                Ok(Some(_)) => return (StatusCode::FORBIDDEN, Json(serde_json::json!({"error":"forbidden"}))).into_response(),
                Ok(None) => return (StatusCode::NOT_FOUND, Json(serde_json::json!({"error":"not_found"}))).into_response(),
                Err(e) => {
                    tracing::error!("{e}");
                    return (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response();
                }
            }
            // Apply edit (title, description only — price/content immutable after listing)
            if let Some(ref title) = evt.title {
                let _ = db::listing_update_title(&state.pool, listing_id, title).await;
            }
            if let Some(ref desc) = evt.description {
                let _ = db::listing_update_description(&state.pool, listing_id, desc).await;
            }
            if let Some(active) = evt.is_active {
                let _ = db::listing_set_active(&state.pool, listing_id, active).await;
            }
            Json(serde_json::json!({"ok":true})).into_response()
        }

        "marketplace.listing.delete" => {
            let listing_id = match evt.listing_id {
                Some(id) => id,
                None => return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"listing_id required"}))).into_response(),
            };
            match db::listing_get(&state.pool, listing_id).await {
                Ok(Some(l)) if l.shop_pial == seller_pial => {
                    let _ = db::listing_set_active(&state.pool, listing_id, false).await;
                    Json(serde_json::json!({"ok":true})).into_response()
                }
                Ok(Some(_)) => (StatusCode::FORBIDDEN, Json(serde_json::json!({"error":"forbidden"}))).into_response(),
                _ => (StatusCode::NOT_FOUND, Json(serde_json::json!({"error":"not_found"}))).into_response(),
            }
        }

        "marketplace.purchase.initiate" => {
            let listing_id = match evt.listing_id {
                Some(id) => id,
                None => return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"listing_id required"}))).into_response(),
            };
            let listing = match db::listing_get(&state.pool, listing_id).await {
                Ok(Some(l)) if l.is_active => l,
                Ok(_) => return (StatusCode::NOT_FOUND, Json(serde_json::json!({"error":"listing_not_found"}))).into_response(),
                Err(e) => {
                    tracing::error!("{e}");
                    return (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response();
                }
            };
            // Check if already purchased
            if let Ok(Some(_)) = db::purchase_get_by_buyer_and_listing(&state.pool, seller_pial, listing_id).await {
                return (StatusCode::CONFLICT, Json(serde_json::json!({"error":"already_purchased"}))).into_response();
            }
            // Get seller's shop
            let shop = match db::shop_get(&state.pool, listing.shop_pial).await {
                Ok(Some(s)) if s.is_active => s,
                _ => return (StatusCode::SERVICE_UNAVAILABLE, Json(serde_json::json!({"error":"shop_unavailable"}))).into_response(),
            };

            // Determine payment currency
            let requested_currency = evt.payment_currency.as_deref().unwrap_or("btc");
            let use_xrp = requested_currency == "xrp";

            if use_xrp {
                // XRP payment path
                if shop.xrp_address.is_empty() {
                    return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"xrp_not_supported","message":"This seller does not accept XRP."}))).into_response();
                }
                let drops = listing.price_xrp_drops;
                if drops <= 0 {
                    return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"xrp_price_not_set","message":"No XRP price set for this listing."}))).into_response();
                }
                // Create purchase record — store drops in expected_sats for XRP
                let purchase = match db::purchase_create(
                    &state.pool, listing_id, seller_pial,
                    "", // btc_address unused for XRP
                    drops,
                    "xrp",
                    &shop.xrp_address,
                ).await {
                    Ok(p) => p,
                    Err(e) => {
                        tracing::error!("{e}");
                        return (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response();
                    }
                };
                // Subscribe XRP address in watcher
                watcher::subscribe_xrp_address(&state.xrp_watch_map, watcher::XrpWatchEntry {
                    purchase_id:    purchase.id,
                    buyer_pial:     seller_pial,
                    listing_id,
                    expected_drops: drops,
                    xrp_address:    shop.xrp_address.clone(),
                }).await;

                // Start expiry timer
                let pool = state.pool.clone();
                let purchase_id = purchase.id;
                let expires_at = purchase.expires_at;
                tokio::spawn(async move {
                    let now = chrono::Utc::now();
                    let delay = (expires_at - now).to_std().unwrap_or_default();
                    tokio::time::sleep(delay).await;
                    let _ = db::purchase_mark_expired(&pool, purchase_id).await;
                });

                let amount_xrp = drops as f64 / 1_000_000.0;
                Json(serde_json::json!({
                    "ok":          true,
                    "purchase_id": purchase.id,
                    "currency":    "xrp",
                    "xrp_address": shop.xrp_address,
                    "amount_drops": drops,
                    "amount_xrp":  amount_xrp,
                    "expires_at":  purchase.expires_at,
                })).into_response()
            } else {
                // BTC payment path (default)
                let purchase = match db::purchase_create(
                    &state.pool, listing_id, seller_pial,
                    &shop.btc_address, listing.price_sats,
                    "btc", "",
                ).await {
                    Ok(p) => p,
                    Err(e) => {
                        tracing::error!("{e}");
                        return (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response();
                    }
                };
                // Subscribe BTC address in watcher
                watcher::subscribe_address(&state.watch_map, watcher::WatchEntry {
                    purchase_id:   purchase.id,
                    buyer_pial:    seller_pial,  // buyer is the requesting PIAL
                    listing_id,
                    expected_sats: listing.price_sats,
                    btc_address:   shop.btc_address.clone(),
                    confirmations: 0,
                }).await;

                // Start expiry timer
                let pool = state.pool.clone();
                let purchase_id = purchase.id;
                let expires_at = purchase.expires_at;
                tokio::spawn(async move {
                    let now = chrono::Utc::now();
                    let delay = (expires_at - now).to_std().unwrap_or_default();
                    tokio::time::sleep(delay).await;
                    let _ = db::purchase_mark_expired(&pool, purchase_id).await;
                });

                Json(serde_json::json!({
                    "ok":          true,
                    "purchase_id": purchase.id,
                    "currency":    "btc",
                    "btc_address": shop.btc_address,
                    "amount_sats": listing.price_sats,
                    "expires_at":  purchase.expires_at,
                })).into_response()
            }
        }

        "marketplace.subscription.create" => {
            let sp = match evt.seller_pial {
                Some(p) => p,
                None => return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"seller_pial required"}))).into_response(),
            };
            match db::marketplace_subscription_create(&state.pool, seller_pial, sp).await {
                Ok(_) => Json(serde_json::json!({"ok":true})).into_response(),
                Err(e) => {
                    tracing::error!("{e}");
                    (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
                }
            }
        }

        "marketplace.subscription.cancel" => {
            let sp = match evt.seller_pial {
                Some(p) => p,
                None => return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"seller_pial required"}))).into_response(),
            };
            let _ = db::marketplace_subscription_cancel(&state.pool, seller_pial, sp).await;
            Json(serde_json::json!({"ok":true})).into_response()
        }

        _ => (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"unknown_event_type"}))).into_response(),
    }
}

// GET /v1/shop/:seller_pial — public shop listings
pub async fn shop_listings(
    State(state): State<Arc<AppState>>,
    Path(seller_pial): Path<Uuid>,
    headers: HeaderMap,
) -> impl IntoResponse {
    let requester = pial_from_headers(&headers);
    let is_owner = requester == Some(seller_pial);
    let include_private = if let Some(buyer) = requester {
        is_owner || db::marketplace_subscription_active(&state.pool, buyer, seller_pial).await.unwrap_or(false)
    } else {
        false
    };

    match db::listings_for_shop(&state.pool, seller_pial, include_private).await {
        Ok(listings) => Json(serde_json::json!({"listings": listings})).into_response(),
        Err(e) => {
            tracing::error!("{e}");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
        }
    }
}

// GET /v1/marketplace — public feed of all listings
#[derive(Deserialize)]
pub struct FeedQuery {
    pub limit:  Option<i64>,
    pub offset: Option<i64>,
}

pub async fn marketplace_feed(
    State(state): State<Arc<AppState>>,
    Query(q): Query<FeedQuery>,
) -> impl IntoResponse {
    let limit  = q.limit.unwrap_or(20).min(50);
    let offset = q.offset.unwrap_or(0);
    match db::listings_public(&state.pool, limit, offset).await {
        Ok(listings) => Json(serde_json::json!({"listings": listings})).into_response(),
        Err(e) => {
            tracing::error!("{e}");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
        }
    }
}

// GET /v1/purchase/:id — purchase status
pub async fn purchase_status(
    State(state): State<Arc<AppState>>,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
) -> impl IntoResponse {
    let requester = pial_from_headers(&headers);
    match db::purchase_get(&state.pool, id).await {
        Ok(Some(p)) => {
            if requester != Some(p.buyer_pial) {
                return (StatusCode::FORBIDDEN, Json(serde_json::json!({"error":"forbidden"}))).into_response();
            }
            Json(serde_json::json!({
                "id":            p.id,
                "status":        p.status,
                "btc_address":   p.btc_address,
                "amount_sats":   p.expected_sats,
                "expires_at":    p.expires_at,
                "cek_for_buyer": p.cek_for_buyer.map(|b| B64.encode(&b)),
            })).into_response()
        }
        Ok(None) => (StatusCode::NOT_FOUND, Json(serde_json::json!({"error":"not_found"}))).into_response(),
        Err(e) => {
            tracing::error!("{e}");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
        }
    }
}

// GET /v1/library — buyer's delivered purchases
pub async fn buyer_library(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> impl IntoResponse {
    let buyer = match pial_from_headers(&headers) {
        Some(p) => p,
        None => return (StatusCode::UNAUTHORIZED, Json(serde_json::json!({"error":"unauthorized"}))).into_response(),
    };
    match db::purchases_by_buyer(&state.pool, buyer).await {
        Ok(purchases) => Json(serde_json::json!({"purchases": purchases})).into_response(),
        Err(e) => {
            tracing::error!("{e}");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
        }
    }
}

// GET /v1/access — check subscription or purchase access for a listing
#[derive(Deserialize)]
pub struct AccessQuery {
    pub listing_id: Uuid,
}

pub async fn access_check(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Query(q): Query<AccessQuery>,
) -> impl IntoResponse {
    let buyer = match pial_from_headers(&headers) {
        Some(p) => p,
        None => return Json(serde_json::json!({"has_access": false})).into_response(),
    };
    let listing = match db::listing_get(&state.pool, q.listing_id).await {
        Ok(Some(l)) => l,
        _ => return Json(serde_json::json!({"has_access": false})).into_response(),
    };
    // Direct purchase check
    if let Ok(Some(_)) = db::purchase_get_by_buyer_and_listing(&state.pool, buyer, q.listing_id).await {
        return Json(serde_json::json!({"has_access": true})).into_response();
    }
    // Subscription check for subscribers_only listings
    if listing.visibility == "subscribers_only" {
        let active = db::marketplace_subscription_active(&state.pool, buyer, listing.shop_pial).await.unwrap_or(false);
        return Json(serde_json::json!({"has_access": active})).into_response();
    }
    Json(serde_json::json!({"has_access": false})).into_response()
}

// GET /v1/shop/:seller_pial/status — check if a shop exists and is open
pub async fn shop_status(
    State(state): State<Arc<AppState>>,
    Path(seller_pial): Path<Uuid>,
) -> impl IntoResponse {
    match db::shop_get(&state.pool, seller_pial).await {
        Ok(Some(shop)) => Json(serde_json::json!({
            "exists":      true,
            "is_active":   shop.is_active,
            "btc_address": shop.btc_address,
            "xrp_address": shop.xrp_address,
        })).into_response(),
        Ok(None) => Json(serde_json::json!({
            "exists":      false,
            "is_active":   false,
            "btc_address": "",
            "xrp_address": "",
        })).into_response(),
        Err(e) => {
            tracing::error!("shop_status: {e}");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
        }
    }
}

// GET /v1/seller/:seller_pial/stats — seller's own sales stats
pub async fn seller_stats(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(seller_pial): Path<Uuid>,
) -> impl IntoResponse {
    // Only the seller themselves can see their stats
    let requester = match pial_from_headers(&headers) {
        Some(p) => p,
        None => return (StatusCode::UNAUTHORIZED, Json(serde_json::json!({"error":"unauthorized"}))).into_response(),
    };
    if requester != seller_pial {
        return (StatusCode::FORBIDDEN, Json(serde_json::json!({"error":"forbidden"}))).into_response();
    }
    match db::seller_stats(&state.pool, seller_pial).await {
        Ok((sales, revenue_sats, pending)) => Json(serde_json::json!({
            "sales":        sales,
            "revenue_sats": revenue_sats,
            "pending":      pending,
        })).into_response(),
        Err(e) => {
            tracing::error!("seller_stats: {e}");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response()
        }
    }
}

// ── Suppress unused import warning for internal_key_ok (used as a guard helper) ──
#[allow(dead_code)]
fn _use_internal_key_ok(headers: &HeaderMap, key: &str) -> bool {
    internal_key_ok(headers, key)
}

// ── Commerce handlers (from Thessalon) ────────────────────────────────────────

use crate::clients;
use axum::extract::Query as AxumQuery;
use serde_json::{json, Value};
use sqlx::PgPool as SqPool;

type CommerceRes<T> = Result<Json<T>, (StatusCode, Json<Value>)>;

fn cerr(status: StatusCode, msg: &str) -> (StatusCode, Json<Value>) {
    (status, Json(json!({"error": msg})))
}

fn cdberr(e: sqlx::Error) -> (StatusCode, Json<Value>) {
    cerr(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string())
}

pub async fn commerce_stats(State(state): State<Arc<AppState>>) -> Json<Value> {
    let pool: &SqPool = &state.pool;
    let creators: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM creator_eligibility WHERE monetization_enabled"
    ).fetch_one(pool).await.unwrap_or((0,));
    let active_subs: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM subscriptions WHERE status = 'active'"
    ).fetch_one(pool).await.unwrap_or((0,));
    let tx_count: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM commerce_transactions"
    ).fetch_one(pool).await.unwrap_or((0,));
    let volume: (Option<i64>,) = sqlx::query_as(
        "SELECT SUM(gross_aet) FROM commerce_transactions WHERE status = 'completed'"
    ).fetch_one(pool).await.unwrap_or((None,));
    Json(json!({
        "active_creators": creators.0,
        "active_subscriptions": active_subs.0,
        "total_transactions": tx_count.0,
        "total_volume_aet": volume.0.unwrap_or(0),
    }))
}

pub async fn creator_enable(
    State(state): State<Arc<AppState>>,
    Json(req): Json<EnableCreatorReq>,
) -> CommerceRes<Value> {
    let tier = clients::kyc_tier(&state.http, &state.cfg.verity_url, &req.pial_id).await;
    sqlx::query(
        "INSERT INTO creator_eligibility (pial_id, kyc_tier, monetization_enabled, enabled_at) \
         VALUES ($1, $2, true, NOW()) \
         ON CONFLICT (pial_id) DO UPDATE \
         SET kyc_tier = $2, monetization_enabled = true, enabled_at = NOW(), \
             disabled_at = NULL, disabled_reason = NULL"
    )
    .bind(&req.pial_id)
    .bind(tier)
    .execute(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"pial_id": req.pial_id, "monetization_enabled": true, "kyc_tier": tier})))
}

pub async fn creator_status(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<String>,
) -> CommerceRes<Value> {
    let row: Option<(bool, i32)> = sqlx::query_as(
        "SELECT monetization_enabled, kyc_tier FROM creator_eligibility WHERE pial_id = $1"
    )
    .bind(&pial_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    let (enabled, tier) = row.unwrap_or((false, 0));
    Ok(Json(json!({"pial_id": pial_id, "monetization_enabled": enabled, "kyc_tier": tier})))
}

pub async fn list_plans(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<String>,
) -> CommerceRes<Vec<PlanRow>> {
    let rows = sqlx::query_as::<_, PlanRow>(
        "SELECT id, creator_pial_id, name, price_aet, description, is_active, created_at \
         FROM subscription_plans WHERE creator_pial_id = $1 ORDER BY price_aet ASC"
    )
    .bind(&pial_id)
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn create_plan(
    State(state): State<Arc<AppState>>,
    Json(req): Json<CreatePlanReq>,
) -> CommerceRes<PlanRow> {
    let enabled: Option<(bool,)> = sqlx::query_as(
        "SELECT monetization_enabled FROM creator_eligibility WHERE pial_id = $1"
    )
    .bind(&req.creator_pial_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    if !enabled.map(|r| r.0).unwrap_or(false) {
        return Err(cerr(StatusCode::FORBIDDEN, "creator monetization not enabled"));
    }
    let price_units = aet_to_units(req.price_aet);
    if price_units <= 0 {
        return Err(cerr(StatusCode::BAD_REQUEST, "price must be greater than zero"));
    }
    let row = sqlx::query_as::<_, PlanRow>(
        "INSERT INTO subscription_plans (creator_pial_id, name, price_aet, description) \
         VALUES ($1, $2, $3, $4) \
         RETURNING id, creator_pial_id, name, price_aet, description, is_active, created_at"
    )
    .bind(&req.creator_pial_id)
    .bind(&req.name)
    .bind(price_units)
    .bind(&req.description)
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(row))
}

pub async fn update_plan(
    State(state): State<Arc<AppState>>,
    Path(id): Path<Uuid>,
    Json(req): Json<UpdatePlanReq>,
) -> CommerceRes<PlanRow> {
    let price = req.price_aet.map(aet_to_units);
    let row = sqlx::query_as::<_, PlanRow>(
        "UPDATE subscription_plans SET \
           name        = COALESCE($2, name), \
           price_aet   = COALESCE($3, price_aet), \
           description = COALESCE($4, description), \
           is_active   = COALESCE($5, is_active) \
         WHERE id = $1 \
         RETURNING id, creator_pial_id, name, price_aet, description, is_active, created_at"
    )
    .bind(id)
    .bind(req.name)
    .bind(price)
    .bind(req.description)
    .bind(req.is_active)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?
    .ok_or_else(|| cerr(StatusCode::NOT_FOUND, "plan not found"))?;
    Ok(Json(row))
}

pub async fn delete_plan(
    State(state): State<Arc<AppState>>,
    Path(id): Path<Uuid>,
) -> CommerceRes<Value> {
    sqlx::query("UPDATE subscription_plans SET is_active = false WHERE id = $1")
        .bind(id)
        .execute(&state.pool)
        .await
        .map_err(cdberr)?;
    Ok(Json(json!({"id": id, "is_active": false})))
}

pub async fn subscribe(
    State(state): State<Arc<AppState>>,
    Json(req): Json<SubscribeReq>,
) -> CommerceRes<Value> {
    let existing: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM subscriptions \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 AND status = 'active'"
    )
    .bind(&req.subscriber_pial_id)
    .bind(&req.creator_pial_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    if existing.is_some() {
        return Err(cerr(StatusCode::CONFLICT, "already subscribed"));
    }
    let plan: Option<(i64,)> = sqlx::query_as(
        "SELECT price_aet FROM subscription_plans WHERE id = $1 AND is_active"
    )
    .bind(req.plan_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    let price_units = plan.ok_or_else(|| cerr(StatusCode::NOT_FOUND, "plan not found or inactive"))?.0;
    let tx_id = clients::transfer(
        &state.http, &state.cfg.ain_soph_url,
        &req.subscriber_pial_id, &req.creator_pial_id,
        price_units, "subscription",
    )
    .await
    .map_err(|e| cerr(StatusCode::PAYMENT_REQUIRED, &e.to_string()))?;
    let period_end = chrono::Utc::now() + chrono::Duration::days(30);
    let sub_id: (Uuid,) = sqlx::query_as(
        "INSERT INTO subscriptions \
           (subscriber_pial_id, creator_pial_id, plan_id, current_period_end) \
         VALUES ($1, $2, $3, $4) RETURNING id"
    )
    .bind(&req.subscriber_pial_id)
    .bind(&req.creator_pial_id)
    .bind(req.plan_id)
    .bind(period_end)
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    sqlx::query(
        "INSERT INTO commerce_transactions \
           (tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet, ain_soph_tx_id) \
         VALUES ('subscription', $1, $2, $3, $4, $5)"
    )
    .bind(sub_id.0)
    .bind(&req.subscriber_pial_id)
    .bind(&req.creator_pial_id)
    .bind(price_units)
    .bind(&tx_id)
    .execute(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({
        "subscription_id": sub_id.0,
        "subscriber_pial_id": req.subscriber_pial_id,
        "creator_pial_id":    req.creator_pial_id,
        "plan_id":            req.plan_id,
        "amount_aet":         units_to_aet(price_units),
        "period_end":         period_end,
        "ain_soph_tx_id":     tx_id,
        "status":             "active",
    })))
}

pub async fn unsubscribe(
    State(state): State<Arc<AppState>>,
    Json(req): Json<UnsubscribeReq>,
) -> CommerceRes<Value> {
    let result = sqlx::query(
        "UPDATE subscriptions SET status = 'cancelled', cancelled_at = NOW() \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 AND status = 'active'"
    )
    .bind(&req.subscriber_pial_id)
    .bind(&req.creator_pial_id)
    .execute(&state.pool)
    .await
    .map_err(cdberr)?;
    if result.rows_affected() == 0 {
        return Err(cerr(StatusCode::NOT_FOUND, "no active subscription found"));
    }
    Ok(Json(json!({"status": "cancelled"})))
}

pub async fn check_subscription(
    State(state): State<Arc<AppState>>,
    AxumQuery(q): AxumQuery<CommerceAccessQuery>,
) -> CommerceRes<Value> {
    let subscriber = q.subscriber.ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "subscriber required"))?;
    let creator    = q.creator.ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "creator required"))?;
    let has: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM subscriptions \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 \
           AND status = 'active' AND current_period_end > NOW()"
    )
    .bind(&subscriber)
    .bind(&creator)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"has_access": has.is_some()})))
}

pub async fn list_subscribers(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<String>,
) -> CommerceRes<Vec<SubscriptionRow>> {
    let rows = sqlx::query_as::<_, SubscriptionRow>(
        "SELECT id, subscriber_pial_id, creator_pial_id, plan_id, status, \
                current_period_start, current_period_end, cancelled_at, created_at \
         FROM subscriptions WHERE creator_pial_id = $1 ORDER BY created_at DESC"
    )
    .bind(&pial_id)
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn list_subscriptions(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<String>,
) -> CommerceRes<Vec<SubscriptionRow>> {
    let rows = sqlx::query_as::<_, SubscriptionRow>(
        "SELECT id, subscriber_pial_id, creator_pial_id, plan_id, status, \
                current_period_start, current_period_end, cancelled_at, created_at \
         FROM subscriptions WHERE subscriber_pial_id = $1 ORDER BY created_at DESC"
    )
    .bind(&pial_id)
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn create_ppv(
    State(state): State<Arc<AppState>>,
    Json(req): Json<CreatePpvReq>,
) -> CommerceRes<PpvItemRow> {
    let enabled: Option<(bool,)> = sqlx::query_as(
        "SELECT monetization_enabled FROM creator_eligibility WHERE pial_id = $1"
    )
    .bind(&req.creator_pial_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    if !enabled.map(|r| r.0).unwrap_or(false) {
        return Err(cerr(StatusCode::FORBIDDEN, "creator monetization not enabled"));
    }
    let price_units = aet_to_units(req.price_aet);
    if price_units <= 0 {
        return Err(cerr(StatusCode::BAD_REQUEST, "price must be greater than zero"));
    }
    let row = sqlx::query_as::<_, PpvItemRow>(
        "INSERT INTO ppv_items (creator_pial_id, content_id, price_aet, title) \
         VALUES ($1, $2, $3, $4) \
         RETURNING id, creator_pial_id, content_id, price_aet, title, is_active, created_at"
    )
    .bind(&req.creator_pial_id)
    .bind(&req.content_id)
    .bind(price_units)
    .bind(&req.title)
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(row))
}

pub async fn purchase_ppv(
    State(state): State<Arc<AppState>>,
    Path(item_id): Path<Uuid>,
    Json(req): Json<PurchasePpvReq>,
) -> CommerceRes<Value> {
    let item: Option<(String, i64)> = sqlx::query_as(
        "SELECT creator_pial_id, price_aet FROM ppv_items WHERE id = $1 AND is_active"
    )
    .bind(item_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    let (creator_pial, price_units) = item.ok_or_else(|| cerr(StatusCode::NOT_FOUND, "PPV item not found"))?;
    let existing: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM ppv_purchases WHERE buyer_pial_id = $1 AND ppv_item_id = $2"
    )
    .bind(&req.buyer_pial_id)
    .bind(item_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    if existing.is_some() {
        return Err(cerr(StatusCode::CONFLICT, "already purchased"));
    }
    let tx_id = clients::transfer(
        &state.http, &state.cfg.ain_soph_url,
        &req.buyer_pial_id, &creator_pial,
        price_units, "ppv",
    )
    .await
    .map_err(|e| cerr(StatusCode::PAYMENT_REQUIRED, &e.to_string()))?;
    let purchase_id: (Uuid,) = sqlx::query_as(
        "INSERT INTO ppv_purchases (buyer_pial_id, ppv_item_id, amount_aet, ain_soph_tx_id) \
         VALUES ($1, $2, $3, $4) RETURNING id"
    )
    .bind(&req.buyer_pial_id)
    .bind(item_id)
    .bind(price_units)
    .bind(&tx_id)
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    sqlx::query(
        "INSERT INTO commerce_transactions \
           (tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet, ain_soph_tx_id) \
         VALUES ('ppv', $1, $2, $3, $4, $5)"
    )
    .bind(purchase_id.0)
    .bind(&req.buyer_pial_id)
    .bind(&creator_pial)
    .bind(price_units)
    .bind(&tx_id)
    .execute(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({
        "purchase_id": purchase_id.0,
        "item_id":     item_id,
        "amount_aet":  units_to_aet(price_units),
        "ain_soph_tx_id": tx_id,
        "has_access":  true,
    })))
}

pub async fn check_ppv_access(
    State(state): State<Arc<AppState>>,
    AxumQuery(q): AxumQuery<CommerceAccessQuery>,
) -> CommerceRes<Value> {
    let buyer      = q.buyer.ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "buyer required"))?;
    let content_id = q.content_id.ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "content_id required"))?;
    let has: Option<(Uuid,)> = sqlx::query_as(
        "SELECT p.id FROM ppv_purchases p \
         JOIN ppv_items i ON i.id = p.ppv_item_id \
         WHERE p.buyer_pial_id = $1 AND i.content_id = $2"
    )
    .bind(&buyer)
    .bind(&content_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"has_access": has.is_some()})))
}

pub async fn list_ppv_items(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<String>,
) -> CommerceRes<Vec<PpvItemRow>> {
    let rows = sqlx::query_as::<_, PpvItemRow>(
        "SELECT id, creator_pial_id, content_id, price_aet, title, is_active, created_at \
         FROM ppv_items WHERE creator_pial_id = $1 ORDER BY created_at DESC"
    )
    .bind(&pial_id)
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn send_tip(
    State(state): State<Arc<AppState>>,
    Json(req): Json<SendTipReq>,
) -> CommerceRes<Value> {
    if req.amount_aet <= 0.0 {
        return Err(cerr(StatusCode::BAD_REQUEST, "amount must be greater than zero"));
    }
    if req.sender_pial_id == req.recipient_pial_id {
        return Err(cerr(StatusCode::BAD_REQUEST, "cannot tip yourself"));
    }
    let tx_id = clients::tip(
        &state.http, &state.cfg.ain_soph_url,
        &req.sender_pial_id, &req.recipient_pial_id,
        req.amount_aet, &req.message,
    )
    .await
    .map_err(|e| cerr(StatusCode::PAYMENT_REQUIRED, &e.to_string()))?;
    let tip_id: (Uuid,) = sqlx::query_as(
        "INSERT INTO tips \
           (sender_pial_id, recipient_pial_id, amount_aet, message, content_id, ain_soph_tx_id) \
         VALUES ($1, $2, $3, $4, $5, $6) RETURNING id"
    )
    .bind(&req.sender_pial_id)
    .bind(&req.recipient_pial_id)
    .bind(aet_to_units(req.amount_aet))
    .bind(&req.message)
    .bind(req.content_id.as_deref())
    .bind(&tx_id)
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    sqlx::query(
        "INSERT INTO commerce_transactions \
           (tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet, ain_soph_tx_id) \
         VALUES ('tip', $1, $2, $3, $4, $5)"
    )
    .bind(tip_id.0)
    .bind(&req.sender_pial_id)
    .bind(&req.recipient_pial_id)
    .bind(aet_to_units(req.amount_aet))
    .bind(&tx_id)
    .execute(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"tip_id": tip_id.0, "amount_aet": req.amount_aet, "ain_soph_tx_id": tx_id})))
}

// GET /v1/qr/:id — BIP21 QR code PNG for a purchase
pub async fn purchase_qr(
    State(state): State<Arc<AppState>>,
    Path(id): Path<Uuid>,
) -> impl IntoResponse {
    use axum::http::header;
    use image::{ImageEncoder, codecs::png::PngEncoder};
    use qrcode::QrCode;

    let row: Option<(String, i64)> = sqlx::query_as(
        "SELECT btc_address, expected_sats FROM purchases WHERE id = $1"
    )
    .bind(id)
    .fetch_optional(&state.pool)
    .await
    .unwrap_or(None);

    let (btc_address, expected_sats) = match row {
        Some(r) => r,
        None => return (StatusCode::NOT_FOUND, [(header::CONTENT_TYPE, "application/json")], b"{}".to_vec()).into_response(),
    };

    let btc_amount = expected_sats as f64 / 100_000_000.0;
    let uri = format!("bitcoin:{}?amount={:.8}", btc_address, btc_amount);

    let code = match QrCode::new(uri.as_bytes()) {
        Ok(c) => c,
        Err(e) => {
            tracing::error!("QR encode error: {e}");
            return (StatusCode::INTERNAL_SERVER_ERROR, [(header::CONTENT_TYPE, "application/json")], b"{}".to_vec()).into_response();
        }
    };

    // Render to an image with a module size of 4px and 2-module quiet zone
    let image = code.render::<image::Luma<u8>>()
        .min_dimensions(220, 220)
        .max_dimensions(440, 440)
        .build();

    let mut png_bytes: Vec<u8> = Vec::new();
    let encoder = PngEncoder::new(&mut png_bytes);
    if let Err(e) = encoder.write_image(
        image.as_raw(),
        image.width(),
        image.height(),
        image::ColorType::L8.into(),
    ) {
        tracing::error!("PNG encode error: {e}");
        return (StatusCode::INTERNAL_SERVER_ERROR, [(header::CONTENT_TYPE, "application/json")], b"{}".to_vec()).into_response();
    }

    (StatusCode::OK, [(header::CONTENT_TYPE, "image/png")], png_bytes).into_response()
}

pub async fn creator_earnings(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<String>,
) -> CommerceRes<Value> {
    let total: (i64,) = sqlx::query_as(
        "SELECT COALESCE(SUM(gross_aet), 0)::bigint FROM commerce_transactions \
         WHERE creator_pial_id = $1 AND status = 'completed'"
    )
    .bind(&pial_id)
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    let by_type: Vec<(String, i64, i64)> = sqlx::query_as(
        "SELECT tx_type, COALESCE(SUM(gross_aet), 0)::bigint, COUNT(*)::bigint \
         FROM commerce_transactions \
         WHERE creator_pial_id = $1 AND status = 'completed' GROUP BY tx_type"
    )
    .bind(&pial_id)
    .fetch_all(&state.pool)
    .await
    .unwrap_or_default();
    let mut subs_aet = 0i64;
    let mut ppv_aet  = 0i64;
    let mut tips_aet = 0i64;
    let mut tx_count = 0i64;
    for (tx_type, amount, count) in &by_type {
        tx_count += count;
        match tx_type.as_str() {
            "subscription" => subs_aet += amount,
            "ppv"          => ppv_aet  += amount,
            "tip"          => tips_aet += amount,
            _              => {}
        }
    }
    Ok(Json(json!({
        "pial_id":           pial_id,
        "total_aet":         total.0,
        "total_aet_display": units_to_aet(total.0),
        "subscriptions_aet": subs_aet,
        "ppv_aet":           ppv_aet,
        "tips_aet":          tips_aet,
        "tx_count":          tx_count,
    })))
}
