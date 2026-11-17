use axum::{
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    response::IntoResponse,
    Json,
};
use chrono::Utc;
use serde_json::json;
use sqlx::PgPool;
use std::collections::HashMap;
use tracing::{info, warn};
use uuid::Uuid;

use axum::extract::Query;

use crate::api::{ApiError, ApiResult};
use crate::identity;
use crate::models::{
    BootstrapRequest, BootstrapResponse, DecisionLogEntry, DecisionLogQuery, DecisionRequest,
    DecisionResponse, StatsResponse,
};
use crate::number_lease::{self, Admission};
use crate::{contact, db, decision, follow_graph};
use manhattan_client::{ConflictCode, Manhattan, ManhattanError};

// ── Shared application state ──────────────────────────────────────────────────

#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    /// The shared secret every route but /health, /metrics and /v1/auth/jwks
    /// requires as X-Internal-Key (api::require_internal_key).
    pub internal_api_key: String,
    /// Signs PIAL assertion tokens and publishes their verification keys.
    pub tokens: identity::tokens::TokenIssuer,
    /// The naming plane. Identity is resolved through it and never by joining
    /// into another brain's tables.
    pub manhattan: Manhattan,
    /// Signs assembled key bundles. Unconfigured means bundles ship unsigned and
    /// say so; it never means a bundle is presented as signed when it is not.
    pub bundle_signer: contact::BundleSigner,
}

// ── Identity is resolved by PIAL and by nothing else ─────────────────────────

/// Turn the identity segment of a path into a PIAL.
///
/// A bare UUID and the canonical `pial:<uuid>` name are the same thing and are
/// accepted as they stand. Any other name — a handle, a contact address, an id
/// minted by another brain — is not a second way to file a person: it goes to
/// Manhattan, which answers with the identity behind it, and everything
/// downstream keys on the PIAL that comes back.
///
/// That is the whole reason this brain no longer holds a foreign key into
/// anyone else's rows. Resolving a name used to be expensive, so ids got copied
/// between databases; one indexed lookup over a process boundary is cheaper
/// than the drift those copies caused.
async fn resolve_pial_ref(
    state: &AppState,
    raw: &str,
) -> Result<Uuid, (StatusCode, Json<serde_json::Value>)> {
    if let Ok(id) = Uuid::parse_str(raw) {
        return Ok(id);
    }

    if let Some(value) = raw.strip_prefix("pial:") {
        return Uuid::parse_str(value).map_err(|_| {
            (
                StatusCode::BAD_REQUEST,
                Json(json!({ "error": "invalid_pial_id" })),
            )
        });
    }

    if !raw.contains(':') {
        return Err((
            StatusCode::BAD_REQUEST,
            Json(json!({
                "error": "identity must be a PIAL uuid or a '<namespace>:<value>' name"
            })),
        ));
    }

    match state.manhattan.resolve_pial(raw).await {
        Ok(pial) => Uuid::parse_str(&pial).map_err(|e| {
            warn!(name = %raw, error = %e, "manhattan resolved a name to a malformed PIAL");
            (
                StatusCode::BAD_GATEWAY,
                Json(json!({ "error": "manhattan_returned_malformed_pial" })),
            )
        }),
        Err(ManhattanError::NotFound) => Err((
            StatusCode::NOT_FOUND,
            Json(json!({ "error": "pial_not_found" })),
        )),
        // Never a local fallback. Falling back to a join is the drift the naming
        // plane exists to end, so an unreachable plane is an outage, not a hint.
        Err(ManhattanError::NotConfigured) => {
            warn!(name = %raw, "identity lookup by name attempted with no MANHATTAN_URL");
            Err((
                StatusCode::SERVICE_UNAVAILABLE,
                Json(json!({ "error": "naming_plane_not_configured" })),
            ))
        }
        Err(e) => {
            warn!(name = %raw, error = %e, "manhattan resolution failed");
            Err((
                StatusCode::BAD_GATEWAY,
                Json(json!({ "error": "naming_plane_unavailable" })),
            ))
        }
    }
}

// ── Helper: extract trusted caller from X-Elohim-PIAL header ─────────────────

fn trusted_caller(headers: &HeaderMap) -> Option<Uuid> {
    headers
        .get("x-elohim-pial")
        .and_then(|v| v.to_str().ok())
        .and_then(|s| Uuid::parse_str(s).ok())
}

// ── GET /health ───────────────────────────────────────────────────────────────

/// Health carries the state of the naming plane, because a graph write that is
/// queued and never delivered is invisible everywhere else: the rows are in the
/// outbox, the API answers normally, and names simply stop resolving elsewhere.
/// Depth says the queue is behind; the age of its head and the error that head
/// last failed with say whether it is moving.
///
/// A failed backlog query is reported, not swallowed — but it does not fail the
/// check. This endpoint is the container healthcheck other brains gate their
/// start-up on, and turning it into a database liveness probe is a change to
/// make deliberately, not as a side effect of adding a counter to it.
pub async fn health(State(state): State<AppState>) -> impl IntoResponse {
    let configured = state.manhattan.configured();

    let manhattan = match db::outbox_backlog(&state.pool).await {
        Ok(backlog) => json!({
            "manhattan_outbox_pending":            backlog.pending,
            "manhattan_outbox_oldest_age_seconds": backlog.oldest_age_seconds,
            "manhattan_outbox_head_error":         backlog.head_last_error,
            // Rows the drain gave up on: naming-plane writes that did not happen
            // and will not until an operator clears blocked_at.
            "manhattan_outbox_quarantined":        backlog.quarantined,
        }),
        Err(e) => {
            warn!(error = %e, "manhattan outbox backlog query failed");
            json!({ "manhattan_outbox_pending": null, "manhattan_outbox_error": e.to_string() })
        }
    };
    // identity.events the estate has not heard yet, and any the relay gave up on.
    let identity_events = match identity::relay::backlog(&state.pool).await {
        Ok(b) => json!({
            "identity_outbox_pending":            b.pending,
            "identity_outbox_oldest_age_seconds": b.oldest_age_seconds,
            "identity_outbox_quarantined":        b.quarantined,
        }),
        Err(e) => {
            warn!(error = %e, "identity outbox backlog query failed");
            json!({ "identity_outbox_pending": null, "identity_outbox_error": e.to_string() })
        }
    };
    let degraded = manhattan.get("manhattan_outbox_error").is_some()
        || identity_events.get("identity_outbox_error").is_some();
    let mut body = json!({
        "status":    if degraded { "degraded" } else { "ok" },
        "service":   "elohim-veni",
        "manhattan": configured,
    });
    for part in [manhattan, identity_events] {
        if let (Some(dst), serde_json::Value::Object(src)) = (body.as_object_mut(), part) {
            dst.extend(src);
        }
    }
    Json(body)
}

// ── GET /v1/stats ─────────────────────────────────────────────────────────────

pub async fn stats(State(state): State<AppState>) -> ApiResult<Json<StatsResponse>> {
    let pool = &state.pool;
    let total_pials = db::count_pials(pool).await?;
    let total_decisions = db::count_decisions_total(pool).await?;
    let decisions_today = db::count_decisions_today(pool).await?;
    let denials_today = db::count_denials_today(pool).await?;
    let deny_rate_today = if decisions_today == 0 {
        0.0
    } else {
        denials_today as f64 / decisions_today as f64
    };
    Ok(Json(StatsResponse {
        service: "elohim-veni",
        total_pials,
        total_decisions,
        decisions_today,
        deny_rate_today,
    }))
}

// ── POST /v1/pial/bootstrap ───────────────────────────────────────────────────

/// Asserts a PIAL minted elsewhere (feed-engine's assertPIAL). Idempotent; it
/// cannot resurrect a revoked PIAL or change the tier of a live one. PIALs
/// minted by this brain's own account creation need no assertion.
pub async fn pial_bootstrap(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<BootstrapRequest>,
) -> ApiResult<impl IntoResponse> {
    let caller = trusted_caller(&headers);
    info!(pial_id = %req.pial_id, caller = ?caller, "pial_bootstrap");
    let tier = req.tier.as_deref().unwrap_or("BASIC");
    let mut tx = state.pool.begin().await?;
    let created = identity::pial::bootstrap(&mut tx, req.pial_id, tier).await?;
    tx.commit().await?;
    let status = if created { StatusCode::CREATED } else { StatusCode::OK };
    Ok((status, Json(BootstrapResponse { pial_id: req.pial_id, created })))
}

