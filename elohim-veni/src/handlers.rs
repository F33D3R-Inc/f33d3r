use axum::{
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    response::IntoResponse,
    Json,
};
use chrono::Utc;
use serde_json::json;
use sqlx::PgPool;
use tracing::{info, warn};
use uuid::Uuid;

use axum::extract::Query;

use crate::models::{
    BootstrapRequest, BootstrapResponse, BulkCapabilityResponse, BulkCapabilityUpdate,
    BanRequest, CapabilityEntry, CapabilityMap, CapabilityUpdateRequest,
    CapabilityUpdateResponse, DecisionLogEntry, DecisionLogQuery, DecisionRequest,
    DecisionResponse, StatsResponse, TrustResponse, UpdateStatusRequest, UpdateStatusResponse,
};
use crate::{db, decision};

// ── Shared application state ──────────────────────────────────────────────────

#[derive(Clone)]
pub struct AppState {
    pub pool:       PgPool,
    pub verity_url: String,
    pub http:       reqwest::Client,
}

// ── Helper: extract trusted caller from X-Elohim-PIAL header ─────────────────

fn trusted_caller(headers: &HeaderMap) -> Option<Uuid> {
    headers
        .get("x-elohim-pial")
        .and_then(|v| v.to_str().ok())
        .and_then(|s| Uuid::parse_str(s).ok())
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health() -> impl IntoResponse {
    Json(json!({ "status": "ok", "service": "elohim-veni" }))
}

// ── GET /v1/stats ─────────────────────────────────────────────────────────────

pub async fn stats(State(state): State<AppState>) -> impl IntoResponse {
    let pool = &state.pool;

    let total_pials     = db::count_pials(pool).await.unwrap_or(0);
    let total_decisions = db::count_decisions_total(pool).await.unwrap_or(0);
    let decisions_today = db::count_decisions_today(pool).await.unwrap_or(0);
    let denials_today   = db::count_denials_today(pool).await.unwrap_or(0);

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
        account_id = %req.account_id,
        caller = ?caller,
        "pial_bootstrap"
    );

    let tier = req.tier.as_deref().unwrap_or("BASIC");

    // Idempotent upsert: if account_id is already registered (even with a different pial_id),
    // update it to reflect the new canonical PIAL UUID from feed-engine.
    let row: (Uuid, bool) = match sqlx::query_as(
        "INSERT INTO pial_states (pial_id, account_id, tier) VALUES ($1, $2, $3)
         ON CONFLICT (account_id) DO UPDATE SET pial_id = EXCLUDED.pial_id, updated_at = NOW()
         RETURNING pial_id, (xmax = 0) AS inserted"
    )
    .bind(req.pial_id)
    .bind(req.account_id)
    .bind(tier)
    .fetch_one(&state.pool)
    .await
    {
        Ok(r) => r,
        Err(e) => {
            warn!(error = %e, "pial_bootstrap upsert failed");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(BootstrapResponse { pial_id: Uuid::nil(), created: false }),
            );
        }
    };

    let (pial_id, created) = row;

    // Seed trust score row (idempotent).
    let _ = sqlx::query(
        "INSERT INTO trust_scores (pial_id) VALUES ($1) ON CONFLICT DO NOTHING"
    )
    .bind(pial_id)
    .execute(&state.pool)
    .await;

    if created {
        info!(pial_id = %pial_id, "PIAL registered in security brain");
    }

    (
        StatusCode::CREATED,
        Json(BootstrapResponse { pial_id, created: true }),
    )
}

// ── GET /v1/pial/:pial_id/capabilities ───────────────────────────────────────

pub async fn get_capabilities(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
) -> impl IntoResponse {
    // Verify the PIAL exists.
    let exists: Option<(bool,)> = sqlx::query_as(
        "SELECT TRUE FROM pial_states WHERE pial_id = $1"
    )
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
         ORDER BY capability ASC"
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
            CapabilityEntry { capability, granted, expires_at }
        })
        .collect();

    (
        StatusCode::OK,
        Json(CapabilityMap { pial_id, capabilities }),
    )
        .into_response()
}

