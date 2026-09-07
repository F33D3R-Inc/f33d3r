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
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use tracing::{info, warn};
use uuid::Uuid;

use axum::extract::Query;

use crate::models::{
    BanRequest, BootstrapRequest, BootstrapResponse, BulkCapabilityResponse, BulkCapabilityUpdate,
    CapabilityEntry, CapabilityMap, CapabilityUpdateRequest, CapabilityUpdateResponse,
    DecisionLogEntry, DecisionLogQuery, DecisionRequest, DecisionResponse, StatsResponse,
    TrustResponse, UpdateStatusRequest, UpdateStatusResponse,
};
use crate::number_lease::{self, Admission};
use crate::{contact, db, decision, follow_graph};
use manhattan_client::{ConflictCode, Manhattan, ManhattanError};

// ── Shared application state ──────────────────────────────────────────────────

#[derive(Clone)]
pub struct AppState {
    pub pool: PgPool,
    pub verity_url: String,
    pub http: reqwest::Client,
    /// Shared secret for X-Internal-Key auth on PIAL key endpoints.
    pub internal_api_key: String,
    /// In-memory nexus tier cache: pial_shard → (tier, cached_at). 5-minute TTL.
    pub nexus_tier_cache: Arc<Mutex<HashMap<String, (i16, Instant)>>>,
    /// The naming plane. Identity is resolved through it and never by joining
    /// into another brain's tables.
    pub manhattan: Manhattan,
    /// Signs assembled key bundles. Unconfigured means bundles ship unsigned and
    /// say so; it never means a bundle is presented as signed when it is not.
    pub bundle_signer: contact::BundleSigner,
}

const NEXUS_TIER_TTL: Duration = Duration::from_secs(300);

/// Derives pial_shard_id from a PIAL UUID string using the same formula as Nantar.
fn pial_shard_id(pial_id: &str) -> String {
    use sha2::{Digest, Sha256};
    hex::encode(Sha256::digest(
        format!("{}:nexus-pial-shard-v1", pial_id).as_bytes(),
    ))
}

/// Returns the NEXUS tier for a PIAL, or 0 if not linked to any NEXUS.
/// Checks in-memory cache first (5 min TTL). Falls back to Verity on miss.
async fn nexus_tier_for_pial(state: &AppState, pial_id: &str) -> i16 {
    let shard = pial_shard_id(pial_id);

    // Cache read
    {
        let cache = state
            .nexus_tier_cache
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        if let Some((tier, cached_at)) = cache.get(&shard) {
            if cached_at.elapsed() < NEXUS_TIER_TTL {
                return *tier;
            }
        }
    }

    // Cache miss — call Verity
    let url = format!("{}/v1/nexus/tier/{}", state.verity_url, shard);
    let tier: i16 = match state.http.get(&url).send().await {
        Ok(resp) if resp.status().is_success() => resp
            .json::<serde_json::Value>()
            .await
            .ok()
            .and_then(|v| v["tier"].as_i64())
            .map(|t| t as i16)
            .unwrap_or(0),
        Ok(_) | Err(_) => 0,
    };

    // Cache write
    {
        let mut cache = state
            .nexus_tier_cache
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        cache.insert(shard, (tier, Instant::now()));
    }

    tier
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

    match db::outbox_backlog(&state.pool).await {
        Ok(backlog) => Json(json!({
            "status":                              "ok",
            "service":                             "elohim-veni",
            "manhattan":                           configured,
            "manhattan_outbox_pending":            backlog.pending,
            "manhattan_outbox_oldest_age_seconds": backlog.oldest_age_seconds,
            "manhattan_outbox_head_error":         backlog.head_last_error,
            // Rows the drain gave up on: naming-plane writes that did not happen
            // and will not until an operator clears blocked_at.
            "manhattan_outbox_quarantined":        backlog.quarantined,
        })),
        Err(e) => {
            warn!(error = %e, "manhattan outbox backlog query failed");
            Json(json!({
                "status":                   "degraded",
                "service":                  "elohim-veni",
                "manhattan":                configured,
                "manhattan_outbox_pending": serde_json::Value::Null,
                "manhattan_outbox_error":   e.to_string(),
            }))
        }
    }
}

// ── GET /v1/stats ─────────────────────────────────────────────────────────────

