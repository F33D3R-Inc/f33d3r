use crate::{
    caeor::{self, AssetRefusal},
    db,
    models::*,
    pial_ref::{register_content, require_identity, ContentRef, IdentityError, PialRef},
    AppState,
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


pub(crate) fn pial_from_headers(headers: &HeaderMap) -> Option<PialRef> {
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
pub(crate) fn identity_refused(pial: &PialRef, e: IdentityError) -> (StatusCode, Json<serde_json::Value>) {
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

/// A person cannot hold more of the shops' reserved amounts than this at once.
const MAX_WAITING_PURCHASES: i64 = 10;

/// Compares without stopping at the first difference, so timing reveals nothing.
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

pub(crate) fn internal_key_ok(headers: &HeaderMap, cfg_key: &str) -> bool {
    headers
        .get("X-Internal-Key")
        .and_then(|v| v.to_str().ok())
        .map(|k| !cfg_key.is_empty() && constant_time_eq(k.as_bytes(), cfg_key.as_bytes()))
        .unwrap_or(false)
}

/// Only Nantar may call this service, because it trusts X-Pial-Identity as the caller's
/// identity. Health and metrics stay open to the container network's monitors.
pub async fn require_service_key(
    State(state): State<Arc<AppState>>,
    req: axum::extract::Request,
    next: axum::middleware::Next,
) -> axum::response::Response {
    let path = req.uri().path();
    if path == "/health" || path == "/metrics" {
        return next.run(req).await;
    }
    let key = &state.cfg.internal_api_key;
    let presented = req.headers().get("X-Service-Key").and_then(|v| v.to_str().ok()).unwrap_or("");
    if key.is_empty() || !constant_time_eq(presented.as_bytes(), key.as_bytes()) {
        return (StatusCode::UNAUTHORIZED, Json(serde_json::json!({"error":"service_key_required"}))).into_response();
    }
    next.run(req).await
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
    let commerce = crate::commerce_events::backlog(&state.pool).await;
    if let Err(ref e) = commerce {
        tracing::error!("commerce outbox backlog query: {e}");
    }
    let (pending, quarantined) = match commerce {
        Ok((p, q)) => (Some(p), Some(q)),
        Err(_) => (None, None),
    };
    Json(serde_json::json!({
        "ok":        true,
        "manhattan": state.manhattan.configured(),
        "manhattan_outbox_backlog": backlog.ok(),
        "commerce_outbox_pending": pending,
        "commerce_outbox_quarantined": quarantined,
    }))
}

// GET /v1/pubkey — Themis ECDH public key for browser to encrypt CEK
pub async fn pubkey(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    Json(serde_json::json!({ "public_key_b64": state.themis_pubkey_b64 }))
}

// POST /v1/events — receives marketplace.* events from Nantar
/// The content id of a signed marketplace event: sha256 over the event's own
/// fields — everything but `cid` and `pial_sig` — as compact JSON with keys in
/// sorted order. That is exactly what the browser hashes before signing
/// (malkuth.js signMarketplaceEvent: JSON.stringify with sorted keys), so the
/// signature is checked against what the request actually says, never against
/// a cid the request merely claims.
fn event_cid(raw: &serde_json::Value) -> Option<String> {
    use sha2::{Digest, Sha256};
    let obj = raw.as_object()?;
    let fields: std::collections::BTreeMap<&String, &serde_json::Value> = obj
        .iter()
        .filter(|(k, _)| k.as_str() != "cid" && k.as_str() != "pial_sig")
        .collect();
    let canonical = serde_json::to_vec(&fields).ok()?;
    Some(format!("sha256:{}", hex::encode(Sha256::digest(&canonical))))
}

pub async fn events(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(raw): Json<serde_json::Value>,
) -> impl IntoResponse {
    let evt: MarketplaceEvent = match serde_json::from_value(raw.clone()) {
        Ok(e) => e,
        Err(e) => {
            return (
                StatusCode::BAD_REQUEST,
                Json(serde_json::json!({"error":"invalid_event","message": e.to_string()})),
            )
                .into_response()
        }
    };
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
    // Taking a listing down through an edit does what a delete does, so it needs the same.
    let requires_sig = matches!(
        evt.event_type.as_str(),
        "marketplace.listing.create" | "marketplace.listing.delete"
    ) || (evt.event_type == "marketplace.listing.edit" && evt.is_active == Some(false));
    if requires_sig {
        match (&evt.cid, &evt.pial_sig) {
            (Some(cid), Some(sig)) => {
                if event_cid(&raw).as_deref() != Some(cid.as_str()) {
                    return (
                        StatusCode::UNAUTHORIZED,
                        Json(serde_json::json!({
                            "error": "cid_mismatch",
                            "message": "The signed content id does not match the event"
                        })),
                    )
                        .into_response();
                }
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
            if !crate::addresses::valid_btc(&btc) {
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
                    if !crate::addresses::valid_xrp(a) {
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

        "marketplace.shop.close" => match db::shop_close(&state.pool, requester).await {
            Ok(()) => Json(serde_json::json!({"ok":true})).into_response(),
            Err(e) => {
                tracing::error!("shop_close: {e}");
                (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(serde_json::json!({"error":"db_error"})),
                )
                    .into_response()
            }
        },

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
            // The body sold must be the seller's own encrypted upload, as Caeor
            // knows it — not a path the request merely names.
            let asset_id = match caeor::seller_asset(&state, &req.content_url, &requester).await {
                Ok(id) => id,
                Err(AssetRefusal::NotTheSellers(why)) => {
                    return (
                        StatusCode::FORBIDDEN,
                        Json(serde_json::json!({"error":"content_not_yours","message": why})),
                    )
                        .into_response()
                }
                Err(AssetRefusal::Unavailable(why)) => {
                    tracing::error!("listing_create: caeor: {why}");
                    return (
                        StatusCode::SERVICE_UNAVAILABLE,
                        Json(serde_json::json!({"error":"media_unavailable","message":"Your upload could not be checked right now. Try again shortly."})),
                    )
                        .into_response();
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
            match db::listing_create(&state.pool, requester, &req, cek_bytes, asset_id).await {
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
            // Apply edit (title, description, active — price/content immutable after listing)
            let applied = async {
                if let Some(ref title) = evt.title {
                    db::listing_update_title(&state.pool, listing_id, title).await?;
                }
                if let Some(ref desc) = evt.description {
                    db::listing_update_description(&state.pool, listing_id, desc).await?;
                }
                if let Some(active) = evt.is_active {
                    db::listing_set_active(&state.pool, listing_id, active).await?;
                }
                anyhow::Ok(())
            }
            .await;
            match applied {
                Ok(()) => Json(serde_json::json!({"ok":true})).into_response(),
                Err(e) => {
                    tracing::error!("listing_edit: {e}");
                    (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(serde_json::json!({"error":"db_error"})),
                    )
                        .into_response()
                }
            }
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
                    // Deleted, not paused: the maintenance loop withdraws its
                    // encrypted asset through Caeor once no buyer holds it.
                    match db::listing_delete(&state.pool, listing_id).await {
                        Ok(()) => Json(serde_json::json!({"ok":true})).into_response(),
                        Err(e) => {
                            tracing::error!("listing_delete: {e}");
                            (
                                StatusCode::INTERNAL_SERVER_ERROR,
                                Json(serde_json::json!({"error":"db_error"})),
                            )
                                .into_response()
                        }
                    }
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
                if shop.xrp_address.is_empty() {
                    return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"xrp_not_supported","message":"This seller does not accept XRP."}))).into_response();
                }
                if listing.price_xrp_drops <= 0 {
                    return (StatusCode::BAD_REQUEST, Json(serde_json::json!({"error":"xrp_price_not_set","message":"No XRP price set for this listing."}))).into_response();
                }
            }
            let currency = if use_xrp { "xrp" } else { "btc" };
            match db::purchases_waiting_for_buyer(&state.pool, requester).await {
                Ok(n) if n >= MAX_WAITING_PURCHASES => {
                    return (StatusCode::TOO_MANY_REQUESTS, Json(serde_json::json!({"error":"too_many_waiting","message":"You have too many payments waiting. Finish or let some expire first."}))).into_response();
                }
                Ok(_) => {}
                Err(e) => {
                    tracing::error!("{e}");
                    return (StatusCode::INTERNAL_SERVER_ERROR, Json(serde_json::json!({"error":"db_error"}))).into_response();
                }
            }
            let opened = match db::purchase_open_for_buyer(&state.pool, requester, listing_id, currency).await {
                Ok(Some(open)) => Ok(Some(open)),
                Ok(None) if use_xrp => {
                    db::purchase_open_xrp(&state.pool, listing_id, requester, &shop.xrp_address, listing.price_xrp_drops).await
                }
                Ok(None) => {
                    db::purchase_open_btc(&state.pool, listing_id, requester, &shop.btc_address, listing.price_sats).await
                }
                Err(e) => Err(e),
            };
            let purchase = match opened {
                Ok(Some(p)) => p,
                Ok(None) => {
                    return (StatusCode::SERVICE_UNAVAILABLE, Json(serde_json::json!({"error":"shop_busy","message":"This shop has too many payments waiting. Try again shortly."}))).into_response();
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

            if use_xrp {
                Json(serde_json::json!({
                    "ok":              true,
                    "purchase_id":     purchase.id,
                    "currency":        "xrp",
                    "xrp_address":     purchase.xrp_address,
                    "destination_tag": purchase.payment_tag,
                    "amount_drops":    purchase.expected_sats,
                    "amount_xrp":      purchase.expected_sats as f64 / 1_000_000.0,
                    "expires_at":      purchase.expires_at,
                }))
                .into_response()
            } else {
                Json(serde_json::json!({
                    "ok":          true,
                    "purchase_id": purchase.id,
                    "currency":    "btc",
                    "btc_address": purchase.btc_address,
                    "amount_sats": purchase.expected_sats,
                    "expires_at":  purchase.expires_at,
                }))
                .into_response()
            }
        }

        // Nothing charges for this yet, so it must not hand out access for free.
        "marketplace.subscription.create" => (
            StatusCode::GONE,
            Json(serde_json::json!({"error":"subscriptions_unavailable","message":"Marketplace subscriptions aren't available yet."})),
        )
            .into_response(),

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
            match db::marketplace_subscription_cancel(&state.pool, requester, sp).await {
                Ok(()) => Json(serde_json::json!({"ok":true})).into_response(),
                Err(e) => {
                    tracing::error!("marketplace_subscription_cancel: {e}");
                    (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(serde_json::json!({"error":"db_error"})),
                    )
                        .into_response()
                }
            }
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

// GET /v1/qr/:id — BIP21 QR code PNG for a purchase
pub async fn purchase_qr(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Path(id): Path<Uuid>,
) -> impl IntoResponse {
    use axum::http::header;
    use image::{codecs::png::PngEncoder, ImageEncoder};
    use qrcode::QrCode;

    // A payment request is its buyer's alone; anyone else is told it does not
    // exist, the same as purchase_status.
    let Some(requester) = pial_from_headers(&headers) else {
        return (
            StatusCode::UNAUTHORIZED,
            [(header::CONTENT_TYPE, "application/json")],
            b"{}".to_vec(),
        )
            .into_response();
    };
    let row: Option<(String, i64)> = match sqlx::query_as(
        "SELECT btc_address, expected_sats FROM purchases WHERE id = $1 AND buyer_pial = $2 AND currency = 'btc'",
    )
    .bind(id)
    .bind(requester.uuid())
    .fetch_optional(&state.pool)
    .await
    {
        Ok(r) => r,
        Err(e) => {
            tracing::error!("purchase_qr: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                [(header::CONTENT_TYPE, "application/json")],
                b"{}".to_vec(),
            )
                .into_response();
        }
    };

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

/// What a creator is told about a sale. Fixed when the charge is recorded and
/// sent when it settles.
pub(crate) fn sale_notice(event_id: String, pial: String, kind: &str, title: &str, target_id: String, amount: String) -> crate::notifications::Event {
    crate::notifications::Event {
        actor_account: None,
        recipient_account: None,
        event_id,
        recipient_pial: pial,
        kind: kind.into(),
        target_id,
        target_type: "commerce".into(),
        payload: serde_json::json!({ "amount_aet": amount }),
        push_body: amount,
        push_title: title.into(),
        push_url: "/wallet".into(),
    }
}

/// Error body shared by every JSON route.
pub(crate) fn cerr(status: StatusCode, msg: &str) -> (StatusCode, Json<serde_json::Value>) {
    (status, Json(serde_json::json!({"error": msg, "message": msg})))
}
