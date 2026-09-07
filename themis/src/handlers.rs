use crate::{
    db,
    models::*,
    pial_ref::{register_content, require_identity, ContentRef, IdentityError, PialRef},
    watcher, AppState,
};
use axum::{
    extract::{Path, Query, State},
    http::{HeaderMap, StatusCode},
    response::IntoResponse,
    Json,
};
use base64::{
    engine::general_purpose::{STANDARD as B64, URL_SAFE_NO_PAD as B64URL},
    Engine,
};
use manhattan_client::KIND_WORK;
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
    seller_pial: &PialRef,
    cid: &str,
    sig_b64url: &str,
) -> Result<(), ()> {
    // Fetch signing pubkey from the Elohim-veni key service (key custody only)
    let url = format!("{}/v1/pial/{}/signing-pubkey", elohim_veni_url, seller_pial);
    let resp = http
        .get(&url)
        .header("X-Internal-Key", internal_key)
        .send()
        .await
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

fn pial_from_headers(headers: &HeaderMap) -> Option<PialRef> {
    headers
        .get("X-Pial-Identity")
        .and_then(|v| v.to_str().ok())
        .and_then(|s| PialRef::parse(s).ok())
}

/// The refusal shape for a PIAL the naming plane will not vouch for.
///
/// Themis does not own identity — elohim-veni does. So an identity this brain
/// is about to pay, subscribe, or unlock content for is a name it resolved,
/// never a row it assumed. Refusing is always the safe direction here: the
/// registration is already queued durably in manhattan_outbox, so a caller that
/// retries loses nothing, while a payment sent to an identity nobody agreed on
/// cannot be taken back.
fn identity_refused(pial: &PialRef, e: IdentityError) -> (StatusCode, Json<serde_json::Value>) {
    tracing::error!("themis: identity gate refused {pial}: {e}");
    match e {
        IdentityError::NotActive | IdentityError::NotAnIdentity(_) => (
            StatusCode::FORBIDDEN,
            Json(serde_json::json!({
                "error":   "identity_not_active",
                "message": "That identity is not active."
            })),
        ),
        IdentityError::Unreachable(_) => (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(serde_json::json!({
                "error":   "identity_unresolved",
                "message": "Identity could not be resolved right now. Nothing was charged — please try again."
            })),
        ),
    }
}

fn internal_key_ok(headers: &HeaderMap, cfg_key: &str) -> bool {
    headers
        .get("X-Internal-Key")
        .and_then(|v| v.to_str().ok())
        .map(|k| !cfg_key.is_empty() && k == cfg_key)
        .unwrap_or(false)
}

// GET /health
pub async fn health(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    // The backlog is the honest signal. A drain stuck on a row it cannot
    // deliver shows up here as a number that only climbs, long before anyone
    // notices a name that fails to resolve.
    let backlog = db::manhattan_outbox_backlog(&state.pool).await;
    if let Err(ref e) = backlog {
        tracing::error!("manhattan outbox backlog query: {e}");
    }
    Json(serde_json::json!({
        "ok":        true,
        "manhattan": state.manhattan.configured(),
        "manhattan_outbox_backlog": backlog.ok(),
    }))
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
    let requester = match pial_from_headers(&headers) {
        Some(p) => p,
        None => {
            return (
                StatusCode::UNAUTHORIZED,
                Json(serde_json::json!({"error":"pial_required"})),
            )
                .into_response()
        }
    };

    // Every branch below writes the requester's PIAL into a column that decides
    // who gets paid, who owns a shop, or who a content key is unwrapped for. So
    // it is established as an identity Manhattan agrees exists before any of
    // that happens, rather than trusted because a header carried a UUID.
    if let Err(e) = require_identity(&state.manhattan, &requester).await {
        return identity_refused(&requester, e).into_response();
    }

    // Verify PIAL signature for high-value mutations.
    // shop.open uses session auth only — PIAL session already proves creator identity.
    // listing.create and listing.delete require device-bound signing (encrypted content).
    let requires_sig = matches!(
        evt.event_type.as_str(),
        "marketplace.listing.create" | "marketplace.listing.delete"
    );
    if requires_sig {
        match (&evt.cid, &evt.pial_sig) {
            (Some(cid), Some(sig)) => {
                if verify_pial_signature(
                    &state.http,
                    &state.cfg.elohim_veni_url,
                    &state.cfg.internal_api_key,
                    &requester,
                    cid,
                    sig,
                )
                .await
                .is_err()
                {
                    return (
                        StatusCode::UNAUTHORIZED,
                        Json(serde_json::json!({
                            "error": "invalid_signature",
                            "message": "PIAL signature verification failed"
                        })),
                    )
                        .into_response();
                }
            }
            _ => {
                return (
                    StatusCode::UNAUTHORIZED,
                    Json(serde_json::json!({
                        "error": "signature_required",
                        "message": "PIAL signature required for this operation"
                    })),
                )
                    .into_response();
            }
        }
    }

    match evt.event_type.as_str() {
        "marketplace.shop.open" => {
            let btc = match evt.btc_address {
                Some(ref a) if !a.is_empty() => a.clone(),
                _ => {
                    return (
                        StatusCode::BAD_REQUEST,
                        Json(serde_json::json!({"error":"btc_address required"})),
                    )
                        .into_response()
                }
            };
            if !validate_btc_address(&btc) {
                return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"invalid_btc_address","message":"That doesn't look like a valid Bitcoin address."}))).into_response();
            }
            // Reject if another active shop already claims this BTC address
            if let Ok(Some(existing)) = db::shop_get_by_btc(&state.pool, &btc).await {
                if existing.seller_pial != requester {
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
                        if existing.seller_pial != requester {
                            return (StatusCode::CONFLICT, Json(serde_json::json!({"error":"xrp_address_taken","message":"This XRP address is already registered to another shop."}))).into_response();
                        }
                    }
                    a.clone()
                }
                _ => String::new(),
            };
            match db::shop_open(&state.pool, requester, &btc, &xrp).await {
                Ok(_) => Json(serde_json::json!({"ok":true})).into_response(),
                Err(e) => {
                    tracing::error!("shop_open: {e}");
                    (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(serde_json::json!({"error":"db_error"})),
                    )
                        .into_response()
                }
            }
        }

        "marketplace.shop.close" => {
            let _ = db::shop_close(&state.pool, requester).await;
            Json(serde_json::json!({"ok":true})).into_response()
        }

        "marketplace.listing.create" => {
            let req = ListingCreateReq {
                title: evt.title.unwrap_or_default(),
                description: evt.description.unwrap_or_default(),
                price_sats: evt.price_sats.unwrap_or(0),
                price_xrp_drops: evt.price_xrp_drops,
                payment_currency: evt.payment_currency.clone(),
                content_url: evt.content_url.unwrap_or_default(),
                content_hash: evt.content_hash.unwrap_or_default(),
                cek_encrypted: evt.cek_encrypted.unwrap_or_default(),
                phash: evt.phash,
                visibility: evt.visibility,
            };
            if req.title.is_empty() || req.content_url.is_empty() || req.cek_encrypted.is_empty() {
                return (
                    StatusCode::BAD_REQUEST,
                    Json(serde_json::json!({"error":"missing fields"})),
                )
                    .into_response();
            }
            if req.price_sats < 1000 {
                return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"price_too_low","message":"Minimum price is 1,000 sats"}))).into_response();
            }
            let cek_bytes = match B64.decode(&req.cek_encrypted) {
                Ok(b) => b,
                Err(_) => {
                    return (
                        StatusCode::BAD_REQUEST,
                        Json(serde_json::json!({"error":"invalid cek_encrypted"})),
                    )
                        .into_response()
                }
            };
            // Count active listings — max 12
            match db::listings_for_shop(&state.pool, requester, true).await {
                Ok(existing) if existing.len() >= 12 => {
                    return (
                        StatusCode::BAD_REQUEST,
                        Json(serde_json::json!({"error":"max_listings_reached"})),
                    )
                        .into_response();
                }
                _ => {}
            }
            match db::listing_create(&state.pool, requester, &req, cek_bytes).await {
                Ok(l) => {
                    if let Some(content) = ContentRef::parse(&l.content_hash) {
                        register_content(&state.manhattan, KIND_WORK, &content).await;
                    }
                    Json(serde_json::json!({"ok":true, "listing_id": l.id})).into_response()
                }
                Err(e) => {
                    tracing::error!("listing_create: {e}");
                    (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(serde_json::json!({"error":"db_error"})),
                    )
                        .into_response()
                }
            }
        }

        "marketplace.listing.edit" => {
            let listing_id = match evt.listing_id {
                Some(id) => id,
                None => {
                    return (
                        StatusCode::BAD_REQUEST,
                        Json(serde_json::json!({"error":"listing_id required"})),
                    )
                        .into_response()
                }
            };
            // Verify ownership
            match db::listing_get(&state.pool, listing_id).await {
                Ok(Some(l)) if l.shop_pial == requester => {}
                Ok(Some(_)) => {
                    return (
                        StatusCode::FORBIDDEN,
                        Json(serde_json::json!({"error":"forbidden"})),
                    )
                        .into_response()
                }
                Ok(None) => {
                    return (
                        StatusCode::NOT_FOUND,
                        Json(serde_json::json!({"error":"not_found"})),
                    )
                        .into_response()
                }
                Err(e) => {
                    tracing::error!("{e}");
                    return (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(serde_json::json!({"error":"db_error"})),
                    )
                        .into_response();
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
                None => {
                    return (
                        StatusCode::BAD_REQUEST,
                        Json(serde_json::json!({"error":"listing_id required"})),
                    )
                        .into_response()
                }
            };
            match db::listing_get(&state.pool, listing_id).await {
                Ok(Some(l)) if l.shop_pial == requester => {
                    let _ = db::listing_set_active(&state.pool, listing_id, false).await;
                    Json(serde_json::json!({"ok":true})).into_response()
                }
                Ok(Some(_)) => (
                    StatusCode::FORBIDDEN,
                    Json(serde_json::json!({"error":"forbidden"})),
                )
                    .into_response(),
                _ => (
                    StatusCode::NOT_FOUND,
                    Json(serde_json::json!({"error":"not_found"})),
                )
                    .into_response(),
            }
        }

        "marketplace.purchase.initiate" => {
            let listing_id = match evt.listing_id {
                Some(id) => id,
                None => {
                    return (
                        StatusCode::BAD_REQUEST,
                        Json(serde_json::json!({"error":"listing_id required"})),
                    )
                        .into_response()
                }
            };
            let listing = match db::listing_get(&state.pool, listing_id).await {
                Ok(Some(l)) if l.is_active => l,
                Ok(_) => {
                    return (
                        StatusCode::NOT_FOUND,
                        Json(serde_json::json!({"error":"listing_not_found"})),
                    )
                        .into_response()
                }
                Err(e) => {
                    tracing::error!("{e}");
                    return (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(serde_json::json!({"error":"db_error"})),
                    )
                        .into_response();
                }
            };
            // Check if already purchased
            if let Ok(Some(_)) =
                db::purchase_get_by_buyer_and_listing(&state.pool, requester, listing_id).await
            {
                return (
                    StatusCode::CONFLICT,
                    Json(serde_json::json!({"error":"already_purchased"})),
                )
                    .into_response();
            }
            // Get seller's shop
            let shop = match db::shop_get(&state.pool, listing.shop_pial).await {
                Ok(Some(s)) if s.is_active => s,
                _ => {
                    return (
                        StatusCode::SERVICE_UNAVAILABLE,
                        Json(serde_json::json!({"error":"shop_unavailable"})),
                    )
                        .into_response()
                }
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
                    &state.pool,
                    listing_id,
                    requester,
                    "", // btc_address unused for XRP
                    drops,
                    "xrp",
                    &shop.xrp_address,
                )
                .await
                {
                    Ok(p) => p,
                    Err(e) => {
                        tracing::error!("{e}");
                        return (
                            StatusCode::INTERNAL_SERVER_ERROR,
                            Json(serde_json::json!({"error":"db_error"})),
                        )
                            .into_response();
                    }
                };
                // Subscribe XRP address in watcher
                watcher::subscribe_xrp_address(
                    &state.xrp_watch_map,
                    watcher::XrpWatchEntry {
                        purchase_id: purchase.id,
                        buyer_pial: requester,
                        listing_id,
                        expected_drops: drops,
                        xrp_address: shop.xrp_address.clone(),
                    },
                )
                .await;

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
                }))
                .into_response()
            } else {
                // BTC payment path (default)
                let purchase = match db::purchase_create(
                    &state.pool,
                    listing_id,
                    requester,
                    &shop.btc_address,
                    listing.price_sats,
                    "btc",
                    "",
                )
                .await
                {
                    Ok(p) => p,
                    Err(e) => {
                        tracing::error!("{e}");
                        return (
                            StatusCode::INTERNAL_SERVER_ERROR,
                            Json(serde_json::json!({"error":"db_error"})),
                        )
                            .into_response();
                    }
                };
                // Subscribe BTC address in watcher
                watcher::subscribe_address(
                    &state.watch_map,
                    watcher::WatchEntry {
                        purchase_id: purchase.id,
                        buyer_pial: requester, // buyer is the requesting PIAL
                        listing_id,
                        expected_sats: listing.price_sats,
                        btc_address: shop.btc_address.clone(),
                        confirmations: 0,
                    },
                )
                .await;

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
                }))
                .into_response()
            }
        }

        "marketplace.subscription.create" => {
            let sp = match evt.seller_pial {
                Some(p) => p,
                None => {
                    return (
                        StatusCode::BAD_REQUEST,
                        Json(serde_json::json!({"error":"seller_pial required"})),
                    )
                        .into_response()
                }
            };
            if let Err(e) = require_identity(&state.manhattan, &sp).await {
                return identity_refused(&sp, e).into_response();
            }
            match db::marketplace_subscription_create(&state.pool, requester, sp).await {
                Ok(_) => Json(serde_json::json!({"ok":true})).into_response(),
                Err(e) => {
                    tracing::error!("{e}");
                    (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(serde_json::json!({"error":"db_error"})),
                    )
                        .into_response()
                }
            }
        }

        "marketplace.subscription.cancel" => {
            let sp = match evt.seller_pial {
                Some(p) => p,
                None => {
                    return (
                        StatusCode::BAD_REQUEST,
                        Json(serde_json::json!({"error":"seller_pial required"})),
                    )
                        .into_response()
                }
            };
            let _ = db::marketplace_subscription_cancel(&state.pool, requester, sp).await;
            Json(serde_json::json!({"ok":true})).into_response()
        }

        _ => (
            StatusCode::BAD_REQUEST,
            Json(serde_json::json!({"error":"unknown_event_type"})),
        )
            .into_response(),
    }
}