// ── GET /v1/pial/:pial_id/trust ───────────────────────────────────────────────

/// The whole trust row — the decision engine's signal and the enforcement
/// state — with the PIAL's tier.
pub async fn get_trust(
    State(state): State<AppState>,
    Path(pial_ref): Path<String>,
) -> axum::response::Response {
    let pial_id = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };
    let load = async {
        let mut conn = state.pool.acquire().await?;
        identity::view::load_pial(&mut conn, pial_id).await
    };
    match load.await {
        Ok(Some(v)) => {
            let mut body = json!(v.trust);
            body["pial_id"] = json!(pial_id);
            body["tier"] = json!(v.tier);
            Json(body).into_response()
        }
        Ok(None) => ApiError::not_found("not_found", "No such PIAL.").into_response(),
        Err(e) => ApiError::from(e).into_response(),
    }
}

// ── POST /v1/decisions ────────────────────────────────────────────────────────

/// The capability an action is gated on, in the one vocabulary. An action
/// with none is decided on status, trust and risk alone (and node_relay on
/// tier — see decision.rs).
fn capability_for_action(action: &str) -> Option<&'static str> {
    use identity::vocab::*;
    match action {
        "post" => Some(CAP_POSTING),
        "message" => Some(CAP_MESSAGING),
        "monetize" | "payout_activation" => Some(CAP_MONETIZATION),
        "adult_content" => Some(CAP_NSFW_ACCESS),
        "music_upload" => Some(CAP_MUSIC_UPLOAD),
        _ => None,
    }
}

pub async fn make_decision(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<DecisionRequest>,
) -> ApiResult<Json<DecisionResponse>> {
    let caller = trusted_caller(&headers);
    info!(pial_id = %req.pial_id, action = %req.action, caller = ?caller, "make_decision");

    let pial_view = {
        let mut conn = state.pool.acquire().await?;
        identity::view::load_pial(&mut conn, req.pial_id)
            .await?
            .ok_or_else(|| ApiError::not_found("not_found", "No such PIAL."))?
    };
    let now = Utc::now();
    let capability = capability_for_action(&req.action);
    // An explicit entry for the gating capability: whether it is held now.
    let cap_grant = capability
        .and_then(|c| pial_view.caps.get(c))
        .map(|row| (row.is_granted(now), None));
    let trust_score = pial_view.trust.score;
    let stored_anomaly = pial_view.trust.anomaly_score;
    let violations = pial_view.trust.violation_count;

    // The verification gate. The verification level is identity's own fact —
    // kyc_tier and age_verified on this PIAL, set only after Verity accepted
    // the attempt's evidence — so it is read here, from the view just loaded,
    // and nothing is asked of another brain. 'full' is the only tier that names
    // the person operating the account (a document, a face match and a live
    // check). An admin override reaches it too; 18 U.S.C. § 2257 eligibility
    // for publishing 18+ work is decided by Verity at publish time, not here.
    let verified = pial_view.facts.kyc_tier == "full";
    let verification_denial: Option<decision::EvalOutput> = match req.action.as_str() {
        "monetize" | "payout_activation" if !verified => Some(decision::EvalOutput {
            decision: "DENY".into(),
            confidence: 1.0,
            reason: format!(
                "kyc_tier {} — creator monetization requires a verified identity (document, face match and live check)",
                pial_view.facts.kyc_tier
            ),
            capability_required: Some(identity::vocab::CAP_MONETIZATION.into()),
        }),
        "adult_content" | "creator_signup" if !(verified && pial_view.facts.age_verified) => {
            Some(decision::EvalOutput {
                decision: "DENY".into(),
                confidence: 1.0,
                reason: format!(
                    "kyc_tier {}, age_verified {} — adult content requires a verified identity and age",
                    pial_view.facts.kyc_tier, pial_view.facts.age_verified
                ),
                capability_required: Some(identity::vocab::CAP_NSFW_ACCESS.into()),
            })
        }
        _ => None,
    };

    let outcome = match verification_denial {
        Some(denial) => denial,
        None => decision::evaluate(decision::EvalInput {
            pial_id: req.pial_id,
            action: &req.action,
            capability,
            pial_status: &pial_view.status,
            tier: &pial_view.tier,
            cap_grant,
            trust_score,
            stored_anomaly,
            violations,
            content_risk: req.context.content_risk,
            anomaly_score: req.context.anomaly_score,
        }),
    };

    let delta = match outcome.decision.as_str() {
        "ALLOW" => 0.01_f64,
        "QUARANTINE" => -0.05,
        "DENY" => -0.10,
        _ => 0.0,
    };
    let new_score = (trust_score + delta).clamp(0.0, 1.0);
    let new_anomaly = ((stored_anomaly * 0.9) + (req.context.anomaly_score * 0.1)).clamp(0.0, 1.0);
    let new_violations = if outcome.decision == "DENY" { violations + 1 } else { violations };

    // The decision and the trust it moves are one write: an answer whose audit
    // row or trust update failed is not given.
    let mut tx = state.pool.begin().await?;
    sqlx::query(
        "INSERT INTO decision_log \
         (pial_id, action, decision, confidence, reason, capability_required, content_risk, anomaly_score) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
    )
    .bind(req.pial_id)
    .bind(&req.action)
    .bind(&outcome.decision)
    .bind(outcome.confidence)
    .bind(&outcome.reason)
    .bind(&outcome.capability_required)
    .bind(req.context.content_risk)
    .bind(req.context.anomaly_score)
    .execute(&mut *tx)
    .await?;
    sqlx::query(
        "UPDATE trust_scores \
         SET score = $2, anomaly_score = $3, violation_count = $4, last_decision = $5, updated_at = NOW() \
         WHERE pial_id = $1",
    )
    .bind(req.pial_id)
    .bind(new_score)
    .bind(new_anomaly)
    .bind(new_violations)
    .bind(&outcome.decision)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;

    Ok(Json(DecisionResponse {
        decision: outcome.decision,
        confidence: outcome.confidence,
        reason: outcome.reason,
        capability_required: outcome.capability_required,
    }))
}

// ── GET /v1/decisions/log ─────────────────────────────────────────────────────