pub async fn stats(State(state): State<AppState>) -> impl IntoResponse {
    let pool = &state.pool;

    let total_pials = db::count_pials(pool).await.unwrap_or(0);
    let total_decisions = db::count_decisions_total(pool).await.unwrap_or(0);
    let decisions_today = db::count_decisions_today(pool).await.unwrap_or(0);
    let denials_today = db::count_denials_today(pool).await.unwrap_or(0);

    let deny_rate_today = if decisions_today == 0 {
        0.0
    } else {
        denials_today as f64 / decisions_today as f64
    };

    Json(StatsResponse {
        service: "elohim-veni",
        total_pials,
        total_decisions,
        decisions_today,
        deny_rate_today,
    })
}

// ── POST /v1/pial/bootstrap ───────────────────────────────────────────────────

pub async fn pial_bootstrap(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<BootstrapRequest>,
) -> impl IntoResponse {
    let caller = trusted_caller(&headers);
    info!(
        pial_id = %req.pial_id,
        caller = ?caller,
        "pial_bootstrap"
    );

    let tier = req.tier.as_deref().unwrap_or("BASIC");

    // Idempotent upsert keyed on the PIAL, which is the identity itself.
    //
    // This used to conflict on account_id — feed-engine's users.id, another
    // brain's row id carried here as a second identity key — which let a
    // re-bootstrap move a PIAL out from under an account. A PIAL is the root
    // that never moves, so the only key a person is filed under here is their
    // PIAL. The account mapping lives in f33d3r_feed, where the account is.
    //
    // The trigger on this table queues the identity's registration into the
    // naming plane in this same transaction: either the PIAL exists here and is
    // queued for Manhattan, or neither happened.
    let row: (Uuid, bool) = match sqlx::query_as(
        "INSERT INTO pial_states (pial_id, tier) VALUES ($1, $2)
         ON CONFLICT (pial_id) DO UPDATE SET updated_at = NOW()
         RETURNING pial_id, (xmax = 0) AS inserted",
    )
    .bind(req.pial_id)
    .bind(tier)
    .fetch_one(&state.pool)
    .await
    {
        Ok(r) => r,
        Err(e) => {
            warn!(error = %e, "pial_bootstrap upsert failed");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(BootstrapResponse {
                    pial_id: Uuid::nil(),
                    created: false,
                }),
            );
        }
    };

    let (pial_id, created) = row;

    // Seed trust score row (idempotent).
    let _ = sqlx::query("INSERT INTO trust_scores (pial_id) VALUES ($1) ON CONFLICT DO NOTHING")
        .bind(pial_id)
        .execute(&state.pool)
        .await;

    if created {
        info!(pial_id = %pial_id, "PIAL asserted — identity queued for the naming plane");
    }

    let status = if created {
        StatusCode::CREATED
    } else {
        StatusCode::OK
    };
    (status, Json(BootstrapResponse { pial_id, created }))
}

// ── GET /v1/pial/:pial_id/capabilities ───────────────────────────────────────

pub async fn get_capabilities(
    State(state): State<AppState>,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    let pial_id = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };

    // Verify the PIAL exists.
    let exists: Option<(bool,)> = sqlx::query_as("SELECT TRUE FROM pial_states WHERE pial_id = $1")
        .bind(pial_id)
        .fetch_optional(&state.pool)
        .await
        .unwrap_or(None);

    if exists.is_none() {
        return (
            StatusCode::NOT_FOUND,
            Json(json!({ "error": "pial_not_found" })),
        )
            .into_response();
    }

    // Fetch all explicit grants (expired ones are included but flagged).
    let rows: Vec<(String, bool, Option<chrono::DateTime<Utc>>)> = sqlx::query_as(
        "SELECT capability, granted, expires_at \
         FROM capability_grants \
         WHERE pial_id = $1 \
         ORDER BY capability ASC",
    )
    .bind(pial_id)
    .fetch_all(&state.pool)
    .await
    .unwrap_or_default();

    let now = Utc::now();
    let capabilities: Vec<CapabilityEntry> = rows
        .into_iter()
        .map(|(capability, mut granted, expires_at)| {
            // An expired grant is treated as revoked.
            if let Some(exp) = expires_at {
                if exp < now {
                    granted = false;
                }
            }
            CapabilityEntry {
                capability,
                granted,
                expires_at,
            }
        })
        .collect();

    (
        StatusCode::OK,
        Json(CapabilityMap {
            pial_id,
            capabilities,
        }),
    )
        .into_response()
}

// ── POST /v1/pial/:pial_id/capabilities ──────────────────────────────────────