// ── POST /v1/pial/:pial_id/capabilities ──────────────────────────────────────

pub async fn update_capability(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    headers: HeaderMap,
    Json(req): Json<CapabilityUpdateRequest>,
) -> impl IntoResponse {
    let caller = trusted_caller(&headers);
    info!(
        pial_id = %pial_id,
        capability = %req.capability,
        grant = req.grant,
        caller = ?caller,
        "update_capability"
    );

    // Verify the PIAL exists.
    let exists: Option<(bool,)> = sqlx::query_as(
        "SELECT TRUE FROM pial_states WHERE pial_id = $1"
    )
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
    Path(pial_id): Path<Uuid>,
) -> impl IntoResponse {
    let row: Option<(f64, f64, i32, Option<String>, String)> = sqlx::query_as(
        "SELECT ts.score, ts.anomaly_score, ts.violation_count, ts.last_decision, ps.tier \
         FROM trust_scores ts \
         JOIN pial_states ps ON ps.pial_id = ts.pial_id \
         WHERE ts.pial_id = $1"
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
    let pial_row: Option<(String, String)> = sqlx::query_as(
        "SELECT status, tier FROM pial_states WHERE pial_id = $1"
    )
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
         WHERE pial_id = $1 AND capability = $2"
    )
    .bind(req.pial_id)
    .bind(&req.action)
    .fetch_optional(&state.pool)
    .await
    .unwrap_or(None);

    // Load current trust score.
    let trust_row: Option<(f64, f64, i32)> = sqlx::query_as(
        "SELECT score, anomaly_score, violation_count FROM trust_scores WHERE pial_id = $1"
    )
    .bind(req.pial_id)
    .fetch_optional(&state.pool)
    .await
    .unwrap_or(None);
    let (trust_score, stored_anomaly, violations) =
        trust_row.unwrap_or((1.0, 0.0, 0));

    // Verity attestation gate — consult KYC brain for age/creator gated actions.
    // Runs async, non-blocking. A Verity outage degrades to "no attestation" (not a hard fail).
    let verity_tier: i32 = {
        const VERITY_GATED: &[&str] = &["monetize", "adult_content", "node_relay", "payout_activation", "creator_signup"];
        if VERITY_GATED.contains(&req.action.as_str()) {
            let url = format!("{}/v1/profile/{}", state.verity_url, req.pial_id);
            match state.http.get(&url).send().await {
                Ok(resp) if resp.status().is_success() => {
                    resp.json::<serde_json::Value>().await
                        .ok()
                        .and_then(|v| v["creator_tier"].as_i64())
                        .unwrap_or(0) as i32
                }
                Ok(_) | Err(_) => {
                    warn!(pial_id = %req.pial_id, action = %req.action, "Verity unavailable — proceeding without attestation");
                    0
                }
            }
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
            pial_id:        req.pial_id,
            action:         &req.action,
            pial_status:    &status,
            tier:           &tier,
            cap_grant,
            trust_score,
            stored_anomaly,
            violations,
            content_risk:   req.context.content_risk,
            anomaly_score:  req.context.anomaly_score,
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
        "ALLOW"            =>  0.01_f64,
        "ALLOW_RESTRICTED" =>  0.0,
        "QUARANTINE"       => -0.05,
        "DENY"             => -0.10,
        _                  =>  0.0,
    };
    let new_score = (trust_score + delta).clamp(0.0, 1.0);
    let new_anomaly = ((stored_anomaly * 0.9) + (req.context.anomaly_score * 0.1)).clamp(0.0, 1.0);
    let new_violations = if outcome.decision == "DENY" { violations + 1 } else { violations };

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
            decision:            outcome.decision,
            confidence:          outcome.confidence,
            reason:              outcome.reason,
            capability_required: outcome.capability_required,
        }),
    )
        .into_response()
}

// ── POST /v1/pial/:pial_id/status ─────────────────────────────────────────────