// GET /v1/shop/:seller_pial — public shop listings
pub async fn shop_listings(
    State(state): State<Arc<AppState>>,
    Path(seller_pial): Path<PialRef>,
    headers: HeaderMap,
) -> impl IntoResponse {
    let requester = pial_from_headers(&headers);
    let is_owner = requester == Some(seller_pial);
    let include_private = if let Some(buyer) = requester {
        is_owner
            || db::marketplace_subscription_active(&state.pool, buyer, seller_pial)
                .await
                .unwrap_or(false)
    } else {
        false
    };

    match db::listings_for_shop(&state.pool, seller_pial, include_private).await {
        Ok(listings) => Json(serde_json::json!({"listings": listings})).into_response(),
        Err(e) => {
            tracing::error!("{e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// GET /v1/marketplace — public feed of all listings
#[derive(Deserialize)]
pub struct FeedQuery {
    pub limit: Option<i64>,
    pub offset: Option<i64>,
}

pub async fn marketplace_feed(
    State(state): State<Arc<AppState>>,
    Query(q): Query<FeedQuery>,
) -> impl IntoResponse {
    let limit = q.limit.unwrap_or(20).min(50);
    let offset = q.offset.unwrap_or(0);
    match db::listings_public(&state.pool, limit, offset).await {
        Ok(listings) => Json(serde_json::json!({"listings": listings})).into_response(),
        Err(e) => {
            tracing::error!("{e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error":"db_error"})),
            )
                .into_response()
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
                return (
                    StatusCode::FORBIDDEN,
                    Json(serde_json::json!({"error":"forbidden"})),
                )
                    .into_response();
            }
            Json(serde_json::json!({
                "id":            p.id,
                "status":        p.status,
                "btc_address":   p.btc_address,
                "amount_sats":   p.expected_sats,
                "expires_at":    p.expires_at,
                "cek_for_buyer": p.cek_for_buyer.map(|b| B64.encode(&b)),
            }))
            .into_response()
        }
        Ok(None) => (
            StatusCode::NOT_FOUND,
            Json(serde_json::json!({"error":"not_found"})),
        )
            .into_response(),
        Err(e) => {
            tracing::error!("{e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error":"db_error"})),
            )
                .into_response()
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
        None => {
            return (
                StatusCode::UNAUTHORIZED,
                Json(serde_json::json!({"error":"unauthorized"})),
            )
                .into_response()
        }
    };
    match db::purchases_by_buyer(&state.pool, buyer).await {
        Ok(purchases) => Json(serde_json::json!({"purchases": purchases})).into_response(),
        Err(e) => {
            tracing::error!("{e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error":"db_error"})),
            )
                .into_response()
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
    if let Ok(Some(_)) =
        db::purchase_get_by_buyer_and_listing(&state.pool, buyer, q.listing_id).await
    {
        return Json(serde_json::json!({"has_access": true})).into_response();
    }
    // Subscription check for subscribers_only listings
    if listing.visibility == "subscribers_only" {
        let active = db::marketplace_subscription_active(&state.pool, buyer, listing.shop_pial)
            .await
            .unwrap_or(false);
        return Json(serde_json::json!({"has_access": active})).into_response();
    }
    Json(serde_json::json!({"has_access": false})).into_response()
}