pub async fn decision_log(
    State(state): State<AppState>,
    Query(q): Query<DecisionLogQuery>,
) -> impl IntoResponse {
    let limit = q.limit.unwrap_or(50).min(500);
    let offset = q.offset.unwrap_or(0);

    let rows: Result<
        Vec<(
            Uuid,
            Uuid,
            String,
            String,
            f64,
            String,
            Option<String>,
            f64,
            f64,
            chrono::DateTime<Utc>,
        )>,
        _,
    > = if let Some(pial_id) = q.pial_id {
        sqlx::query_as(
            "SELECT id, pial_id, action, decision, confidence, reason, \
                        capability_required, content_risk, anomaly_score, created_at \
                 FROM decision_log WHERE pial_id = $1 \
                 ORDER BY created_at DESC LIMIT $2 OFFSET $3",
        )
        .bind(pial_id)
        .bind(limit)
        .bind(offset)
        .fetch_all(&state.pool)
        .await
    } else {
        sqlx::query_as(
            "SELECT id, pial_id, action, decision, confidence, reason, \
                        capability_required, content_risk, anomaly_score, created_at \
                 FROM decision_log \
                 ORDER BY created_at DESC LIMIT $1 OFFSET $2",
        )
        .bind(limit)
        .bind(offset)
        .fetch_all(&state.pool)
        .await
    };

    match rows {
        Ok(entries) => {
            let log: Vec<DecisionLogEntry> = entries
                .into_iter()
                .map(|r| DecisionLogEntry {
                    id: r.0,
                    pial_id: r.1,
                    action: r.2,
                    decision: r.3,
                    confidence: r.4,
                    reason: r.5,
                    capability_required: r.6,
                    content_risk: r.7,
                    anomaly_score: r.8,
                    created_at: r.9,
                })
                .collect();
            (
                StatusCode::OK,
                Json(json!({ "log": log, "count": log.len() })),
            )
                .into_response()
        }
        Err(e) => {
            warn!(error = %e, "decision_log query failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── POST /v1/pial/signing-key/register ───────────────────────────────────────
/// Register or update a PIAL's ECDSA-P256 signing public key.
/// Body: {"pial_id": "<uuid>", "public_key_b64": "...", "algorithm": "ECDSA-P256"}
/// Auth: X-Internal-Key
pub async fn register_signing_key(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let pial_id_str = body["pial_id"].as_str().unwrap_or_default();
    let pial_id = match Uuid::parse_str(pial_id_str) {
        Ok(id) => id,
        Err(_) => {
            return (
                StatusCode::BAD_REQUEST,
                Json(json!({"error":"invalid_pial_id"})),
            )
                .into_response()
        }
    };
    let pub_key = body["public_key_b64"].as_str().unwrap_or_default();
    let algorithm = body["algorithm"].as_str().unwrap_or("ECDSA-P256");
    if pub_key.is_empty() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error":"public_key_b64 required"})),
        )
            .into_response();
    }
    match db::upsert_signing_key(&state.pool, pial_id, pub_key, algorithm).await {
        Ok(_) => Json(json!({"ok": true})).into_response(),
        Err(e) => {
            tracing::error!("register_signing_key: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── GET /v1/pial/:pial_id/signing-pubkey ─────────────────────────────────────
/// Fetch a PIAL's ECDSA-P256 signing public key.
/// Auth: X-Internal-Key
pub async fn get_signing_pubkey(
    State(state): State<AppState>,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    let pial_id = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };

    match db::get_signing_key(&state.pool, pial_id).await {
        Ok(Some(key)) => Json(json!({"public_key_b64": key})).into_response(),
        Ok(None) => (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response(),
        Err(e) => {
            tracing::error!("get_signing_pubkey: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── POST /v1/pial/ecdh-key/register ──────────────────────────────────────────
/// Register or update a PIAL's ECDH-P256 public key.
/// Body: {"pial_id": "<uuid>", "public_key_b64": "..."}
/// Auth: X-Internal-Key
pub async fn register_ecdh_key(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let pial_id_str = body["pial_id"].as_str().unwrap_or_default();
    let pial_id = match Uuid::parse_str(pial_id_str) {
        Ok(id) => id,
        Err(_) => {
            return (
                StatusCode::BAD_REQUEST,
                Json(json!({"error":"invalid_pial_id"})),
            )
                .into_response()
        }
    };
    let pub_key = body["public_key_b64"].as_str().unwrap_or_default();
    if pub_key.is_empty() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error":"public_key_b64 required"})),
        )
            .into_response();
    }
    match db::upsert_ecdh_key(&state.pool, pial_id, pub_key).await {
        Ok(_) => Json(json!({"ok": true})).into_response(),
        Err(e) => {
            tracing::error!("register_ecdh_key: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── GET /v1/pial/:pial_id/ecdh-pubkey ────────────────────────────────────────
/// Fetch a PIAL's ECDH-P256 public key.
/// Auth: X-Internal-Key
pub async fn get_ecdh_pubkey(
    State(state): State<AppState>,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    let pial_id = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };

    match db::get_ecdh_key(&state.pool, pial_id).await {
        Ok(Some(key)) => Json(json!({"public_key_b64": key})).into_response(),
        Ok(None) => (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response(),
        Err(e) => {
            tracing::error!("get_ecdh_pubkey: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── POST /v1/addresses/mint ──────────────────────────────────────────────────
/// Mint a contact address. Manhattan owns `address`/`addr` to this brain, so
/// mints must originate here rather than from Nantar.
/// Auth: X-Internal-Key
///
/// The address is recorded against the minting ACCOUNT, so it leads to that
/// persona's key bundle and is listed to that persona alone.
pub async fn mint_address(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, pial) = match acting_account(&state, body["account_id"].as_str(), "mint_address").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let identity_node_id = match identity_node_for(&state, pial).await {
        Ok(id) => id,
        Err(e) => return naming_plane_error("mint_address: identity node", e),
    };
    match state.manhattan.mint_address(&identity_node_id).await {
        Ok(addr) => {
            if let Err(e) = contact::record_address(&state.pool, &addr.address, account).await {
                // An address no account answers for would lead to the person's
                // primary persona. Retire it rather than hand it out.
                if let Err(re) = state.manhattan.revoke_address(&addr.address).await {
                    tracing::error!("mint_address: could not retire an unrecorded address: {re}");
                }
                return db_error("mint_address: recording the account", e);
            }
            Json(json!({"address": addr.address, "status": addr.status})).into_response()
        }
        Err(e) => {
            tracing::error!("mint_address for identity {identity_node_id}: {e}");
            (
                StatusCode::BAD_GATEWAY,
                Json(json!({"error":"naming_plane_error"})),
            )
                .into_response()
        }
    }
}

// ── POST /v1/addresses/revoke ────────────────────────────────────────────────
/// Retire a contact address. Only future channel establishment is refused.
/// Ownership is re-checked here, not trusted from the caller: the address must
/// be the acting account's own, not merely its identity's.
/// Auth: X-Internal-Key
pub async fn revoke_address(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, pial) = match acting_account(&state, body["account_id"].as_str(), "revoke_address").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let address = body["address"].as_str().unwrap_or_default();
    if address.is_empty() {
        return bad("address required");
    }

    let target = match state.manhattan.resolve_address(address).await {
        Ok(t) => t,
        Err(e) => {
            tracing::warn!("revoke_address: resolving {address}: {e}");
            return (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response();
        }
    };
    let Some(target_node) = target.identity_node_id.as_deref() else {
        return (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response();
    };
    match pial_for_identity_node(&state, target_node).await {
        Ok(owner) if owner == pial => {}
        Ok(_) => return forbidden(),
        Err(e) => return naming_plane_error("revoke_address: owner", e),
    }
    match contact::address_account(&state.pool, address, pial).await {
        Ok(Some(holder)) if holder == account => {}
        Ok(_) => return forbidden(),
        Err(e) => return db_error("revoke_address: holder", e),
    }

    match state.manhattan.revoke_address(address).await {
        Ok(()) => Json(json!({"ok": true})).into_response(),
        Err(e) => {
            tracing::error!("revoke_address {address}: {e}");
            (
                StatusCode::BAD_GATEWAY,
                Json(json!({"error":"naming_plane_error"})),
            )
                .into_response()
        }
    }
}

// ── F33D3R Numbers and the contact plane ─────────────────────────────────────
//
// Manhattan allocates and resolves Numbers. This brain owns what a Number is
// WORTH: the policy attached to it, the capabilities that pre-authorise contact,
// and the standing grants that outlive rotation. Possession of a Number buys an
// attempt at contact under that Number's policy and nothing else.

/// The only reasons that cross the wire. An unresolved Number, a retired one and
/// a closed policy are one answer, so the endpoint cannot be used to test whether
/// a Number exists. The precise reason goes to the decision log.
const PUBLIC_ALLOWED: &str = "allowed";
const PUBLIC_REQUEST: &str = "request_required";
const PUBLIC_UNAVAILABLE: &str = "unavailable";

fn public_reason(decision: &str) -> &'static str {
    match decision {
        "allow" => PUBLIC_ALLOWED,
        "request" => PUBLIC_REQUEST,
        _ => PUBLIC_UNAVAILABLE,
    }
}

/// Parses any identifier this plane carries: a PIAL, a Manhattan node id, or a
/// capability id. All are UUIDs.
fn parse_uuid(value: &str) -> Option<Uuid> {
    Uuid::parse_str(value).ok()
}

/// The lease an owner asked for: how long this Number keeps admitting people,
/// and how many people it may admit.
///
/// Both are read and CHECKED here rather than at the surface that drew the
/// form. This brain owns the columns, so it owns what may go in them; a second
/// copy of these bounds in feed-engine would be a second answer waiting to
/// disagree.
///
/// The expiry crosses the wire as a DURATION and becomes an instant against
/// this database's clock, so no other machine's idea of "now" can lengthen or
/// shorten a lease.
///
/// ABSENT AND NULL ARE DIFFERENT, and the difference is the whole reason this
/// reads the raw JSON rather than a typed struct. A field that is not present
/// means the caller said nothing and the column is left alone; a field present
/// and null means the caller cleared it. Collapsing the two would make changing
/// a Number's policy from a form that never mentioned its lease quietly discard
/// the lease.
#[allow(clippy::type_complexity)]
fn lease_from(
    body: &serde_json::Value,
) -> Result<(Option<Option<i64>>, Option<Option<i32>>), &'static str> {
    let expires_in_seconds = match body.get("expires_in_seconds") {
        None => None,
        Some(serde_json::Value::Null) => Some(None),
        Some(v) => {
            let secs = v
                .as_i64()
                .ok_or("expires_in_seconds must be a whole number of seconds")?;
            Some(Some(number_lease::validate_lease_seconds(secs)?))
        }
    };
    let max_admissions = match body.get("max_admissions") {
        None => None,
        Some(serde_json::Value::Null) => Some(None),
        Some(v) => {
            let raw = v.as_i64().ok_or("max_admissions must be a whole number")?;
            let n = i32::try_from(raw).map_err(|_| "max_admissions is out of range")?;
            Some(Some(number_lease::validate_budget(n)?))
        }
    };
    Ok((expires_in_seconds, max_admissions))
}

fn bad(msg: &str) -> axum::response::Response {
    (StatusCode::BAD_REQUEST, Json(json!({ "error": msg }))).into_response()
}

fn forbidden() -> axum::response::Response {
    (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response()
}

fn naming_plane_error(context: &str, e: impl std::fmt::Display) -> axum::response::Response {
    tracing::error!("{context}: {e}");
    (
        StatusCode::BAD_GATEWAY,
        Json(json!({"error":"naming_plane_error"})),
    )
        .into_response()
}

/// The identity node for a PIAL, registered if the outbox has not delivered it yet.
async fn identity_node_for(state: &AppState, pial: Uuid) -> Result<String, ManhattanError> {
    let node = state
        .manhattan
        .ensure_node(
            manhattan_client::KIND_IDENTITY,
            &manhattan_client::pial_name(&pial.to_string()),
        )
        .await?;
    Ok(node.node_id)
}

/// A lease refusal, recorded where the owner can see it and nowhere else.
///
/// The caller is told exactly what an unknown Number is told, so this is the
/// only place the difference between "expired", "exhausted" and "never existed"
/// is written down — in the owner's own immutable decision log.
async fn log_lease_refusal(state: &AppState, owner: Uuid, lease: number_lease::LeaseState) {
    if let Err(e) = db::log_decision(
        &state.pool,
        owner,
        "contact.evaluate",
        "DENY",
        1.0,
        &format!(
            "policy=number_lease detail={} via_number=true",
            lease.as_str()
        ),
        Some("contact"),
        0.0,
        0.0,
    )
    .await
    {
        tracing::warn!("evaluate_contact: logging a lease refusal: {e}");
    }
}

/// The PIAL a Manhattan identity node answers to.
async fn pial_for_identity_node(state: &AppState, node_id: &str) -> Result<Uuid, ManhattanError> {
    let node = state.manhattan.get_node(node_id).await?;
    node.name_in(manhattan_client::NS_PIAL)
        .and_then(|p| parse_uuid(&p))
        .ok_or(ManhattanError::NotFound)
}

fn db_error(context: &str, e: impl std::fmt::Display) -> axum::response::Response {
    tracing::error!("{context}: {e}");
    (
        StatusCode::INTERNAL_SERVER_ERROR,
        Json(json!({"error":"db_error"})),
    )
        .into_response()
}

/// The account a contact-plane call acts as, and the identity it is bound to.
///
/// Every action on the contact plane is attributed to the ACCOUNT that took it
/// (migration 0004): the PIAL is derived here, from this brain's own accounts
/// table, and never accepted from the caller beside the account — two claims
/// about one actor are one too many. A missing id is a bad request; an account
/// that does not exist, is deactivated or is purged takes no contact action.
async fn acting_account(
    state: &AppState,
    raw: Option<&str>,
    context: &str,
) -> Result<(Uuid, Uuid), axum::response::Response> {
    let Some(account) = raw.and_then(parse_uuid) else {
        return Err(bad("account_id required"));
    };
    match contact::account_pial(&state.pool, account).await {
        Ok(Some(pial)) => Ok((account, pial)),
        Ok(None) => Err(forbidden()),
        Err(e) => Err(db_error(context, e)),
    }
}

/// The account that holds a Number: its policy row's, or — for a Number minted
/// before this brain recorded accounts — the identity's primary account.
async fn number_holder(
    state: &AppState,
    number_node: Uuid,
    owner_pial: Uuid,
) -> anyhow::Result<Option<Uuid>> {
    match contact::get_number_policy(&state.pool, number_node).await? {
        Some((account, _, _)) => Ok(Some(account)),
        None => contact::primary_account(&state.pool, owner_pial).await,
    }
}

// ── POST /v1/numbers/mint ────────────────────────────────────────────────────
/// Allocate a Number for an account and attach its policy. The Number lives
/// under the account's identity in Manhattan; the account that holds it is
/// recorded here, so it reaches that persona and no other.
/// Auth: X-Internal-Key
pub async fn mint_number(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, pial) = match acting_account(&state, body["account_id"].as_str(), "mint_number").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let policy = body["policy"]
        .as_str()
        .unwrap_or(contact::DEFAULT_NUMBER_POLICY);
    if !contact::is_policy(policy) {
        return bad("unknown policy");
    }
    // Owner-private and untrusted: cleaned once, here, at the authority that
    // owns the column, rather than at each surface that later renders it.
    let label = number_lease::sanitise_label(body["label"].as_str().unwrap_or_default());
    let (expires_in_seconds, max_admissions) = match lease_from(&body) {
        Ok(l) => l,
        Err(msg) => return bad(msg),
    };
    // THE DEFAULT BUDGET, applied here and only here.
    //
    // `None` means the caller's form did not mention a budget at all, which is
    // the one case a default may fill in. `Some(None)` is an owner who chose no
    // limit deliberately and keeps it. `set_number_policy` shares `lease_from`
    // and deliberately does NOT do this: an absent field there means "leave the
    // column alone", and a default applied on the way past would silently cap a
    // Number that has already been shared.
    let max_admissions = match max_admissions {
        None => Some(Some(number_lease::DEFAULT_BUDGET)),
        chosen => chosen,
    };
    // A Number this identity previously held and wants back. Manhattan decides
    // whether it may have it; this brain only carries the request.
    let reclaim = body["reclaim"]
        .as_str()
        .map(str::trim)
        .filter(|v| !v.is_empty());

    let node_id = match identity_node_for(&state, pial).await {
        Ok(id) => id,
        Err(e) => return naming_plane_error("mint_number: identity node", e),
    };
    let minted = match state.manhattan.mint_number(&node_id, reclaim).await {
        Ok(m) => m,
        // These three are not naming-plane failures, they are answers, and the
        // person who asked can act on every one of them. Reporting them as a
        // bad gateway would tell somebody holding five Numbers that the service
        // was broken rather than that they should retire one.
        Err(ManhattanError::Conflict(ConflictCode::NumberCapReached)) => {
            return (
                StatusCode::CONFLICT,
                // Deliberately no count. The cap is Manhattan's, because the
                // address space is Manhattan's, and a second copy of the number
                // here would be a second answer waiting to disagree with it.
                Json(json!({"error": "number_cap_reached"})),
            )
                .into_response();
        }
        Err(ManhattanError::Conflict(ConflictCode::NumberNotReclaimable)) => {
            return (
                StatusCode::CONFLICT,
                Json(json!({"error": "number_not_reclaimable"})),
            )
                .into_response()
        }
        Err(ManhattanError::Status(429, _)) => {
            return (
                StatusCode::TOO_MANY_REQUESTS,
                Json(json!({"error": "number_mint_rate"})),
            )
                .into_response()
        }
        Err(e) => return naming_plane_error("mint_number: allocation", e),
    };
    let Some(number_node_id) = minted.number_node_id.as_deref().and_then(parse_uuid) else {
        return naming_plane_error("mint_number", "Manhattan returned no number_node_id");
    };
    // Policy, label, lease and budget land as ONE row: they are one decision
    // the owner made at one moment, and a Number that is live with only half of
    // it is a promise nobody made.
    if let Err(e) = number_lease::attach(
        &state.pool,
        number_node_id,
        pial,
        account,
        policy,
        &label,
        expires_in_seconds,
        max_admissions,
    )
    .await
    {
        tracing::error!("mint_number: attaching policy: {e}");
        // The allocation happened in another brain, so a failure here would
        // otherwise leave a live Number with no policy behind it while the
        // person is told the mint failed — and the `number` namespace is not
        // reusable, so that Number would be burned for good. Retiring it makes
        // the failure total, which is the only honest version of it.
        if let Err(re) = state.manhattan.revoke_number(&minted.number).await {
            tracing::error!(
                "mint_number: could not retire the Number whose policy failed to attach: {re}"
            );
        }
        return (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(json!({"error":"db_error"})),
        )
            .into_response();
    }
    Json(json!({
        "number": minted.number,
        "number_node_id": number_node_id,
        "policy": policy,
        "label": label,
        "expires_in_seconds": expires_in_seconds.flatten(),
        "max_admissions": max_admissions.flatten(),
    }))
    .into_response()
}

// ── POST /v1/numbers/revoke ──────────────────────────────────────────────────
/// Retire a Number. The identity, its keys, its devices, its sessions and every
/// open conversation are untouched.
/// Auth: X-Internal-Key
pub async fn revoke_number(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, pial) = match acting_account(&state, body["account_id"].as_str(), "revoke_number").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let number = body["number"].as_str().unwrap_or_default();
    if number.is_empty() {
        return bad("number required");
    }

    let resolution = match state.manhattan.resolve_number(number).await {
        Ok(r) => r,
        Err(e) => return naming_plane_error("revoke_number: resolve", e),
    };
    let (Some(number_node), Some(identity_node)) = (
        resolution.number_node_id.as_deref(),
        resolution.identity_node_id.as_deref(),
    ) else {
        return (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response();
    };

    // Ownership is re-checked here even though the caller checked: this brain is
    // the authority for the namespace, so the test has to hold here too.
    match pial_for_identity_node(&state, identity_node).await {
        Ok(owner) if owner == pial => {}
        Ok(_) => return forbidden(),
        Err(e) => return naming_plane_error("revoke_number: owner", e),
    }
    let Some(node) = parse_uuid(number_node) else {
        return naming_plane_error("revoke_number", "Manhattan returned a malformed number_node_id");
    };
    // The identity holding it is not enough: the Number must be this account's,
    // never another persona's of the same person.
    match number_holder(&state, node, pial).await {
        Ok(Some(holder)) if holder == account => {}
        Ok(_) => return forbidden(),
        Err(e) => return db_error("revoke_number: holder", e),
    }

    if let Err(e) = state.manhattan.revoke_number(number).await {
        return naming_plane_error("revoke_number", e);
    }
    if let Err(e) = contact::drop_number_policy(&state.pool, node, account).await {
        return db_error("revoke_number: dropping policy", e);
    }
    Json(json!({"ok": true})).into_response()
}

// ── GET /v1/numbers/:account_id ──────────────────────────────────────────────
/// One account's own Numbers, live and retired, each with its policy. Manhattan
/// lists every Number under the identity; only the ones this account holds are
/// answered, so one persona's listing never shows another's Numbers.
/// Auth: X-Internal-Key
pub async fn list_numbers(
    State(state): State<AppState>,
    Path(account_ref): Path<String>,
) -> impl IntoResponse {
    let (account, pial) = match acting_account(&state, Some(account_ref.as_str()), "list_numbers").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let node_id = match identity_node_for(&state, pial).await {
        Ok(id) => id,
        Err(e) => return naming_plane_error("list_numbers: identity node", e),
    };
    let entries = match state.manhattan.list_numbers(&node_id).await {
        Ok(e) => e,
        Err(e) => return naming_plane_error("list_numbers", e),
    };
    let holders: HashMap<String, Uuid> = match contact::number_holders(&state.pool, pial).await {
        Ok(rows) => rows.into_iter().map(|(node, holder)| (node.to_string(), holder)).collect(),
        Err(e) => return db_error("list_numbers: holders", e),
    };
    // A Number with no policy row predates accounts on this plane and is the
    // identity's primary account's.
    let primary = match contact::primary_account(&state.pool, pial).await {
        Ok(p) => p,
        Err(e) => return db_error("list_numbers: primary account", e),
    };
    let entries: Vec<_> = entries
        .into_iter()
        .filter(|e| holders.get(&e.number_node_id).copied().or(primary) == Some(account))
        .collect();
    let policies = match contact::number_policies_for(&state.pool, account).await {
        Ok(p) => p,
        Err(e) => {
            tracing::error!("list_numbers: policies: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response();
        }
    };
    // The lease beside the policy. Both are owner-private: this route is keyed
    // by the owner's own account behind the internal key, and nothing here is
    // ever reachable from a resolve.
    let leases = match number_lease::leases_for(&state.pool, account).await {
        Ok(l) => l,
        Err(e) => {
            tracing::error!("list_numbers: leases: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response();
        }
    };
    let by_lease: HashMap<String, number_lease::LeaseView> = leases
        .into_iter()
        .map(|l| (l.number_node_id.to_string(), l))
        .collect();

    let by_node: HashMap<String, (String, String)> = policies
        .into_iter()
        .map(|(node, policy, label)| (node.to_string(), (policy, label)))
        .collect();

    let out: Vec<serde_json::Value> = entries
        .into_iter()
        .map(|e| {
            let (policy, label) = by_node
                .get(&e.number_node_id)
                .cloned()
                .unwrap_or_else(|| (contact::DEFAULT_NUMBER_POLICY.to_string(), String::new()));
            let lease = by_lease.get(&e.number_node_id);
            json!({
                "number": e.number,
                "number_node_id": e.number_node_id,
                // Owner-facing only, and only on the owner's own listing: this
                // is how somebody still holding a base32 Number learns it is an
                // older shape and can choose to mint a new one. No resolve
                // carries it and no refusal mentions it.
                "legacy": e.legacy,
                "status": e.status,
                "policy": policy,
                "label": label,
                "created_at": e.created_at,
                "revoked_at": e.revoked_at,
                "expires_at": lease.and_then(|l| l.expires_at).map(|t| t.to_rfc3339()),
                "expires_in_seconds": lease.and_then(|l| l.remaining_seconds),
                "max_admissions": lease.and_then(|l| l.max_admissions),
                "admissions": lease.map(|l| l.admissions).unwrap_or(0),
                "lease_state": lease
                    .map(|l| l.state.as_str())
                    .unwrap_or_else(|| number_lease::LeaseState::Live.as_str()),
            })
        })
        .collect();
    Json(json!({ "numbers": out })).into_response()
}

// ── POST /v1/numbers/policy ──────────────────────────────────────────────────
/// Set one Number's policy. A conference-badge Number and a business-card Number
/// are different promises, so policy is per-Number and not per-identity.
/// Auth: X-Internal-Key
pub async fn set_number_policy(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, pial) = match acting_account(&state, body["account_id"].as_str(), "set_number_policy").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let number = body["number"].as_str().unwrap_or_default();
    let policy = body["policy"].as_str().unwrap_or_default();
    if number.is_empty() || !contact::is_policy(policy) {
        return bad("number and a known policy are required");
    }
    let label = number_lease::sanitise_label(body["label"].as_str().unwrap_or_default());
    let (expires_in_seconds, max_admissions) = match lease_from(&body) {
        Ok(l) => l,
        Err(msg) => return bad(msg),
    };

    let resolution = match state.manhattan.resolve_number(number).await {
        Ok(r) => r,
        Err(e) => return naming_plane_error("set_number_policy: resolve", e),
    };
    let (Some(number_node), Some(identity_node)) = (
        resolution.number_node_id.as_deref().and_then(parse_uuid),
        resolution.identity_node_id.as_deref(),
    ) else {
        return (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response();
    };
    match pial_for_identity_node(&state, identity_node).await {
        Ok(owner) if owner == pial => {}
        Ok(_) => return forbidden(),
        Err(e) => return naming_plane_error("set_number_policy: owner", e),
    }
    match number_holder(&state, number_node, pial).await {
        Ok(Some(holder)) if holder == account => {}
        Ok(_) => return forbidden(),
        Err(e) => return db_error("set_number_policy: holder", e),
    }
    match number_lease::attach(
        &state.pool,
        number_node,
        pial,
        account,
        policy,
        &label,
        expires_in_seconds,
        max_admissions,
    )
    .await
    {
        Ok(()) => Json(json!({"ok": true, "policy": policy})).into_response(),
        Err(e) => {
            tracing::error!("set_number_policy: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── GET /v1/contact/policy/:account_id ───────────────────────────────────────
/// One account's contact settings. Each persona has its own @handle, so who may
/// reach it is its own choice.
/// Auth: X-Internal-Key
pub async fn get_contact_policy(
    State(state): State<AppState>,
    Path(account_ref): Path<String>,
) -> impl IntoResponse {
    let (account, _) = match acting_account(&state, Some(account_ref.as_str()), "get_contact_policy").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    match contact::get_contact_policy(&state.pool, account).await {
        Ok((handle_policy, default_number_policy)) => Json(json!({
            "handle_policy": handle_policy,
            "default_number_policy": default_number_policy,
            "policies": contact::POLICIES,
        }))
        .into_response(),
        Err(e) => {
            tracing::error!("get_contact_policy: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── POST /v1/contact/policy ──────────────────────────────────────────────────
/// Auth: X-Internal-Key
pub async fn set_contact_policy(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, _) = match acting_account(&state, body["account_id"].as_str(), "set_contact_policy").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let (existing_handle, existing_number) =
        match contact::get_contact_policy(&state.pool, account).await {
            Ok(p) => p,
            Err(e) => {
                tracing::error!("set_contact_policy: read: {e}");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(json!({"error":"db_error"})),
                )
                    .into_response();
            }
        };
    let handle_policy = body["handle_policy"].as_str().unwrap_or(&existing_handle);
    let number_policy = body["default_number_policy"]
        .as_str()
        .unwrap_or(&existing_number);
    if !contact::is_policy(handle_policy) || !contact::is_policy(number_policy) {
        return bad("unknown policy");
    }
    match contact::set_contact_policy(&state.pool, account, handle_policy, number_policy).await {
        Ok(()) => Json(json!({
            "handle_policy": handle_policy,
            "default_number_policy": number_policy,
        }))
        .into_response(),
        Err(e) => {
            tracing::error!("set_contact_policy: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── POST /v1/contact/evaluate ────────────────────────────────────────────────
/// The decision. Given a Number (or a target account, for the @handle path),
/// decide whether the initiating account may contact the account it reaches.
///
/// Both ends are ACCOUNTS. A Number reaches the account that holds it, never
/// the holder's primary persona; a grant, a link and an admission are each
/// between two accounts. The PIALs are derived here for what the identity still
/// owns — the follow graph Manhattan publishes and the owner's decision log —
/// and never cross the wire.
///
/// `target_account` is returned ONLY on allow. Every other outcome carries the
/// same coarse reason, so this endpoint cannot be used to test whether a Number
/// exists.
/// Auth: X-Internal-Key
pub async fn evaluate_contact(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (initiator_account, initiator) =
        match acting_account(&state, body["initiator_account"].as_str(), "evaluate_contact").await {
            Ok(a) => a,
            Err(resp) => return resp,
        };
    let number = body["number"]
        .as_str()
        .unwrap_or_default()
        .trim()
        .to_string();
    let via_number = !number.is_empty();
    // `follows` and `mutual` are deliberately NOT read off this request. They
    // used to be, and a security decision resting on a boolean the caller sets
    // is not a decision, it is a suggestion. They are read from Manhattan's
    // association plane below, by the brain that is taking the decision.
    let presented_capability = body["capability"].as_str().unwrap_or_default().to_string();
    let note = body["note"].as_str().unwrap_or_default();

    let deny = |reason: &'static str| {
        Json(json!({
            "decision": "deny",
            "reason": PUBLIC_UNAVAILABLE,
            "detail": reason,
        }))
    };

    // Resolve the target: by Number, or by account on the @handle path. The
    // target is an account and its identity; a Number reaches the account that
    // holds it.
    let (owner_account, owner, number_node): (Uuid, Uuid, Option<Uuid>) = if via_number {
        let resolution = match state.manhattan.resolve_number(&number).await {
            Ok(r) => r,
            Err(e) => return naming_plane_error("evaluate_contact: resolve", e),
        };
        if !resolution.found {
            return deny("unresolved").into_response();
        }
        let Some(identity_node) = resolution.identity_node_id.as_deref() else {
            return deny("unresolved").into_response();
        };
        let owner = match pial_for_identity_node(&state, identity_node).await {
            Ok(o) => o,
            Err(ManhattanError::NotFound) => return deny("unresolved").into_response(),
            Err(e) => return naming_plane_error("evaluate_contact: owner", e),
        };
        let Some(node) = resolution.number_node_id.as_deref().and_then(parse_uuid) else {
            return deny("unresolved").into_response();
        };
        let holder = match number_holder(&state, node, owner).await {
            Ok(Some(h)) => h,
            Ok(None) => return deny("unresolved").into_response(),
            Err(e) => return db_error("evaluate_contact: holder", e),
        };
        (holder, owner, Some(node))
    } else {
        let Some(target_account) = body["target_account"].as_str().and_then(parse_uuid) else {
            return bad("number or target_account required");
        };
        match contact::account_pial(&state.pool, target_account).await {
            Ok(Some(target_pial)) => (target_account, target_pial, None),
            Ok(None) => return deny("unresolved").into_response(),
            Err(e) => return db_error("evaluate_contact: target", e),
        }
    };

    // The caller is the only one told this, and the caller is the person: two
    // of one person's accounts never need permission to reach each other.
    if owner == initiator {
        return bad("an identity does not need permission to reach itself");
    }

    // ── The Number's lease ───────────────────────────────────────────────────
    // A Number whose lease has run out, or whose budget is spent, stops
    // admitting contact. It must be impossible to tell that apart from a
    // retired or an unknown Number, so it takes the SAME exit with the same
    // body — and it takes it HERE, before a presented contact link could be
    // redeemed, so a refused attempt cannot burn one of the owner's uses on the
    // way out. The precise reason goes to the owner's decision log, which is
    // theirs and not the caller's.
    let mut number_has_budget = false;
    if let Some(node) = number_node {
        let gate = match number_lease::gate(&state.pool, node).await {
            Ok(l) => l,
            Err(e) => {
                tracing::error!("evaluate_contact: lease: {e}");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(json!({"error":"db_error"})),
                )
                    .into_response();
            }
        };
        if !gate.state.admits() {
            log_lease_refusal(&state, owner, gate.state).await;
            return deny("unresolved").into_response();
        }
        number_has_budget = gate.has_budget;
    }

    // Policy: the Number's own, else the owning account's default for that channel.
    let (handle_policy, default_number_policy) =
        match contact::get_contact_policy(&state.pool, owner_account).await {
            Ok(p) => p,
            Err(e) => {
                tracing::error!("evaluate_contact: policy: {e}");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(json!({"error":"db_error"})),
                )
                    .into_response();
            }
        };
    let policy = if via_number {
        match number_node {
            Some(node) => match contact::get_number_policy(&state.pool, node).await {
                Ok(Some((_, p, _))) => p,
                Ok(None) => default_number_policy,
                Err(e) => {
                    tracing::error!("evaluate_contact: number policy: {e}");
                    return (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(json!({"error":"db_error"})),
                    )
                        .into_response();
                }
            },
            None => default_number_policy,
        }
    } else {
        handle_policy.clone()
    };

    let granted = match contact::has_grant(&state.pool, owner_account, initiator_account).await {
        Ok(g) => g,
        Err(e) => {
            tracing::error!("evaluate_contact: grant: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response();
        }
    };

    // A capability is redeemed only when one is actually presented, so an
    // ordinary resolve never consumes a use.
    let capability_ok = if presented_capability.is_empty() || granted {
        false
    } else {
        match contact::redeem_capability(&state.pool, owner_account, &presented_capability, number_node)
            .await
        {
            Ok(Some(_)) => true,
            Ok(None) => false,
            Err(e) => {
                tracing::error!("evaluate_contact: capability: {e}");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(json!({"error":"db_error"})),
                )
                    .into_response();
            }
        }
    };

    // ── The follow graph ─────────────────────────────────────────────────────
    // Read, not asserted. `followers` and `mutuals` are decisions about who
    // follows whom; feed-engine owns that graph and publishes it to Manhattan's
    // association plane, and this brain — which already knows the target,
    // because it resolved the Number itself — looks it up rather than being
    // told. That is what lets a mutual follow presenting a Number be admitted
    // straight through instead of being demoted to a contact request, without
    // the resolving brain ever learning whose Number it was.
    //
    // Asked when it can change the answer, and in exactly two cases it can. The
    // first is a policy that reads the graph. The second is a Number carrying a
    // USE BUDGET: on F33D3R a contact IS a mutual follow, a contact never needed
    // the Number, and so a mutual follow must not spend a unit of it. That rule
    // is unenforceable without this fact, and it was silently unreachable while
    // these booleans arrived as `false` from a caller that could not compute
    // them. A Number with no budget still costs no round trip.
    let mut follows = false;
    let mut mutual = false;
    if !granted
        && !capability_ok
        && (follow_graph::depends_on_follow_graph(&policy) || number_has_budget)
    {
        match follow_graph::between(&state.manhattan, initiator, owner).await {
            Ok((facts, answer)) => {
                // A name the plane has never seen can carry no association, so
                // false is the truthful answer — but an identity missing from
                // the plane is an undrained outbox, and that is worth saying out
                // loud rather than recording as "not a follower".
                if !answer.subject_resolved || !answer.object_resolved {
                    tracing::warn!(
                        subject_resolved = answer.subject_resolved,
                        object_resolved = answer.object_resolved,
                        "evaluate_contact: an identity in this pair is not in the naming plane; \
                         the follow graph cannot be read for it and contact falls back to the \
                         policy's refusal"
                    );
                }
                follows = facts.follows;
                mutual = facts.mutual;
            }
            // "Not following" and "could not ask" are different answers and only
            // one of them may quietly refuse. Fail closed and loud: the same
            // refusal an unreachable naming plane already produces everywhere
            // else on this path, rather than a silent demotion to a knock that
            // looks exactly like a stranger's.
            Err(e) => return naming_plane_error("evaluate_contact: follow graph", e),
        }
    }

    let facts = contact::ContactFacts {
        via_number,
        follows,
        mutual,
        granted,
        capability_ok,
    };
    let (decision, detail) = contact::decide(&policy, &facts);

    // ── Spending the Number's budget ─────────────────────────────────────────
    // Only an `allow` spends, and only when the Number is what produced it.
    //
    // A knock spends nothing. Charging one would let a hostile crowd exhaust a
    // Number the owner never agreed to open for anybody — the same denial of
    // service the budget exists to prevent, wearing the budget's own clothes.
    // The knock queue has its own bound: one pending request per requester, for
    // ever, enforced by a unique index, and it is reachable by @handle without
    // any Number at all.
    //
    // Someone who could already reach the owner spends nothing either. On
    // F33D3R a contact IS a mutual follow, and a contact never needed the
    // Number; letting the owner's own people drain a conference Number would
    // make the budget mean something nobody asked for.
    let mut budget_alert = number_lease::BudgetAlert::None;
    if number_lease::spends_budget(decision, via_number, &facts) {
        // via_number is true here, so Manhattan resolved a Number node; the
        // match is exhaustive rather than assuming it.
        if let Some(node) = number_node {
            match number_lease::record_admission(&state.pool, node, initiator_account, owner_account, owner)
                .await
            {
                // Spent, already spent by this same account, or a Number
                // carrying no budget at all. All three proceed.
                Ok(Admission::Recorded(alert)) => budget_alert = alert,
                Ok(Admission::AlreadyAdmitted) | Ok(Admission::NoLease) => {}
                // The last slot went to somebody else between the pre-gate above
                // and this write. This is the authoritative check — it holds the
                // row lock — so the cap is exact rather than approximately right
                // under load. Same exit as an unknown Number, and nothing was
                // written on the way to it.
                Ok(Admission::Refused) => {
                    log_lease_refusal(&state, owner, number_lease::LeaseState::Exhausted).await;
                    return deny("unresolved").into_response();
                }
                Err(e) => {
                    tracing::error!("evaluate_contact: recording an admission: {e}");
                    return (
                        StatusCode::INTERNAL_SERVER_ERROR,
                        Json(json!({"error":"db_error"})),
                    )
                        .into_response();
                }
            }
        }
    }

    // A redeemed capability becomes a standing grant, so the link works once and
    // the contact survives the Number being rotated away.
    if capability_ok {
        if let Err(e) =
            contact::grant_contact(&state.pool, owner_account, owner, initiator_account, "capability", number_node)
                .await
        {
            tracing::error!("evaluate_contact: recording grant: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response();
        }
    }

    let mut request_id: Option<Uuid> = None;
    if decision == "request" {
        match contact::open_request(&state.pool, owner_account, owner, initiator_account, number_node, note).await {
            Ok(id) => request_id = id,
            Err(e) => {
                tracing::error!("evaluate_contact: opening request: {e}");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(json!({"error":"db_error"})),
                )
                    .into_response();
            }
        }
    }

    // The precise reason is audit, not wire.
    if let Err(e) = db::log_decision(
        &state.pool,
        owner,
        "contact.evaluate",
        &decision.to_uppercase(),
        1.0,
        &format!("policy={policy} detail={detail} via_number={via_number}"),
        Some("contact"),
        0.0,
        0.0,
    )
    .await
    {
        tracing::warn!("evaluate_contact: logging decision: {e}");
    }

    let mut out = json!({
        "decision": decision,
        "reason": public_reason(decision),
    });
    if decision == "allow" {
        out["target_account"] = json!(owner_account.to_string());
        // A Number that quietly stopped working is the worst failure this
        // feature has, so the moment it stops is the moment its owner hears
        // about it — through the one notification pipeline, addressed to the
        // account that holds the Number (record_admission queues it).
        //
        // It rides on an ALLOW and nowhere else. An allow has already disclosed
        // the target to the calling brain, so nothing new crosses; a refusal
        // discloses nothing, and attaching "that Number is exhausted" to one
        // would hand over the single fact the refusal exists to withhold.
        if let Some(alert) = budget_alert.as_str() {
            out["budget_alert"] = json!(alert);
            out["budget_owner_account"] = json!(owner_account.to_string());
        }
    }
    if let Some(id) = request_id {
        out["request_id"] = json!(id.to_string());
        // Whose inbox the request landed in. A request that nobody is told about
        // is a request nobody answers, and the brain that owns notifications
        // cannot address one without knowing the recipient. This is disclosed
        // only alongside a request that was actually opened — which already
        // implies the Number resolved — and it is disclosed to the caller
        // brain, which decides what a person sees, never to the requester.
        out["request_owner_account"] = json!(owner_account.to_string());
    }
    Json(out).into_response()
}

// ── POST /v1/contact/keybundle ───────────────────────────────────────────────
/// Assemble and sign one ACCOUNT's messaging key bundle.
///
/// The account is named directly (`account_id`, after a Number's allow named
/// it) or by a live contact address (`address`), which resolves to the account
/// that minted it. An unknown or retired address is a flat 404 — a rotated
/// address must not confirm that it was ever real.
///
/// The bundle carries the account id, the time and that account's per-device
/// keys — no PIAL and no identity-wide key, because whoever holds a Number or an
/// address receives it, and anything one person's personas share would link
/// them. What is signed is exactly the `bundle` object this response carries,
/// as serialised here; a verifier checks the signature over those bytes.
///
/// `signed` is false when no service signing key is configured; the response
/// never implies a signature it does not carry. The signature covers transport
/// tampering only — it is this service's key, not the account's.
/// Auth: X-Internal-Key
pub async fn contact_key_bundle(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let not_found = || (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response();
    let account = if let Some(address) = body["address"].as_str().map(str::trim).filter(|a| !a.is_empty()) {
        let target = match state.manhattan.resolve_address(address).await {
            Ok(t) => t,
            Err(ManhattanError::NotFound) => return not_found(),
            Err(e) => return naming_plane_error("contact_key_bundle: resolve address", e),
        };
        let Some(identity_node) = target.identity_node_id.as_deref() else {
            return not_found();
        };
        let pial = match pial_for_identity_node(&state, identity_node).await {
            Ok(p) => p,
            Err(ManhattanError::NotFound) => return not_found(),
            Err(e) => return naming_plane_error("contact_key_bundle: address owner", e),
        };
        match contact::address_account(&state.pool, address, pial).await {
            Ok(Some(a)) => a,
            Ok(None) => return not_found(),
            Err(e) => return db_error("contact_key_bundle: address account", e),
        }
    } else {
        match acting_account(&state, body["account_id"].as_str(), "contact_key_bundle").await {
            Ok((a, _)) => a,
            Err(resp) => return resp,
        }
    };

    let devices = match contact::messaging_keys(&state.pool, account).await {
        Ok(d) => d,
        Err(e) => {
            tracing::error!("contact_key_bundle: messaging keys: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response();
        }
    };
    if devices.is_empty() {
        // No key material exists for this account, so no channel can be opened.
        return (
            StatusCode::CONFLICT,
            Json(json!({"error":"no_key_material"})),
        )
            .into_response();
    }

    let bundle = contact::KeyBundle {
        account_id: account.to_string(),
        issued_at: Utc::now().to_rfc3339(),
        devices,
    };

    // Sign the Value, not the struct: the response ships the bundle as this
    // Value, and serialising the same Value again yields the same bytes, so
    // what is signed is byte for byte the `bundle` the response carries.
    // Signing to_vec(&bundle) would cover struct-order bytes nobody receives.
    let bundle_value = match serde_json::to_value(&bundle) {
        Ok(v) => v,
        Err(e) => {
            tracing::error!("contact_key_bundle: encoding: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"encode_error"})),
            )
                .into_response();
        }
    };
    let payload = match serde_json::to_vec(&bundle_value) {
        Ok(p) => p,
        Err(e) => {
            tracing::error!("contact_key_bundle: encoding: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"encode_error"})),
            )
                .into_response();
        }
    };
    let signature = state.bundle_signer.sign(&payload);

    Json(json!({
        "bundle": bundle_value,
        "signed": signature.is_some(),
        "signature": signature,
        "signature_algorithm": signature.as_ref().map(|_| "Ed25519"),
        "signature_key_b64": state.bundle_signer.public_b64(),
    }))
    .into_response()
}

// ── POST /v1/pial/messaging-key/register ─────────────────────────────────────
/// Register the X25519 public key Gnosis seals to, for one device of one
/// account. Public halves only. Each account keeps its own keys: a key shared by
/// every persona of one person would put the same bytes in each persona's
/// bundle.
/// Auth: X-Internal-Key
pub async fn register_messaging_key(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, pial) =
        match acting_account(&state, body["account_id"].as_str(), "register_messaging_key").await {
            Ok(a) => a,
            Err(resp) => return resp,
        };
    let public_key_b64 = body["public_key_b64"].as_str().unwrap_or_default();
    if public_key_b64.is_empty() || public_key_b64.len() > 512 {
        return bad("public_key_b64 required");
    }
    let device_id = body["device_id"].as_str().unwrap_or_default();
    if device_id.len() > 128 {
        return bad("device_id too long");
    }
    match contact::upsert_messaging_key(&state.pool, account, pial, device_id, public_key_b64).await {
        Ok(()) => Json(json!({"ok": true})).into_response(),
        Err(e) => {
            tracing::error!("register_messaging_key: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── POST /v1/contact/capabilities/mint ───────────────────────────────────────
/// Mint a 160-bit contact capability. Returned once, in full, and never again:
/// only its SHA-256 is stored.
/// Auth: X-Internal-Key
pub async fn mint_contact_capability(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, pial) =
        match acting_account(&state, body["account_id"].as_str(), "mint_contact_capability").await {
            Ok(a) => a,
            Err(resp) => return resp,
        };
    let label = body["label"].as_str().unwrap_or_default();
    let max_uses = body["max_uses"].as_i64().map(|v| v as i32);

    let number_node = match body["number"].as_str() {
        Some(number) if !number.trim().is_empty() => {
            let resolution = match state.manhattan.resolve_number(number).await {
                Ok(r) => r,
                Err(e) => return naming_plane_error("mint_contact_capability: resolve", e),
            };
            if !resolution.found {
                return (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response();
            }
            match resolution.identity_node_id.as_deref() {
                Some(node) => match pial_for_identity_node(&state, node).await {
                    Ok(owner) if owner == pial => {}
                    Ok(_) => return forbidden(),
                    Err(e) => return naming_plane_error("mint_contact_capability: owner", e),
                },
                None => return forbidden(),
            }
            let Some(node) = resolution.number_node_id.as_deref().and_then(parse_uuid) else {
                return naming_plane_error("mint_contact_capability", "Manhattan returned no number_node_id");
            };
            // A link scoped to a Number is the Number holder's to mint.
            match number_holder(&state, node, pial).await {
                Ok(Some(holder)) if holder == account => {}
                Ok(_) => return forbidden(),
                Err(e) => return db_error("mint_contact_capability: holder", e),
            }
            Some(node)
        }
        _ => None,
    };

    match contact::mint_capability(&state.pool, account, pial, number_node, label, max_uses).await {
        Ok(secret) => Json(json!({ "capability": secret, "label": label })).into_response(),
        Err(e) => {
            tracing::error!("mint_contact_capability: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── POST /v1/contact/capabilities/revoke ─────────────────────────────────────
/// Auth: X-Internal-Key
pub async fn revoke_contact_capability(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, _) =
        match acting_account(&state, body["account_id"].as_str(), "revoke_contact_capability").await {
            Ok(a) => a,
            Err(resp) => return resp,
        };
    let Some(capability_id) = body["capability_id"].as_str().and_then(parse_uuid) else {
        return bad("capability_id required");
    };
    match contact::revoke_capability(&state.pool, account, capability_id).await {
        Ok(0) => (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response(),
        Ok(_) => Json(json!({"ok": true})).into_response(),
        Err(e) => {
            tracing::error!("revoke_contact_capability: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── GET /v1/contact/capabilities/:account_id ─────────────────────────────────
/// The contact links one account minted.
/// Auth: X-Internal-Key
pub async fn list_contact_capabilities(
    State(state): State<AppState>,
    Path(account_ref): Path<String>,
) -> impl IntoResponse {
    let (account, _) =
        match acting_account(&state, Some(account_ref.as_str()), "list_contact_capabilities").await {
            Ok(a) => a,
            Err(resp) => return resp,
        };
    match contact::list_capabilities(&state.pool, account).await {
        Ok(rows) => Json(json!({
            "capabilities": rows.into_iter().map(|(id, label, number_node, uses, max_uses, revoked)| json!({
                "id": id.to_string(),
                "label": label,
                "number_node_id": number_node.map(|n| n.to_string()),
                "uses": uses,
                "max_uses": max_uses,
                "revoked": revoked,
            })).collect::<Vec<_>>()
        }))
        .into_response(),
        Err(e) => {
            tracing::error!("list_contact_capabilities: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── GET /v1/contact/requests/:account_id ─────────────────────────────────────
/// The pending contact requests addressed to one account, each naming the
/// account that asked — the persona that asked, never its person's primary.
/// Auth: X-Internal-Key
pub async fn list_contact_requests(
    State(state): State<AppState>,
    Path(account_ref): Path<String>,
) -> impl IntoResponse {
    let (account, _) =
        match acting_account(&state, Some(account_ref.as_str()), "list_contact_requests").await {
            Ok(a) => a,
            Err(resp) => return resp,
        };
    match contact::list_requests(&state.pool, account).await {
        Ok(rows) => Json(json!({
            "requests": rows.into_iter().map(|(id, requester, note, status, created_at)| json!({
                "id": id.to_string(),
                "requester_account": requester.to_string(),
                "note": note,
                "status": status,
                "created_at": created_at.to_rfc3339(),
            })).collect::<Vec<_>>()
        }))
        .into_response(),
        Err(e) => {
            tracing::error!("list_contact_requests: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

// ── POST /v1/contact/requests/decide ─────────────────────────────────────────
/// Accept or decline a contact request. Accepting writes the standing grant in
/// the same transaction, so an accepted request can never leave contact
/// unauthorised.
/// Auth: X-Internal-Key
pub async fn decide_contact_request(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, _) =
        match acting_account(&state, body["account_id"].as_str(), "decide_contact_request").await {
            Ok(a) => a,
            Err(resp) => return resp,
        };
    let Some(request_id) = body["request_id"].as_str().and_then(parse_uuid) else {
        return bad("request_id required");
    };
    let accept = body["accept"].as_bool().unwrap_or(false);
    match contact::decide_request(&state.pool, account, request_id, accept).await {
        Ok(Some(requester)) => Json(json!({
            "ok": true,
            "accepted": accept,
            "requester_account": requester.to_string(),
        }))
        .into_response(),
        Ok(None) => (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response(),
        Err(e) => {
            tracing::error!("decide_contact_request: {e}");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response()
        }
    }
}

/// List one account's contact addresses. Manhattan lists every address under
/// the identity and gates that read to this brain; only the addresses the
/// acting account minted are answered, so one persona never sees another's.
/// Auth: X-Internal-Key
pub async fn list_addresses(
    State(state): State<AppState>,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    let (account, pial) = match acting_account(&state, body["account_id"].as_str(), "list_addresses").await {
        Ok(a) => a,
        Err(resp) => return resp,
    };
    let identity_node_id = match identity_node_for(&state, pial).await {
        Ok(id) => id,
        Err(e) => return naming_plane_error("list_addresses: identity node", e),
    };
    let list = match state.manhattan.list_addresses(&identity_node_id).await {
        Ok(list) => list,
        Err(e) => return naming_plane_error("list_addresses", e),
    };
    let mut out: Vec<serde_json::Value> = Vec::with_capacity(list.len());
    for a in list {
        match contact::address_account(&state.pool, &a.address, pial).await {
            Ok(Some(holder)) if holder == account => {
                out.push(json!({"address": a.address, "status": a.status}));
            }
            Ok(_) => {}
            Err(e) => return db_error("list_addresses: holder", e),
        }
    }
    Json(json!({ "addresses": out })).into_response()
}