pub async fn update_pial_status(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<UpdateStatusRequest>,
) -> impl IntoResponse {
    let valid = ["ACTIVE", "SUSPENDED", "REVOKED"];
    if !valid.contains(&req.status.as_str()) {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({ "error": "status must be ACTIVE | SUSPENDED | REVOKED" })),
        ).into_response();
    }

    let result = sqlx::query(
        "UPDATE pial_states SET status = $2, updated_at = NOW() WHERE pial_id = $1"
    )
    .bind(pial_id)
    .bind(&req.status)
    .execute(&state.pool)
    .await;

    match result {
        Ok(r) if r.rows_affected() > 0 => {
            info!(pial_id = %pial_id, status = %req.status, "PIAL status updated");
            (
                StatusCode::OK,
                Json(UpdateStatusResponse { pial_id, status: req.status }),
            ).into_response()
        }
        Ok(_) => (StatusCode::NOT_FOUND, Json(json!({ "error": "pial_not_found" }))).into_response(),
        Err(e) => {
            warn!(error = %e, "update_pial_status failed");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({ "error": "db_error" }))).into_response()
        }
    }
}

// ── POST /v1/pial/:pial_id/ban ────────────────────────────────────────────────
// Shorthand: REVOKE status + revoke all capabilities in one call (used by Zodacare).

pub async fn ban_pial(
    State(state): State<AppState>,
    Path(pial_id): Path<Uuid>,
    Json(req): Json<BanRequest>,
) -> impl IntoResponse {
    // Set status to REVOKED.
    let _ = sqlx::query(
        "UPDATE pial_states SET status = 'REVOKED', updated_at = NOW() WHERE pial_id = $1"
    )
    .bind(pial_id)
    .execute(&state.pool)
    .await;

    // Revoke all active capability grants.
    let _ = sqlx::query(
        "UPDATE capability_grants SET granted = FALSE, reason = $2, updated_at = NOW() \
         WHERE pial_id = $1 AND granted = TRUE"
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
    ).into_response()
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

        if result.is_ok() { updated += 1; }
    }

    info!(pial_id = %pial_id, updated = updated, "bulk capability update");
    (
        StatusCode::OK,
        Json(BulkCapabilityResponse { pial_id, updated }),
    ).into_response()
}

// ── GET /v1/decisions/log ─────────────────────────────────────────────────────

pub async fn decision_log(
    State(state): State<AppState>,
    Query(q): Query<DecisionLogQuery>,
) -> impl IntoResponse {
    let limit  = q.limit.unwrap_or(50).min(500);
    let offset = q.offset.unwrap_or(0);

    let rows: Result<Vec<(Uuid, Uuid, String, String, f64, String, Option<String>, f64, f64, chrono::DateTime<Utc>)>, _> =
        if let Some(pial_id) = q.pial_id {
            sqlx::query_as(
                "SELECT id, pial_id, action, decision, confidence, reason, \
                        capability_required, content_risk, anomaly_score, created_at \
                 FROM decision_log WHERE pial_id = $1 \
                 ORDER BY created_at DESC LIMIT $2 OFFSET $3"
            )
            .bind(pial_id).bind(limit).bind(offset)
            .fetch_all(&state.pool).await
        } else {
            sqlx::query_as(
                "SELECT id, pial_id, action, decision, confidence, reason, \
                        capability_required, content_risk, anomaly_score, created_at \
                 FROM decision_log \
                 ORDER BY created_at DESC LIMIT $1 OFFSET $2"
            )
            .bind(limit).bind(offset)
            .fetch_all(&state.pool).await
        };

    match rows {
        Ok(entries) => {
            let log: Vec<DecisionLogEntry> = entries.into_iter().map(|r| DecisionLogEntry {
                id: r.0, pial_id: r.1, action: r.2, decision: r.3,
                confidence: r.4, reason: r.5, capability_required: r.6,
                content_risk: r.7, anomaly_score: r.8, created_at: r.9,
            }).collect();
            (StatusCode::OK, Json(json!({ "log": log, "count": log.len() }))).into_response()
        }
        Err(e) => {
            warn!(error = %e, "decision_log query failed");
            (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({ "error": "db_error" }))).into_response()
        }
    }
}