pub async fn update_capability(
    State(state): State<AppState>,
    Path(pial_ref): Path<String>,
    headers: HeaderMap,
    Json(req): Json<CapabilityUpdateRequest>,
) -> impl IntoResponse {
    let pial_id = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };

    let caller = trusted_caller(&headers);
    info!(
        pial_id = %pial_id,
        capability = %req.capability,
        grant = req.grant,
        caller = ?caller,
        "update_capability"
    );

    // Verify the PIAL exists.
    let exists: Option<(bool,)> = sqlx::query_as("SELECT TRUE FROM pial_states WHERE pial_id = $1")
        .bind(pial_id)
        .fetch_optional(&state.pool)
        .await
        .unwrap_or(None);

    if exists.is_none() {
        return (
            StatusCode::NOT_FOUND,
            Json(json!({ "error": "pial_not_found" })),
        )
            .into_response();
    }

    let result = sqlx::query(
        "INSERT INTO capability_grants (pial_id, capability, granted, granted_by, reason, expires_at) \
         VALUES ($1, $2, $3, $4, $5, $6) \
         ON CONFLICT (pial_id, capability) DO UPDATE \
           SET granted = EXCLUDED.granted, \
               granted_by = EXCLUDED.granted_by, \
               reason = EXCLUDED.reason, \
               expires_at = EXCLUDED.expires_at, \
               updated_at = NOW()"
    )
    .bind(pial_id)
    .bind(&req.capability)
    .bind(req.grant)
    .bind(req.granted_by)
    .bind(&req.reason)
    .bind(req.expires_at)
    .execute(&state.pool)
    .await;

    match result {
        Ok(_) => (
            StatusCode::OK,
            Json(CapabilityUpdateResponse {
                pial_id,
                capability: req.capability,
                granted: req.grant,
            }),
        )
            .into_response(),
        Err(e) => {
            warn!(error = %e, "capability update failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── GET /v1/pial/:pial_id/trust ───────────────────────────────────────────────

pub async fn get_trust(
    State(state): State<AppState>,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    let pial_id = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };

    let row: Option<(f64, f64, i32, Option<String>, String)> = sqlx::query_as(
        "SELECT ts.score, ts.anomaly_score, ts.violation_count, ts.last_decision, ps.tier \
         FROM trust_scores ts \
         JOIN pial_states ps ON ps.pial_id = ts.pial_id \
         WHERE ts.pial_id = $1",
    )
    .bind(pial_id)
    .fetch_optional(&state.pool)
    .await
    .unwrap_or(None);

    match row {
        None => (
            StatusCode::NOT_FOUND,
            Json(json!({ "error": "pial_not_found" })),
        )
            .into_response(),
        Some((score, anomaly_score, violation_count, last_decision, tier)) => (
            StatusCode::OK,
            Json(TrustResponse {
                pial_id,
                score,
                anomaly_score,
                violation_count,
                tier,
                last_decision,
            }),
        )
            .into_response(),
    }
}

// ── POST /v1/decisions ────────────────────────────────────────────────────────

pub async fn make_decision(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<DecisionRequest>,
) -> impl IntoResponse {
    let caller = trusted_caller(&headers);
    info!(
        pial_id = %req.pial_id,
        action  = %req.action,
        caller  = ?caller,
        "make_decision"
    );

    // Load PIAL state.
    let pial_row: Option<(String, String)> =
        sqlx::query_as("SELECT status, tier FROM pial_states WHERE pial_id = $1")
            .bind(req.pial_id)
            .fetch_optional(&state.pool)
            .await
            .unwrap_or(None);

    let (status, tier) = match pial_row {
        None => {
            return (
                StatusCode::NOT_FOUND,
                Json(json!({ "error": "pial_not_found" })),
            )
                .into_response();
        }
        Some(r) => r,
    };

    // Load capability grants for this action (if any explicit entry exists).
    let cap_grant: Option<(bool, Option<chrono::DateTime<Utc>>)> = sqlx::query_as(
        "SELECT granted, expires_at FROM capability_grants \
         WHERE pial_id = $1 AND capability = $2",
    )
    .bind(req.pial_id)
    .bind(&req.action)
    .fetch_optional(&state.pool)
    .await
    .unwrap_or(None);

    // Load current trust score.
    let trust_row: Option<(f64, f64, i32)> = sqlx::query_as(
        "SELECT score, anomaly_score, violation_count FROM trust_scores WHERE pial_id = $1",
    )
    .bind(req.pial_id)
    .fetch_optional(&state.pool)
    .await
    .unwrap_or(None);
    let (trust_score, stored_anomaly, violations) = trust_row.unwrap_or((1.0, 0.0, 0));

    // Verity attestation gate — consult KYC brain for age/creator gated actions.
    // Uses NEXUS tier (shared across all linked personas) when available,
    // falling back to the PIAL-level attestation tier.
    // Runs async, non-blocking. A Verity outage degrades to "no attestation" (not a hard fail).
    let verity_tier: i32 = {
        const VERITY_GATED: &[&str] = &[
            "monetize",
            "adult_content",
            "node_relay",
            "payout_activation",
            "creator_signup",
        ];
        if VERITY_GATED.contains(&req.action.as_str()) {
            // 1. NEXUS tier (shared across personas — preferred when linked)
            let nexus_tier = nexus_tier_for_pial(&state, &req.pial_id.to_string()).await as i32;

            // 2. PIAL attestation tier (per-account fallback)
            let pial_tier: i32 = {
                let url = format!("{}/v1/profile/{}", state.verity_url, req.pial_id);
                match state.http.get(&url).send().await {
                    Ok(resp) if resp.status().is_success() => {
                        resp.json::<serde_json::Value>()
                            .await
                            .ok()
                            .and_then(|v| v["creator_tier"].as_i64())
                            .unwrap_or(0) as i32
                    }
                    Ok(_) | Err(_) => {
                        warn!(pial_id = %req.pial_id, action = %req.action, "Verity unavailable — proceeding without attestation");
                        0
                    }
                }
            };

            // Use the higher of nexus tier and pial tier.
            nexus_tier.max(pial_tier)
        } else {
            -1 // sentinel: action is not verity-gated
        }
    };

    // If action is verity-gated and user doesn't meet the minimum tier, DENY immediately.
    let verity_denial: Option<decision::EvalOutput> = match req.action.as_str() {
        "monetize" | "payout_activation" if verity_tier >= 0 && verity_tier < 3 => {
            Some(decision::EvalOutput {
                decision: "DENY".into(),
                confidence: 1.0,
                reason: format!("Verity tier {} — creator monetization requires tier 3 (complete KYC + age verification)", verity_tier),
                capability_required: Some("creator_verified".into()),
            })
        }
        "adult_content" | "creator_signup" if verity_tier >= 0 && verity_tier < 2 => {
            Some(decision::EvalOutput {
                decision: "DENY".into(),
                confidence: 1.0,
                reason: format!("Verity tier {} — adult content requires tier 2 (age verification)", verity_tier),
                capability_required: Some("age_verified".into()),
            })
        }
        _ => None,
    };

    // Run the deterministic decision engine (skipped if Verity already denied).
    let outcome = if let Some(denial) = verity_denial {
        denial
    } else {
        decision::evaluate(decision::EvalInput {
            pial_id: req.pial_id,
            action: &req.action,
            pial_status: &status,
            tier: &tier,
            cap_grant,
            trust_score,
            stored_anomaly,
            violations,
            content_risk: req.context.content_risk,
            anomaly_score: req.context.anomaly_score,
        })
    };

    // Audit-log the decision (best-effort, never fail the response).
    let _ = sqlx::query(
        "INSERT INTO decision_log \
         (pial_id, action, decision, confidence, reason, capability_required, content_risk, anomaly_score) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)"
    )
    .bind(req.pial_id)
    .bind(&req.action)
    .bind(&outcome.decision)
    .bind(outcome.confidence)
    .bind(&outcome.reason)
    .bind(&outcome.capability_required)
    .bind(req.context.content_risk)
    .bind(req.context.anomaly_score)
    .execute(&state.pool)
    .await;

    // Update trust score based on outcome.
    let delta = match outcome.decision.as_str() {
        "ALLOW" => 0.01_f64,
        "ALLOW_RESTRICTED" => 0.0,
        "QUARANTINE" => -0.05,
        "DENY" => -0.10,
        _ => 0.0,
    };
    let new_score = (trust_score + delta).clamp(0.0, 1.0);
    let new_anomaly = ((stored_anomaly * 0.9) + (req.context.anomaly_score * 0.1)).clamp(0.0, 1.0);
    let new_violations = if outcome.decision == "DENY" {
        violations + 1
    } else {
        violations
    };

    let _ = sqlx::query(
        "UPDATE trust_scores \
         SET score = $2, anomaly_score = $3, violation_count = $4, last_decision = $5, updated_at = NOW() \
         WHERE pial_id = $1"
    )
    .bind(req.pial_id)
    .bind(new_score)
    .bind(new_anomaly)
    .bind(new_violations)
    .bind(&outcome.decision)
    .execute(&state.pool)
    .await;

    (
        StatusCode::OK,
        Json(DecisionResponse {
            decision: outcome.decision,
            confidence: outcome.confidence,
            reason: outcome.reason,
            capability_required: outcome.capability_required,
        }),
    )
        .into_response()
}

// ── POST /v1/pial/:pial_id/status ─────────────────────────────────────────────

pub async fn update_pial_status(
    State(state): State<AppState>,
    Path(pial_ref): Path<String>,
    Json(req): Json<UpdateStatusRequest>,
) -> impl IntoResponse {
    let pial_id = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };

    let valid = ["ACTIVE", "SUSPENDED", "REVOKED"];
    if !valid.contains(&req.status.as_str()) {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "error": "status must be ACTIVE | SUSPENDED | REVOKED" })),
        )
            .into_response();
    }

    let result =
        sqlx::query("UPDATE pial_states SET status = $2, updated_at = NOW() WHERE pial_id = $1")
            .bind(pial_id)
            .bind(&req.status)
            .execute(&state.pool)
            .await;

    match result {
        Ok(r) if r.rows_affected() > 0 => {
            info!(
                pial_id = %pial_id,
                status = %req.status,
                updated_by = ?req.updated_by,
                "PIAL status updated"
            );

            // A status change is an assertion about an identity, and this brain is
            // where identity is asserted — so it belongs in the same immutable log
            // every other decision does. The caller has always supplied a reason
            // and an author with this request; until now both were accepted and
            // dropped, which left the one kind of decision nobody could audit.
            let reason = match (req.reason.as_deref(), req.updated_by.as_deref()) {
                (Some(why), Some(by)) => format!("{why} (by {by})"),
                (Some(why), None) => why.to_string(),
                (None, Some(by)) => format!("status set to {} by {by}", req.status),
                (None, None) => format!("status set to {}", req.status),
            };
            let decision = if req.status == "ACTIVE" {
                "ALLOW"
            } else {
                "DENY"
            };

            if let Err(e) = sqlx::query(
                "INSERT INTO decision_log \
                 (pial_id, action, decision, confidence, reason) \
                 VALUES ($1, 'status_change', $2, 1.0, $3)",
            )
            .bind(pial_id)
            .bind(decision)
            .bind(&reason)
            .execute(&state.pool)
            .await
            {
                // The status is already committed, so this cannot fail the
                // response — but an unlogged enforcement action is a hole in an
                // audit trail that claims to be complete, and it says so.
                warn!(
                    error = %e,
                    pial_id = %pial_id,
                    status = %req.status,
                    "PIAL status change applied but NOT audit-logged"
                );
            }

            (
                StatusCode::OK,
                Json(UpdateStatusResponse {
                    pial_id,
                    status: req.status,
                }),
            )
                .into_response()
        }
        Ok(_) => (
            StatusCode::NOT_FOUND,
            Json(json!({ "error": "pial_not_found" })),
        )
            .into_response(),
        Err(e) => {
            warn!(error = %e, "update_pial_status failed");
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({ "error": "db_error" })),
            )
                .into_response()
        }
    }
}