// GET /v1/shop/:seller_pial/status — check if a shop exists and is open
pub async fn shop_status(
    State(state): State<Arc<AppState>>,
    Path(seller_pial): Path<PialRef>,
) -> impl IntoResponse {
    match db::shop_get(&state.pool, seller_pial).await {
        Ok(Some(shop)) => Json(serde_json::json!({
            "exists":      true,
            "is_active":   shop.is_active,
            "btc_address": shop.btc_address,
            "xrp_address": shop.xrp_address,
        }))
        .into_response(),
        Ok(None) => Json(serde_json::json!({
            "exists":      false,
            "is_active":   false,
            "btc_address": "",
            "xrp_address": "",
        }))
        .into_response(),
        Err(e) => {
            tracing::error!("shop_status: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// GET /v1/seller/:seller_pial/stats — seller's own sales stats
pub async fn seller_stats(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(seller_pial): Path<PialRef>,
) -> impl IntoResponse {
    // Only the seller themselves can see their stats
    let requester = match pial_from_headers(&headers) {
        Some(p) => p,
        None => {
            return (
                StatusCode::UNAUTHORIZED,
                Json(serde_json::json!({"error":"unauthorized"})),
            )
                .into_response()
        }
    };
    if requester != seller_pial {
        return (
            StatusCode::FORBIDDEN,
            Json(serde_json::json!({"error":"forbidden"})),
        )
            .into_response();
    }
    match db::seller_stats(&state.pool, seller_pial).await {
        Ok((sales, revenue_sats, pending)) => Json(serde_json::json!({
            "sales":        sales,
            "revenue_sats": revenue_sats,
            "pending":      pending,
        }))
        .into_response(),
        Err(e) => {
            tracing::error!("seller_stats: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(serde_json::json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── Commerce handlers (from Thessalon) ────────────────────────────────────────

// ── Payment intent: the record exists before the money moves ──────────────────
//
// `subscribe`, `purchase_ppv` and `send_tip` used to call Ain Soph first and
// INSERT afterwards. A database failure in that window moved a user's money and
// left no local record of it whatsoever — nothing to reconcile against, nothing
// to refund from, and no way to discover it had happened at all. The user was
// simply poorer.
//
// The order is inverted here: record the intent, move the money, settle the
// record. The asymmetry is the whole argument — an abandoned `pending` row costs
// nothing and is visible, while a transfer with no row costs someone their
// balance and is invisible. When those are the two failure modes, you choose the
// one you can see.
//
// A crash between the transfer and the settlement still leaves a `pending` row.
// That is not a silent loss: the row names the payer, the payee, the amount and
// the moment, so it can be reconciled against Ain Soph by hand or by a sweep.
// idx_ctx_pending exists to make that set cheap to find.

/// Writes the ledger row for a payment that has not happened yet.
///
/// `reference_id` is chosen by the caller before either row exists, so the
/// domain row and its ledger entry can be inserted together in one transaction
/// and point at each other without a round trip.
async fn record_intent(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    tx_type: &str,
    reference_id: Uuid,
    payer: &PialRef,
    payee: &PialRef,
    gross_units: i64,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "INSERT INTO commerce_transactions \
           (tx_type, reference_id, payer_pial_id, creator_pial_id, gross_aet, status) \
         VALUES ($1, $2, $3, $4, $5, 'pending')",
    )
    .bind(tx_type)
    .bind(reference_id)
    .bind(payer.text())
    .bind(payee.text())
    .bind(gross_units)
    .execute(&mut **tx)
    .await
    .map(|_| ())
}

/// Marks a recorded payment settled, in the ledger and in its domain table.
///
/// Both rows move together or neither does, so the ledger can never disagree
/// with the thing it is the ledger for. `domain_table` and `domain_status` are
/// passed rather than hardcoded because a subscription settles to 'active' while
/// a tip settles to 'completed' — the same event, different vocabulary per table.
///
/// `domain_table` is interpolated into SQL, so it is `&'static str`: only a
/// literal written in this crate can be passed, never anything a request
/// carried. The type is the guarantee, not the callers' discipline.
async fn settle_intent(
    pool: &SqPool,
    domain_table: &'static str,
    domain_status: &str,
    reference_id: Uuid,
    outcome: Result<&str, &str>,
) -> Result<(), sqlx::Error> {
    let (ledger_status, domain_state, tx_id, reason) = match outcome {
        Ok(tx_id) => ("completed", domain_status, Some(tx_id), None),
        Err(why) => ("failed", "failed", None, Some(why)),
    };

    // Only a row still pending may be settled. Without this, a settlement landing
    // after something else moved the row — a cancellation, a sweep — would
    // silently overwrite that decision and resurrect the payment.
    let mut tx = pool.begin().await?;
    sqlx::query(&format!(
        "UPDATE {domain_table} SET status = $2, ain_soph_tx_id = $3, failure_reason = $4 \
          WHERE id = $1 AND status = 'pending'"
    ))
    .bind(reference_id)
    .bind(domain_state)
    .bind(tx_id)
    .bind(reason)
    .execute(&mut *tx)
    .await?;
    sqlx::query(
        "UPDATE commerce_transactions \
            SET status = $2, ain_soph_tx_id = $3, failure_reason = $4 \
          WHERE reference_id = $1 AND status = 'pending'",
    )
    .bind(reference_id)
    .bind(ledger_status)
    .bind(tx_id)
    .bind(reason)
    .execute(&mut *tx)
    .await?;
    tx.commit().await
}

/// Settlement failed to record even though the transfer succeeded. The money has
/// moved and the row is still `pending`, which is exactly the state the pending
/// row exists to make visible. Nothing here can fix it, so it says so loudly and
/// names everything needed to reconcile it.
fn settlement_unrecorded(
    reference_id: Uuid,
    tx_id: &str,
    e: sqlx::Error,
) -> (StatusCode, Json<Value>) {
    tracing::error!(
        reference_id = %reference_id,
        ain_soph_tx_id = %tx_id,
        "INCIDENT: transfer succeeded but settlement could not be recorded — \
         the row is still pending and needs reconciling by hand: {e}"
    );
    cerr(
        StatusCode::INTERNAL_SERVER_ERROR,
        "payment went through but could not be recorded — it has been logged for reconciliation, do not retry",
    )
}

/// Establishes that the caller IS the PIAL it claims to be spending or acting as.
///
/// `require_identity` proves a PIAL *exists*. It cannot prove the caller *is*
/// that PIAL — and every commerce endpoint below used to take the payer straight
/// out of the request body. Anyone able to reach this port could therefore spend
/// any PIAL's balance, open a shop as anyone, or enable monetisation for anyone,
/// simply by naming them. Existence was being checked; authority was not.
///
/// The authenticated identity arrives in `X-Pial-Identity`, which feed-engine
/// sets from the session after authenticating it and strips the inbound `Cookie`
/// for, so the header cannot be forged by a browser. This is the same source the
/// `/v1/events` handler has always used; the commerce handlers simply never
/// consulted it.
///
/// `X-Internal-Key` is the service-to-service path — a chain watcher confirming
/// a settlement acts for a buyer who is not making the call. It is honoured only
/// when a key is actually configured, so an empty `INTERNAL_API_KEY` cannot turn
/// into a blanket bypass.
///
/// Deny is the default: no header, no action.
fn require_caller(
    headers: &HeaderMap,
    cfg_key: &str,
    acting_as: &PialRef,
) -> Result<(), (StatusCode, Json<Value>)> {
    if internal_key_ok(headers, cfg_key) {
        return Ok(());
    }
    match pial_from_headers(headers) {
        None => Err(cerr(
            StatusCode::UNAUTHORIZED,
            "X-Pial-Identity is required: this endpoint spends or acts for an identity",
        )),
        Some(caller) if caller == *acting_as => Ok(()),
        Some(caller) => {
            tracing::warn!(
                caller = %caller.text(),
                claimed = %acting_as.text(),
                "refused: caller tried to act as a different identity"
            );
            Err(cerr(
                StatusCode::FORBIDDEN,
                "you may only act as your own identity",
            ))
        }
    }
}

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
    let creators: (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM creator_eligibility WHERE monetization_enabled")
            .fetch_one(pool)
            .await
            .unwrap_or((0,));
    let active_subs: (i64,) =
        sqlx::query_as("SELECT COUNT(*) FROM subscriptions WHERE status = 'active'")
            .fetch_one(pool)
            .await
            .unwrap_or((0,));
    let tx_count: (i64,) = sqlx::query_as("SELECT COUNT(*) FROM commerce_transactions")
        .fetch_one(pool)
        .await
        .unwrap_or((0,));
    let volume: (Option<i64>,) = sqlx::query_as(
        "SELECT SUM(gross_aet) FROM commerce_transactions WHERE status = 'completed'",
    )
    .fetch_one(pool)
    .await
    .unwrap_or((None,));
    Json(json!({
        "active_creators": creators.0,
        "active_subscriptions": active_subs.0,
        "total_transactions": tx_count.0,
        "total_volume_aet": volume.0.unwrap_or(0),
    }))
}

pub async fn creator_enable(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<EnableCreatorReq>,
) -> CommerceRes<Value> {
    require_caller(&headers, &state.cfg.internal_api_key, &req.pial_id)?;
    // Enabling monetization is the moment a PIAL becomes payable by this brain.
    require_identity(&state.manhattan, &req.pial_id)
        .await
        .map_err(|e| identity_refused(&req.pial_id, e))?;
    let tier = clients::kyc_tier(&state.http, &state.cfg.verity_url, &req.pial_id.text()).await;
    sqlx::query(
        "INSERT INTO creator_eligibility (pial_id, kyc_tier, monetization_enabled, enabled_at) \
         VALUES ($1, $2, true, NOW()) \
         ON CONFLICT (pial_id) DO UPDATE \
         SET kyc_tier = $2, monetization_enabled = true, enabled_at = NOW(), \
             disabled_at = NULL, disabled_reason = NULL",
    )
    .bind(req.pial_id.text())
    .bind(tier)
    .execute(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(
        json!({"pial_id": req.pial_id, "monetization_enabled": true, "kyc_tier": tier}),
    ))
}

pub async fn creator_status(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Value> {
    let row: Option<(bool, i32)> = sqlx::query_as(
        "SELECT monetization_enabled, kyc_tier FROM creator_eligibility WHERE pial_id = $1",
    )
    .bind(pial_id.text())
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    let (enabled, tier) = row.unwrap_or((false, 0));
    Ok(Json(
        json!({"pial_id": pial_id, "monetization_enabled": enabled, "kyc_tier": tier}),
    ))
}

pub async fn list_plans(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Vec<PlanRow>> {
    let rows = sqlx::query_as::<_, PlanRow>(
        "SELECT id, creator_pial_id, name, price_aet, description, is_active, created_at \
         FROM subscription_plans WHERE creator_pial_id = $1 ORDER BY price_aet ASC",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn create_plan(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<CreatePlanReq>,
) -> CommerceRes<PlanRow> {
    require_caller(&headers, &state.cfg.internal_api_key, &req.creator_pial_id)?;
    require_identity(&state.manhattan, &req.creator_pial_id)
        .await
        .map_err(|e| identity_refused(&req.creator_pial_id, e))?;
    let enabled: Option<(bool,)> =
        sqlx::query_as("SELECT monetization_enabled FROM creator_eligibility WHERE pial_id = $1")
            .bind(req.creator_pial_id.text())
            .fetch_optional(&state.pool)
            .await
            .map_err(cdberr)?;
    if !enabled.map(|r| r.0).unwrap_or(false) {
        return Err(cerr(
            StatusCode::FORBIDDEN,
            "creator monetization not enabled",
        ));
    }
    let price_units = aet_to_units(req.price_aet);
    if price_units <= 0 {
        return Err(cerr(
            StatusCode::BAD_REQUEST,
            "price must be greater than zero",
        ));
    }
    let row = sqlx::query_as::<_, PlanRow>(
        "INSERT INTO subscription_plans (creator_pial_id, name, price_aet, description) \
         VALUES ($1, $2, $3, $4) \
         RETURNING id, creator_pial_id, name, price_aet, description, is_active, created_at",
    )
    .bind(req.creator_pial_id.text())
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
         RETURNING id, creator_pial_id, name, price_aet, description, is_active, created_at",
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
    headers: HeaderMap,
    Json(req): Json<SubscribeReq>,
) -> CommerceRes<Value> {
    require_caller(
        &headers,
        &state.cfg.internal_api_key,
        &req.subscriber_pial_id,
    )?;
    // Both ends of a recurring payment, resolved before either is written into
    // a row that decides where the money goes.
    require_identity(&state.manhattan, &req.subscriber_pial_id)
        .await
        .map_err(|e| identity_refused(&req.subscriber_pial_id, e))?;
    require_identity(&state.manhattan, &req.creator_pial_id)
        .await
        .map_err(|e| identity_refused(&req.creator_pial_id, e))?;
    let existing: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM subscriptions \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 \
           AND status IN ('active', 'pending')",
    )
    .bind(req.subscriber_pial_id.text())
    .bind(req.creator_pial_id.text())
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    if existing.is_some() {
        return Err(cerr(StatusCode::CONFLICT, "already subscribed"));
    }
    let plan: Option<(i64,)> =
        sqlx::query_as("SELECT price_aet FROM subscription_plans WHERE id = $1 AND is_active")
            .bind(req.plan_id)
            .fetch_optional(&state.pool)
            .await
            .map_err(cdberr)?;
    let price_units = plan
        .ok_or_else(|| cerr(StatusCode::NOT_FOUND, "plan not found or inactive"))?
        .0;
    // Record first, charge second, settle third. See record_intent. A
    // subscription settles to 'active' rather than 'completed' — same event,
    // different vocabulary in that table.
    let period_end = chrono::Utc::now() + chrono::Duration::days(30);
    let sub_id = Uuid::new_v4();
    let mut itx = state.pool.begin().await.map_err(cdberr)?;
    sqlx::query(
        "INSERT INTO subscriptions \
           (id, subscriber_pial_id, creator_pial_id, plan_id, current_period_end, status) \
         VALUES ($1, $2, $3, $4, $5, 'pending')",
    )
    .bind(sub_id)
    .bind(req.subscriber_pial_id.text())
    .bind(req.creator_pial_id.text())
    .bind(req.plan_id)
    .bind(period_end)
    .execute(&mut *itx)
    .await
    .map_err(cdberr)?;
    record_intent(
        &mut itx,
        "subscription",
        sub_id,
        &req.subscriber_pial_id,
        &req.creator_pial_id,
        price_units,
    )
    .await
    .map_err(cdberr)?;
    itx.commit().await.map_err(cdberr)?;

    let transfer = clients::transfer(
        &state.http,
        &state.cfg.ain_soph_url,
        &state.cfg.internal_api_key,
        &req.subscriber_pial_id.text(),
        &req.creator_pial_id.text(),
        price_units,
        "subscription",
    )
    .await;

    let tx_id = match transfer {
        Ok(tx_id) => {
            settle_intent(&state.pool, "subscriptions", "active", sub_id, Ok(&tx_id))
                .await
                .map_err(|e| settlement_unrecorded(sub_id, &tx_id, e))?;
            tx_id
        }
        Err(e) => {
            let why = e.to_string();
            let _ = settle_intent(&state.pool, "subscriptions", "active", sub_id, Err(&why)).await;
            return Err(cerr(StatusCode::PAYMENT_REQUIRED, &why));
        }
    };
    Ok(Json(json!({
        "subscription_id": sub_id,
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
    // Cancels only an active subscription — deliberately NOT a pending one.
    //
    // A pending row is one whose payment is in flight. Cancelling it would race
    // the settlement: either the settlement overwrites the cancellation and the
    // subscriber is charged for something they just cancelled, or the
    // cancellation wins and the transfer has already moved money for a
    // subscription that no longer exists. Neither is recoverable from here.
    //
    // The window is one HTTP call wide, so the honest answer is to say what is
    // actually happening and let the caller retry a moment later. Returning
    // "no active subscription found" — which is what this did once pending
    // existed — would tell the subscriber they were not subscribed while their
    // payment was mid-flight.
    let result = sqlx::query(
        "UPDATE subscriptions SET status = 'cancelled', cancelled_at = NOW() \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 AND status = 'active'",
    )
    .bind(req.subscriber_pial_id.text())
    .bind(req.creator_pial_id.text())
    .execute(&state.pool)
    .await
    .map_err(cdberr)?;
    if result.rows_affected() == 0 {
        let pending: Option<(Uuid,)> = sqlx::query_as(
            "SELECT id FROM subscriptions \
             WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 AND status = 'pending'",
        )
        .bind(req.subscriber_pial_id.text())
        .bind(req.creator_pial_id.text())
        .fetch_optional(&state.pool)
        .await
        .map_err(cdberr)?;
        if pending.is_some() {
            return Err(cerr(
                StatusCode::CONFLICT,
                "this subscription's payment is still settling — try again in a moment",
            ));
        }
        return Err(cerr(StatusCode::NOT_FOUND, "no active subscription found"));
    }
    Ok(Json(json!({"status": "cancelled"})))
}

pub async fn check_subscription(
    State(state): State<Arc<AppState>>,
    AxumQuery(q): AxumQuery<CommerceAccessQuery>,
) -> CommerceRes<Value> {
    let subscriber = q
        .subscriber
        .ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "subscriber required"))?;
    let creator = q
        .creator
        .ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "creator required"))?;
    let has: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM subscriptions \
         WHERE subscriber_pial_id = $1 AND creator_pial_id = $2 \
           AND status = 'active' AND current_period_end > NOW()",
    )
    .bind(subscriber.text())
    .bind(creator.text())
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"has_access": has.is_some()})))
}

pub async fn list_subscribers(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Vec<SubscriptionRow>> {
    let rows = sqlx::query_as::<_, SubscriptionRow>(
        "SELECT id, subscriber_pial_id, creator_pial_id, plan_id, status, \
                current_period_start, current_period_end, cancelled_at, created_at \
         FROM subscriptions \
          WHERE creator_pial_id = $1 AND status <> 'failed' \
          ORDER BY created_at DESC",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn list_subscriptions(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Vec<SubscriptionRow>> {
    let rows = sqlx::query_as::<_, SubscriptionRow>(
        "SELECT id, subscriber_pial_id, creator_pial_id, plan_id, status, \
                current_period_start, current_period_end, cancelled_at, created_at \
         FROM subscriptions \
          WHERE subscriber_pial_id = $1 AND status <> 'failed' \
          ORDER BY created_at DESC",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn create_ppv(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<CreatePpvReq>,
) -> CommerceRes<PpvItemRow> {
    require_caller(&headers, &state.cfg.internal_api_key, &req.creator_pial_id)?;
    require_identity(&state.manhattan, &req.creator_pial_id)
        .await
        .map_err(|e| identity_refused(&req.creator_pial_id, e))?;
    // The item being sold is a work owned by feed-engine, referenced here by
    // name. Themis registers the name it must reference and resolves it; it
    // never assumes another brain's id scheme.
    if let Some(content) = ContentRef::parse(&req.content_id) {
        register_content(&state.manhattan, KIND_WORK, &content).await;
    }
    let enabled: Option<(bool,)> =
        sqlx::query_as("SELECT monetization_enabled FROM creator_eligibility WHERE pial_id = $1")
            .bind(req.creator_pial_id.text())
            .fetch_optional(&state.pool)
            .await
            .map_err(cdberr)?;
    if !enabled.map(|r| r.0).unwrap_or(false) {
        return Err(cerr(
            StatusCode::FORBIDDEN,
            "creator monetization not enabled",
        ));
    }
    let price_units = aet_to_units(req.price_aet);
    if price_units <= 0 {
        return Err(cerr(
            StatusCode::BAD_REQUEST,
            "price must be greater than zero",
        ));
    }
    let row = sqlx::query_as::<_, PpvItemRow>(
        "INSERT INTO ppv_items (creator_pial_id, content_id, price_aet, title) \
         VALUES ($1, $2, $3, $4) \
         RETURNING id, creator_pial_id, content_id, price_aet, title, is_active, created_at",
    )
    .bind(req.creator_pial_id.text())
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
    headers: HeaderMap,
    Json(req): Json<PurchasePpvReq>,
) -> CommerceRes<Value> {
    require_caller(&headers, &state.cfg.internal_api_key, &req.buyer_pial_id)?;
    require_identity(&state.manhattan, &req.buyer_pial_id)
        .await
        .map_err(|e| identity_refused(&req.buyer_pial_id, e))?;
    let item: Option<(PialRef, i64)> = sqlx::query_as(
        "SELECT creator_pial_id, price_aet FROM ppv_items WHERE id = $1 AND is_active",
    )
    .bind(item_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    let (creator_pial, price_units) =
        item.ok_or_else(|| cerr(StatusCode::NOT_FOUND, "PPV item not found"))?;
    require_identity(&state.manhattan, &creator_pial)
        .await
        .map_err(|e| identity_refused(&creator_pial, e))?;
    // A purchase already paid for, or one whose payment is in flight, blocks a
    // second charge. A FAILED one must not — otherwise one declined payment
    // locks the buyer out of ever buying that item again. This mirrors the
    // partial unique index uq_ppv_purchase_live.
    let existing: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM ppv_purchases \
          WHERE buyer_pial_id = $1 AND ppv_item_id = $2 AND status <> 'failed'",
    )
    .bind(req.buyer_pial_id.text())
    .bind(item_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    if existing.is_some() {
        return Err(cerr(StatusCode::CONFLICT, "already purchased"));
    }
    // Record first, charge second, settle third. See record_intent.
    let purchase_id = Uuid::new_v4();
    let mut itx = state.pool.begin().await.map_err(cdberr)?;
    sqlx::query(
        "INSERT INTO ppv_purchases (id, buyer_pial_id, ppv_item_id, amount_aet, status) \
         VALUES ($1, $2, $3, $4, 'pending')",
    )
    .bind(purchase_id)
    .bind(req.buyer_pial_id.text())
    .bind(item_id)
    .bind(price_units)
    .execute(&mut *itx)
    .await
    .map_err(cdberr)?;
    record_intent(
        &mut itx,
        "ppv",
        purchase_id,
        &req.buyer_pial_id,
        &creator_pial,
        price_units,
    )
    .await
    .map_err(cdberr)?;
    itx.commit().await.map_err(cdberr)?;

    let transfer = clients::transfer(
        &state.http,
        &state.cfg.ain_soph_url,
        &state.cfg.internal_api_key,
        &req.buyer_pial_id.text(),
        &creator_pial.text(),
        price_units,
        "ppv",
    )
    .await;

    let tx_id = match transfer {
        Ok(tx_id) => {
            settle_intent(
                &state.pool,
                "ppv_purchases",
                "completed",
                purchase_id,
                Ok(&tx_id),
            )
            .await
            .map_err(|e| settlement_unrecorded(purchase_id, &tx_id, e))?;
            tx_id
        }
        Err(e) => {
            let why = e.to_string();
            let _ = settle_intent(
                &state.pool,
                "ppv_purchases",
                "completed",
                purchase_id,
                Err(&why),
            )
            .await;
            return Err(cerr(StatusCode::PAYMENT_REQUIRED, &why));
        }
    };
    Ok(Json(json!({
        "purchase_id": purchase_id,
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
    let buyer = q
        .buyer
        .ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "buyer required"))?;
    let content_id = q
        .content_id
        .ok_or_else(|| cerr(StatusCode::BAD_REQUEST, "content_id required"))?;
    // Only a completed purchase unlocks content. The status filter is the whole
    // check: a purchase row is now written BEFORE the transfer (see
    // record_intent), so without it a buyer would gain access the instant they
    // pressed buy — and keep it if the payment then failed.
    let has: Option<(Uuid,)> = sqlx::query_as(
        "SELECT p.id FROM ppv_purchases p \
         JOIN ppv_items i ON i.id = p.ppv_item_id \
         WHERE p.buyer_pial_id = $1 AND i.content_id = $2 \
           AND p.status = 'completed'",
    )
    .bind(buyer.text())
    .bind(&content_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(json!({"has_access": has.is_some()})))
}

pub async fn list_ppv_items(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Vec<PpvItemRow>> {
    let rows = sqlx::query_as::<_, PpvItemRow>(
        "SELECT id, creator_pial_id, content_id, price_aet, title, is_active, created_at \
         FROM ppv_items WHERE creator_pial_id = $1 ORDER BY created_at DESC",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .map_err(cdberr)?;
    Ok(Json(rows))
}

pub async fn send_tip(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<SendTipReq>,
) -> CommerceRes<Value> {
    require_caller(&headers, &state.cfg.internal_api_key, &req.sender_pial_id)?;
    if req.amount_aet <= 0.0 {
        return Err(cerr(
            StatusCode::BAD_REQUEST,
            "amount must be greater than zero",
        ));
    }
    if req.sender_pial_id == req.recipient_pial_id {
        return Err(cerr(StatusCode::BAD_REQUEST, "cannot tip yourself"));
    }
    require_identity(&state.manhattan, &req.sender_pial_id)
        .await
        .map_err(|e| identity_refused(&req.sender_pial_id, e))?;
    require_identity(&state.manhattan, &req.recipient_pial_id)
        .await
        .map_err(|e| identity_refused(&req.recipient_pial_id, e))?;
    if let Some(content) = req.content_id.as_deref().and_then(ContentRef::parse) {
        register_content(&state.manhattan, KIND_WORK, &content).await;
    }
    // Record first, charge second, settle third. See record_intent.
    let tip_id = Uuid::new_v4();
    let units = aet_to_units(req.amount_aet);
    let mut itx = state.pool.begin().await.map_err(cdberr)?;
    sqlx::query(
        "INSERT INTO tips \
           (id, sender_pial_id, recipient_pial_id, amount_aet, message, content_id, status) \
         VALUES ($1, $2, $3, $4, $5, $6, 'pending')",
    )
    .bind(tip_id)
    .bind(req.sender_pial_id.text())
    .bind(req.recipient_pial_id.text())
    .bind(units)
    .bind(&req.message)
    .bind(req.content_id.as_deref())
    .execute(&mut *itx)
    .await
    .map_err(cdberr)?;
    record_intent(
        &mut itx,
        "tip",
        tip_id,
        &req.sender_pial_id,
        &req.recipient_pial_id,
        units,
    )
    .await
    .map_err(cdberr)?;
    itx.commit().await.map_err(cdberr)?;

    let transfer = clients::tip(
        &state.http,
        &state.cfg.ain_soph_url,
        &state.cfg.internal_api_key,
        &req.sender_pial_id.text(),
        &req.recipient_pial_id.text(),
        req.amount_aet,
        &req.message,
    )
    .await;

    let tx_id = match transfer {
        Ok(tx_id) => {
            settle_intent(&state.pool, "tips", "completed", tip_id, Ok(&tx_id))
                .await
                .map_err(|e| settlement_unrecorded(tip_id, &tx_id, e))?;
            tx_id
        }
        Err(e) => {
            let why = e.to_string();
            let _ = settle_intent(&state.pool, "tips", "completed", tip_id, Err(&why)).await;
            return Err(cerr(StatusCode::PAYMENT_REQUIRED, &why));
        }
    };
    Ok(Json(
        json!({"tip_id": tip_id, "amount_aet": req.amount_aet, "ain_soph_tx_id": tx_id}),
    ))
}

// GET /v1/qr/:id — BIP21 QR code PNG for a purchase
pub async fn purchase_qr(
    State(state): State<Arc<AppState>>,
    Path(id): Path<Uuid>,
) -> impl IntoResponse {
    use axum::http::header;
    use image::{codecs::png::PngEncoder, ImageEncoder};
    use qrcode::QrCode;

    let row: Option<(String, i64)> =
        sqlx::query_as("SELECT btc_address, expected_sats FROM purchases WHERE id = $1")
            .bind(id)
            .fetch_optional(&state.pool)
            .await
            .unwrap_or(None);

    let (btc_address, expected_sats) = match row {
        Some(r) => r,
        None => {
            return (
                StatusCode::NOT_FOUND,
                [(header::CONTENT_TYPE, "application/json")],
                b"{}".to_vec(),
            )
                .into_response()
        }
    };

    let btc_amount = expected_sats as f64 / 100_000_000.0;
    let uri = format!("bitcoin:{}?amount={:.8}", btc_address, btc_amount);

    let code = match QrCode::new(uri.as_bytes()) {
        Ok(c) => c,
        Err(e) => {
            tracing::error!("QR encode error: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                [(header::CONTENT_TYPE, "application/json")],
                b"{}".to_vec(),
            )
                .into_response();
        }
    };

    // Render to an image with a module size of 4px and 2-module quiet zone
    let image = code
        .render::<image::Luma<u8>>()
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
        return (
            StatusCode::INTERNAL_SERVER_ERROR,
            [(header::CONTENT_TYPE, "application/json")],
            b"{}".to_vec(),
        )
            .into_response();
    }

    (
        StatusCode::OK,
        [(header::CONTENT_TYPE, "image/png")],
        png_bytes,
    )
        .into_response()
}

pub async fn creator_earnings(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<PialRef>,
) -> CommerceRes<Value> {
    let total: (i64,) = sqlx::query_as(
        "SELECT COALESCE(SUM(gross_aet), 0)::bigint FROM commerce_transactions \
         WHERE creator_pial_id = $1 AND status = 'completed'",
    )
    .bind(pial_id.text())
    .fetch_one(&state.pool)
    .await
    .map_err(cdberr)?;
    let by_type: Vec<(String, i64, i64)> = sqlx::query_as(
        "SELECT tx_type, COALESCE(SUM(gross_aet), 0)::bigint, COUNT(*)::bigint \
         FROM commerce_transactions \
         WHERE creator_pial_id = $1 AND status = 'completed' GROUP BY tx_type",
    )
    .bind(pial_id.text())
    .fetch_all(&state.pool)
    .await
    .unwrap_or_default();
    let mut subs_aet = 0i64;
    let mut ppv_aet = 0i64;
    let mut tips_aet = 0i64;
    let mut tx_count = 0i64;
    for (tx_type, amount, count) in &by_type {
        tx_count += count;
        match tx_type.as_str() {
            "subscription" => subs_aet += amount,
            "ppv" => ppv_aet += amount,
            "tip" => tips_aet += amount,
            _ => {}
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