// ── POST /v1/pial/:pial_id/ban ────────────────────────────────────────────────
// Shorthand: REVOKE status + revoke all capabilities in one call (used by Zodacare).

pub async fn ban_pial(
    State(state): State<AppState>,
    Path(pial_ref): Path<String>,
    Json(req): Json<BanRequest>,
) -> impl IntoResponse {
    let pial_id = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };

    // Set status to REVOKED.
    let _ = sqlx::query(
        "UPDATE pial_states SET status = 'REVOKED', updated_at = NOW() WHERE pial_id = $1",
    )
    .bind(pial_id)
    .execute(&state.pool)
    .await;

    // Revoke all active capability grants.
    let _ = sqlx::query(
        "UPDATE capability_grants SET granted = FALSE, reason = $2, updated_at = NOW() \
         WHERE pial_id = $1 AND granted = TRUE",
    )
    .bind(pial_id)
    .bind(format!("Ban: {}", req.reason))
    .execute(&state.pool)
    .await;

    // Audit log.
    let _ = sqlx::query(
        "INSERT INTO decision_log \
         (pial_id, action, decision, confidence, reason, capability_required, content_risk, anomaly_score) \
         VALUES ($1, 'ban', 'DENY', 1.0, $2, NULL, 0.0, 0.0)"
    )
    .bind(pial_id)
    .bind(&req.reason)
    .execute(&state.pool)
    .await;

    info!(pial_id = %pial_id, applied_by = ?req.applied_by, "PIAL banned — all capabilities revoked");

    (
        StatusCode::OK,
        Json(json!({
            "pial_id": pial_id,
            "status":  "REVOKED",
            "message": "PIAL banned and all capabilities revoked"
        })),
    )
        .into_response()
}

// ── POST /v1/pial/capabilities/bulk ──────────────────────────────────────────

pub async fn bulk_update_capabilities(
    State(state): State<AppState>,
    Json(req): Json<BulkCapabilityUpdate>,
) -> impl IntoResponse {
    let pial_id = req.pial_id;
    let mut updated = 0usize;

    for cap in &req.capabilities {
        let result = sqlx::query(
            "INSERT INTO capability_grants (pial_id, capability, granted, granted_by, reason, expires_at) \
             VALUES ($1, $2, $3, $4, $5, $6) \
             ON CONFLICT (pial_id, capability) DO UPDATE \
               SET granted = EXCLUDED.granted, \
                   granted_by = EXCLUDED.granted_by, \
                   reason = EXCLUDED.reason, \
                   expires_at = EXCLUDED.expires_at, \
                   updated_at = NOW()"
        )
        .bind(pial_id)
        .bind(&cap.capability)
        .bind(cap.grant)
        .bind(cap.granted_by)
        .bind(&cap.reason)
        .bind(cap.expires_at)
        .execute(&state.pool)
        .await;

        if result.is_ok() {
            updated += 1;
        }
    }

    info!(pial_id = %pial_id, updated = updated, "bulk capability update");
    (
        StatusCode::OK,
        Json(BulkCapabilityResponse { pial_id, updated }),
    )
        .into_response()
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

// ── GET /v1/nexus/tier/:shard ─────────────────────────────────────────────────
// Proxy to Verity's NEXUS tier endpoint. Elohim Veni is the single trusted
// gateway from Nantar to NEXUS state — no other brain calls Verity directly.
pub async fn nexus_tier(
    State(state): State<AppState>,
    Path(shard): Path<String>,
) -> impl IntoResponse {
    let url = format!("{}/v1/nexus/tier/{}", state.verity_url, shard);
    match state.http.get(&url).send().await {
        Ok(resp) => {
            let status = resp.status();
            match resp.json::<serde_json::Value>().await {
                Ok(body) => (
                    StatusCode::from_u16(status.as_u16()).unwrap_or(StatusCode::OK),
                    Json(body),
                )
                    .into_response(),
                Err(_) => (
                    StatusCode::BAD_GATEWAY,
                    Json(json!({ "error": "verity_parse_error" })),
                )
                    .into_response(),
            }
        }
        Err(e) => {
            warn!(error = %e, "verity nexus_tier request failed");
            (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(json!({ "tier": 0, "tier_source": "unavailable", "is_suspended": false })),
            )
                .into_response()
        }
    }
}

// ── Internal key guard ────────────────────────────────────────────────────────

fn internal_key_ok(headers: &HeaderMap, cfg_key: &str) -> bool {
    headers
        .get("X-Internal-Key")
        .and_then(|v| v.to_str().ok())
        .map(|k| !cfg_key.is_empty() && k == cfg_key)
        .unwrap_or(false)
}

// ── POST /v1/pial/signing-key/register ───────────────────────────────────────
/// Register or update a PIAL's ECDSA-P256 signing public key.
/// Body: {"pial_id": "<uuid>", "public_key_b64": "...", "algorithm": "ECDSA-P256"}
/// Auth: X-Internal-Key
pub async fn register_signing_key(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response();
    }
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
    headers: HeaderMap,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response();
    }
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
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response();
    }
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
    headers: HeaderMap,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response();
    }
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
pub async fn mint_address(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response();
    }
    let identity_node_id = body["identity_node_id"].as_str().unwrap_or_default();
    if identity_node_id.is_empty() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error":"identity_node_id required"})),
        )
            .into_response();
    }
    match state.manhattan.mint_address(identity_node_id).await {
        Ok(addr) => Json(json!({"address": addr.address, "status": addr.status})).into_response(),
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
/// Ownership is re-checked here, not trusted from the caller.
/// Auth: X-Internal-Key
pub async fn revoke_address(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response();
    }
    let address = body["address"].as_str().unwrap_or_default();
    let identity_node_id = body["identity_node_id"].as_str().unwrap_or_default();
    if address.is_empty() || identity_node_id.is_empty() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error":"address and identity_node_id required"})),
        )
            .into_response();
    }

    let target = match state.manhattan.resolve_address(address).await {
        Ok(t) => t,
        Err(e) => {
            tracing::warn!("revoke_address: resolving {address}: {e}");
            return (StatusCode::NOT_FOUND, Json(json!({"error":"not_found"}))).into_response();
        }
    };
    if target.identity_node_id.as_deref() != Some(identity_node_id) {
        return (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response();
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

// ── POST /v1/numbers/mint ────────────────────────────────────────────────────
/// Allocate a Number for an identity and attach its policy.
/// Auth: X-Internal-Key
pub async fn mint_number(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
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
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
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

    if let Err(e) = state.manhattan.revoke_number(number).await {
        return naming_plane_error("revoke_number", e);
    }
    if let Some(node) = parse_uuid(number_node) {
        if let Err(e) = contact::drop_number_policy(&state.pool, node, pial).await {
            tracing::error!("revoke_number: dropping policy: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response();
        }
    }
    Json(json!({"ok": true})).into_response()
}

// ── GET /v1/numbers/:pial_id ─────────────────────────────────────────────────
/// The identity's own Numbers, live and retired, each with its policy.
/// Auth: X-Internal-Key
pub async fn list_numbers(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let pial = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };
    let node_id = match identity_node_for(&state, pial).await {
        Ok(id) => id,
        Err(e) => return naming_plane_error("list_numbers: identity node", e),
    };
    let entries = match state.manhattan.list_numbers(&node_id).await {
        Ok(e) => e,
        Err(e) => return naming_plane_error("list_numbers", e),
    };
    let policies = match contact::number_policies_for(&state.pool, pial).await {
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
    // by the owner's own PIAL behind the internal key, and nothing here is ever
    // reachable from a resolve.
    let leases = match number_lease::leases_for(&state.pool, pial).await {
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
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
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
    match number_lease::attach(
        &state.pool,
        number_node,
        pial,
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

// ── GET /v1/contact/policy/:pial_id ──────────────────────────────────────────
/// Auth: X-Internal-Key
pub async fn get_contact_policy(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let pial = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };
    match contact::get_contact_policy(&state.pool, pial).await {
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
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
    };
    let (existing_handle, existing_number) =
        match contact::get_contact_policy(&state.pool, pial).await {
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
    match contact::set_contact_policy(&state.pool, pial, handle_policy, number_policy).await {
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
/// The decision. Given a Number (or a target PIAL, for the @handle path) and the
/// follow-graph facts asserted by the brain that owns the follow graph, decide
/// whether contact may proceed.
///
/// `target_pial` is returned ONLY on allow. Every other outcome carries the same
/// coarse reason, so this endpoint cannot be used to test whether a Number exists.
/// Auth: X-Internal-Key
pub async fn evaluate_contact(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(initiator) = body["initiator_pial"].as_str().and_then(parse_uuid) else {
        return bad("initiator_pial required");
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

    // Resolve the target: by Number, or by PIAL on the @handle path.
    let (owner, number_node): (Uuid, Option<Uuid>) = if via_number {
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
        (
            owner,
            resolution.number_node_id.as_deref().and_then(parse_uuid),
        )
    } else {
        let Some(target) = body["target_pial"].as_str().and_then(parse_uuid) else {
            return bad("number or target_pial required");
        };
        (target, None)
    };

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

    // Policy: the Number's own, else the identity's default for that channel.
    let (handle_policy, default_number_policy) =
        match contact::get_contact_policy(&state.pool, owner).await {
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

    let granted = match contact::has_grant(&state.pool, owner, initiator).await {
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
        match contact::redeem_capability(&state.pool, owner, &presented_capability, number_node)
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
            match number_lease::record_admission(&state.pool, node, initiator).await {
                // Spent, already spent by this same identity, or a Number
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
            contact::grant_contact(&state.pool, owner, initiator, "capability", number_node).await
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
        match contact::open_request(&state.pool, owner, initiator, number_node, note).await {
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
        out["target_pial"] = json!(owner.to_string());
        // A Number that quietly stopped working is the worst failure this
        // feature has, so the moment it stops is the moment its owner hears
        // about it — through the one notification pipeline feed-engine already
        // has, addressed to the identity named here.
        //
        // It rides on an ALLOW and nowhere else. An allow has already disclosed
        // the target to the calling brain, so nothing new crosses; a refusal
        // discloses nothing, and attaching "that Number is exhausted" to one
        // would hand over the single fact the refusal exists to withhold.
        if let Some(alert) = budget_alert.as_str() {
            out["budget_alert"] = json!(alert);
            out["budget_owner_pial"] = json!(owner.to_string());
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
        out["request_owner_pial"] = json!(owner.to_string());
    }
    Json(out).into_response()
}

// ── POST /v1/contact/keybundle ───────────────────────────────────────────────
/// Assemble and sign the identity's messaging key bundle.
///
/// `signed` is false when no service signing key is configured; the response never
/// implies a signature it does not carry. The signature covers transport
/// tampering only — it is this service's key, not the identity's.
/// Auth: X-Internal-Key
pub async fn contact_key_bundle(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
    };

    let devices = match contact::messaging_keys(&state.pool, pial).await {
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
        // No key material exists for this identity, so no channel can be opened.
        return (
            StatusCode::CONFLICT,
            Json(json!({"error":"no_key_material"})),
        )
            .into_response();
    }

    let signing_pubkey_b64 = match db::get_signing_key(&state.pool, pial).await {
        Ok(k) => k,
        Err(e) => {
            tracing::error!("contact_key_bundle: signing key: {e}");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(json!({"error":"db_error"})),
            )
                .into_response();
        }
    };

    let bundle = contact::KeyBundle {
        pial_id: pial.to_string(),
        issued_at: Utc::now().to_rfc3339(),
        signing_algorithm: signing_pubkey_b64
            .as_ref()
            .map(|_| "ECDSA-P256".to_string()),
        signing_pubkey_b64,
        devices,
    };

    // Sign the Value, not the struct: the response ships the bundle through
    // json!, which reorders keys via Value's BTreeMap. Signing to_vec(&bundle)
    // would cover declaration-order bytes the client never receives.
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
/// Register the X25519 public key Gnosis seals to. Public halves only.
/// Auth: X-Internal-Key
pub async fn register_messaging_key(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
    };
    let public_key_b64 = body["public_key_b64"].as_str().unwrap_or_default();
    if public_key_b64.is_empty() || public_key_b64.len() > 512 {
        return bad("public_key_b64 required");
    }
    let device_id = body["device_id"].as_str().unwrap_or_default();
    if device_id.len() > 128 {
        return bad("device_id too long");
    }
    match contact::upsert_messaging_key(&state.pool, pial, device_id, public_key_b64).await {
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
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
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
            resolution.number_node_id.as_deref().and_then(parse_uuid)
        }
        _ => None,
    };

    match contact::mint_capability(&state.pool, pial, number_node, label, max_uses).await {
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
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
    };
    let Some(capability_id) = body["capability_id"].as_str().and_then(parse_uuid) else {
        return bad("capability_id required");
    };
    match contact::revoke_capability(&state.pool, pial, capability_id).await {
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

// ── GET /v1/contact/capabilities/:pial_id ────────────────────────────────────
/// Auth: X-Internal-Key
pub async fn list_contact_capabilities(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let pial = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };
    match contact::list_capabilities(&state.pool, pial).await {
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

// ── GET /v1/contact/requests/:pial_id ────────────────────────────────────────
/// The identity's own pending contact requests.
/// Auth: X-Internal-Key
pub async fn list_contact_requests(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(pial_ref): Path<String>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let pial = match resolve_pial_ref(&state, &pial_ref).await {
        Ok(id) => id,
        Err((status, body)) => return (status, body).into_response(),
    };
    match contact::list_requests(&state.pool, pial).await {
        Ok(rows) => Json(json!({
            "requests": rows.into_iter().map(|(id, requester, note, status, created_at)| json!({
                "id": id.to_string(),
                "requester_pial": requester.to_string(),
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
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return forbidden();
    }
    let Some(pial) = body["pial_id"].as_str().and_then(parse_uuid) else {
        return bad("pial_id required");
    };
    let Some(request_id) = body["request_id"].as_str().and_then(parse_uuid) else {
        return bad("request_id required");
    };
    let accept = body["accept"].as_bool().unwrap_or(false);
    match contact::decide_request(&state.pool, pial, request_id, accept).await {
        Ok(Some(requester)) => Json(json!({
            "ok": true,
            "accepted": accept,
            "requester_pial": requester.to_string(),
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

/// List an identity's contact addresses. Manhattan gates this read to the owning
/// brain, so it cannot be called from Nantar directly.
/// Auth: X-Internal-Key
pub async fn list_addresses(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<serde_json::Value>,
) -> impl IntoResponse {
    if !internal_key_ok(&headers, &state.internal_api_key) {
        return (StatusCode::FORBIDDEN, Json(json!({"error":"forbidden"}))).into_response();
    }
    let identity_node_id = body["identity_node_id"].as_str().unwrap_or_default();
    if identity_node_id.is_empty() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({"error":"identity_node_id required"})),
        )
            .into_response();
    }
    match state.manhattan.list_addresses(identity_node_id).await {
        Ok(list) => {
            let out: Vec<serde_json::Value> = list
                .into_iter()
                .map(|a| json!({"address": a.address, "status": a.status}))
                .collect();
            Json(json!({ "addresses": out })).into_response()
        }
        Err(e) => {
            tracing::error!("list_addresses for {identity_node_id}: {e}");
            (
                StatusCode::BAD_GATEWAY,
                Json(json!({"error":"naming_plane_error"})),
            )
                .into_response()
        }
    }
}
