// nexus.rs — NEXUS multi-persona identity layer for Verity
// Security contract: nexus_id is NEVER returned to HTTP clients.
// Only tier, result, and request_id are returned.

use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::Json,
    routing::{delete, get, post},
    Router,
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sqlx::PgPool;
use std::sync::Arc;
use uuid::Uuid;

#[derive(Clone)]
pub struct NexusState {
    pub pool: PgPool,
}

type NexusRes<T> = Result<Json<T>, (StatusCode, Json<Value>)>;

fn err(status: StatusCode, msg: &str) -> (StatusCode, Json<Value>) {
    (status, Json(json!({ "error": msg })))
}

// ── Request / response types ──────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct VerifyReq {
    pial_shard_id:  String,
    biometric_hash: String,
    document_hash:  String,
    confidence:     f64,
    #[serde(default)]
    jurisdiction:   Option<String>,
}

#[derive(Serialize)]
pub struct VerifyResp {
    result:     String,   // "created" | "already_linked" | "match_found"
    tier:       i16,
    #[serde(skip_serializing_if = "Option::is_none")]
    request_id: Option<String>,
}

#[derive(Deserialize)]
pub struct LinkActionReq {
    request_id:   String,
    pial_shard_id: String,
}

#[derive(Deserialize)]
pub struct InitiateLinkReq {
    source_pial_shard_id: String,
    target_pial_shard_id: String,
    persona_type:         String,
    #[serde(default)]
    display_label:        Option<String>,
}

#[derive(Deserialize)]
pub struct UnlinkQuery {
    pial_shard_id: String,
}

// ── Router ────────────────────────────────────────────────────────────────────

pub fn nexus_router(state: Arc<NexusState>) -> Router {
    Router::new()
        .route("/v1/nexus/verify",                post(verify))
        .route("/v1/nexus/link/confirm",          post(link_confirm))
        .route("/v1/nexus/link/decline",          post(link_decline))
        .route("/v1/nexus/link/initiate",         post(link_initiate))
        .route("/v1/nexus/link/pending",          get(link_pending))
        .route("/v1/nexus/personas",              get(list_personas))
        .route("/v1/nexus/personas/:shard",       delete(unlink_persona))
        .route("/v1/nexus/tier/:shard",           get(get_tier))
        .route("/v1/nexus/aml/:shard",            get(aml_get).post(aml_record))
        .with_state(state)
}

// ── POST /v1/nexus/verify ─────────────────────────────────────────────────────
// Create or find NEXUS identity, link current PIAL.
// SECURITY: nexus_id is never returned in the response.
async fn verify(
    State(state): State<Arc<NexusState>>,
    Json(req): Json<VerifyReq>,
) -> NexusRes<VerifyResp> {
    if req.confidence < 0.85 {
        return Err(err(StatusCode::UNPROCESSABLE_ENTITY, "biometric confidence below threshold"));
    }
    if req.biometric_hash.is_empty() || req.document_hash.is_empty() || req.pial_shard_id.is_empty() {
        return Err(err(StatusCode::BAD_REQUEST, "biometric_hash, document_hash, pial_shard_id required"));
    }

    // Step 1: Look up existing nexus identity by biometric hash.
    let existing: Option<(Uuid, i16, bool)> = sqlx::query_as(
        "SELECT nexus_id, verity_tier, is_suspended
         FROM nexus_identities WHERE biometric_hash = $1",
    )
    .bind(&req.biometric_hash)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    match existing {
        None => {
            // New identity — create nexus_identities + primary persona link.
            let nexus_id: Uuid = sqlx::query_scalar(
                "INSERT INTO nexus_identities
                     (biometric_hash, document_hash, verity_tier, jurisdiction)
                 VALUES ($1, $2, 1, $3) RETURNING nexus_id",
            )
            .bind(&req.biometric_hash)
            .bind(&req.document_hash)
            .bind(req.jurisdiction.as_deref())
            .fetch_one(&state.pool)
            .await
            .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

            // Primary persona link.
            sqlx::query(
                "INSERT INTO nexus_persona_links
                     (nexus_id, pial_shard_id, persona_type, linked_by, is_primary)
                 VALUES ($1, $2, 'personal', 'biometric_match', true)",
            )
            .bind(nexus_id)
            .bind(&req.pial_shard_id)
            .execute(&state.pool)
            .await
            .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

            // Link most recent attestation for this PIAL shard.
            // We store the pial_shard_id in attestations.metadata as well as nexus_id.
            // Note: attestations.pial_id is a UUID; pial_shard_id is a derived hash.
            // We update rows where metadata->'pial_shard_id' matches (or use a join if available).
            // For simplicity, update attestations that don't yet have a nexus_id and whose
            // pial_id matches (if pial_shard_id can be resolved). Since Verity doesn't store
            // the mapping, we record the nexus_id in nexus_audit_log only.
            let _ = sqlx::query(
                "INSERT INTO nexus_audit_log
                     (nexus_id, action, pial_shard_id, performed_by)
                 VALUES ($1, 'nexus_created', $2, 'system')",
            )
            .bind(nexus_id)
            .bind(&req.pial_shard_id)
            .execute(&state.pool)
            .await;

            Ok(Json(VerifyResp {
                result:     "created".to_string(),
                tier:       1,
                request_id: None,
            }))
        }

        Some((nexus_id, tier, is_suspended)) => {
            if is_suspended {
                return Err(err(StatusCode::FORBIDDEN, "account suspended"));
            }

            // Check if this PIAL shard is already linked.
            let already_linked: bool = sqlx::query_scalar(
                "SELECT EXISTS(
                     SELECT 1 FROM nexus_persona_links
                     WHERE nexus_id = $1 AND pial_shard_id = $2
                 )",
            )
            .bind(nexus_id)
            .bind(&req.pial_shard_id)
            .fetch_one(&state.pool)
            .await
            .unwrap_or(false);

            if already_linked {
                // Update last_verified_at.
                let _ = sqlx::query(
                    "UPDATE nexus_identities SET last_verified_at = NOW() WHERE nexus_id = $1",
                )
                .bind(nexus_id)
                .execute(&state.pool)
                .await;

                return Ok(Json(VerifyResp {
                    result:     "already_linked".to_string(),
                    tier,
                    request_id: None,
                }));
            }

            // PIAL not yet linked — create a link request.
            let request_id: Uuid = sqlx::query_scalar(
                "INSERT INTO nexus_link_requests
                     (nexus_id, target_pial_shard_id, status)
                 VALUES ($1, $2, 'pending') RETURNING request_id",
            )
            .bind(nexus_id)
            .bind(&req.pial_shard_id)
            .fetch_one(&state.pool)
            .await
            .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

            let _ = sqlx::query(
                "INSERT INTO nexus_audit_log
                     (nexus_id, action, pial_shard_id, performed_by)
                 VALUES ($1, 'link_request_created', $2, 'system')",
            )
            .bind(nexus_id)
            .bind(&req.pial_shard_id)
            .execute(&state.pool)
            .await;

            Ok(Json(VerifyResp {
                result:     "match_found".to_string(),
                tier,
                request_id: Some(request_id.to_string()),
            }))
        }
    }
}

// ── POST /v1/nexus/link/confirm ───────────────────────────────────────────────
// User confirms a pending link request — approves linking the new PIAL.
async fn link_confirm(
    State(state): State<Arc<NexusState>>,
    Json(req): Json<LinkActionReq>,
) -> NexusRes<Value> {
    let request_uuid = Uuid::parse_str(&req.request_id)
        .map_err(|_| err(StatusCode::BAD_REQUEST, "invalid request_id"))?;

    // Fetch the pending link request.
    let row: Option<(Uuid, String)> = sqlx::query_as(
        "SELECT nexus_id, target_pial_shard_id FROM nexus_link_requests
         WHERE request_id = $1 AND status = 'pending'
           AND expires_at > NOW()",
    )
    .bind(request_uuid)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let (nexus_id, target_shard) = row
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "request not found or expired"))?;

    // Ownership proof: the caller must BE the target — only the account being
    // invited can confirm the link. This prevents source from self-approving.
    if req.pial_shard_id != target_shard {
        return Err(err(StatusCode::FORBIDDEN, "only the target account can confirm this link"));
    }

    // Create the new persona link.
    sqlx::query(
        "INSERT INTO nexus_persona_links
             (nexus_id, pial_shard_id, persona_type, linked_by, is_primary)
         VALUES ($1, $2, 'personal', 'user_initiated', false)
         ON CONFLICT (pial_shard_id) DO NOTHING",
    )
    .bind(nexus_id)
    .bind(&target_shard)
    .execute(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    // Mark request confirmed.
    sqlx::query(
        "UPDATE nexus_link_requests SET status = 'confirmed', confirmed_at = NOW()
         WHERE request_id = $1",
    )
    .bind(request_uuid)
    .execute(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let _ = sqlx::query(
        "INSERT INTO nexus_audit_log
             (nexus_id, action, pial_shard_id, performed_by)
         VALUES ($1, 'link_confirmed', $2, 'user')",
    )
    .bind(nexus_id)
    .bind(&target_shard)
    .execute(&state.pool)
    .await;

    Ok(Json(json!({ "result": "confirmed" })))
}

// ── POST /v1/nexus/link/decline ───────────────────────────────────────────────
// User declines a pending link request.
async fn link_decline(
    State(state): State<Arc<NexusState>>,
    Json(req): Json<LinkActionReq>,
) -> NexusRes<Value> {
    let request_uuid = Uuid::parse_str(&req.request_id)
        .map_err(|_| err(StatusCode::BAD_REQUEST, "invalid request_id"))?;

    let row: Option<(Uuid, String)> = sqlx::query_as(
        "SELECT nexus_id, target_pial_shard_id FROM nexus_link_requests
         WHERE request_id = $1 AND status = 'pending'
           AND expires_at > NOW()",
    )
    .bind(request_uuid)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let (nexus_id, target_shard) = row
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "request not found or expired"))?;

    sqlx::query(
        "UPDATE nexus_link_requests SET status = 'declined'
         WHERE request_id = $1",
    )
    .bind(request_uuid)
    .execute(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let _ = sqlx::query(
        "INSERT INTO nexus_audit_log
             (nexus_id, action, pial_shard_id, performed_by)
         VALUES ($1, 'link_declined', $2, 'user')",
    )
    .bind(nexus_id)
    .bind(&target_shard)
    .execute(&state.pool)
    .await;

    Ok(Json(json!({ "result": "declined" })))
}

// ── POST /v1/nexus/link/initiate ──────────────────────────────────────────────
// User-initiated linking: caller already owns both accounts (proven by owning
// source_pial_shard_id which is already linked to a nexus).
// ── POST /v1/nexus/link/initiate ─────────────────────────────────────────────
// User-initiated account linking. Creates a pending link request that the
// TARGET account must confirm. Does NOT directly link — that requires confirm.
//
// If the source has no NEXUS yet, a tier-0 user-initiated NEXUS is created so
// it can hold the pending request. KYC later upgrades it to tier 1+.
async fn link_initiate(
    State(state): State<Arc<NexusState>>,
    Json(req): Json<InitiateLinkReq>,
) -> NexusRes<Value> {
    if req.source_pial_shard_id.is_empty() || req.target_pial_shard_id.is_empty() {
        return Err(err(StatusCode::BAD_REQUEST, "source_pial_shard_id and target_pial_shard_id required"));
    }
    if req.source_pial_shard_id == req.target_pial_shard_id {
        return Err(err(StatusCode::BAD_REQUEST, "cannot link an account to itself"));
    }

    // Check target not already linked to a different nexus.
    let target_nexus: Option<Uuid> = sqlx::query_scalar(
        "SELECT nexus_id FROM nexus_persona_links WHERE pial_shard_id = $1",
    )
    .bind(&req.target_pial_shard_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    if target_nexus.is_some() {
        return Err(err(StatusCode::CONFLICT, "target account is already linked to a nexus"));
    }

    // Find or create NEXUS for the source shard.
    let nexus_id: Uuid = match sqlx::query_scalar::<_, Uuid>(
        "SELECT nexus_id FROM nexus_persona_links WHERE pial_shard_id = $1",
    )
    .bind(&req.source_pial_shard_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    {
        Some(id) => id,
        None => {
            // Source has no NEXUS — create a tier-0 user-initiated one.
            let new_id: Uuid = sqlx::query_scalar(
                "INSERT INTO nexus_identities (source, verity_tier) VALUES ('user_initiated', 0)
                 RETURNING nexus_id",
            )
            .fetch_one(&state.pool)
            .await
            .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

            sqlx::query(
                "INSERT INTO nexus_persona_links
                     (nexus_id, pial_shard_id, persona_type, linked_by, is_primary)
                 VALUES ($1, $2, 'personal', 'user_initiated', true)",
            )
            .bind(new_id)
            .bind(&req.source_pial_shard_id)
            .execute(&state.pool)
            .await
            .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

            let _ = sqlx::query(
                "INSERT INTO nexus_audit_log (nexus_id, action, pial_shard_id, performed_by)
                 VALUES ($1, 'nexus_created_user_initiated', $2, 'user')",
            )
            .bind(new_id)
            .bind(&req.source_pial_shard_id)
            .execute(&state.pool)
            .await;

            new_id
        }
    };

    // Check for an existing pending request to this target (avoid duplicates).
    let existing: Option<Uuid> = sqlx::query_scalar(
        "SELECT request_id FROM nexus_link_requests
         WHERE nexus_id = $1 AND target_pial_shard_id = $2
           AND status = 'pending' AND expires_at > NOW()",
    )
    .bind(nexus_id)
    .bind(&req.target_pial_shard_id)
    .fetch_optional(&state.pool)
    .await
    .unwrap_or(None);

    if let Some(rid) = existing {
        return Ok(Json(json!({ "result": "pending", "request_id": rid })));
    }

    // Create pending link request. Expires in 7 days (enough time to log into
    // the other account without urgency).
    let request_id: Uuid = sqlx::query_scalar(
        "INSERT INTO nexus_link_requests
             (nexus_id, target_pial_shard_id, source_pial_shard_id, status, expires_at)
         VALUES ($1, $2, $3, 'pending', NOW() + INTERVAL '7 days')
         RETURNING request_id",
    )
    .bind(nexus_id)
    .bind(&req.target_pial_shard_id)
    .bind(&req.source_pial_shard_id)
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let _ = sqlx::query(
        "INSERT INTO nexus_audit_log (nexus_id, action, pial_shard_id, performed_by)
         VALUES ($1, 'link_request_sent', $2, 'user')",
    )
    .bind(nexus_id)
    .bind(&req.target_pial_shard_id)
    .execute(&state.pool)
    .await;

    Ok(Json(json!({ "result": "pending", "request_id": request_id })))
}

// ── GET /v1/nexus/link/pending ────────────────────────────────────────────────
// Returns all pending link requests where the caller is the TARGET.
// Called on settings page load so the user can see who wants to link with them.
async fn link_pending(
    State(state): State<Arc<NexusState>>,
    axum::extract::Query(params): axum::extract::Query<std::collections::HashMap<String, String>>,
) -> NexusRes<Value> {
    let shard = params.get("target_pial_shard_id")
        .ok_or_else(|| err(StatusCode::BAD_REQUEST, "target_pial_shard_id required"))?;

    let rows: Vec<(Uuid, String, chrono::DateTime<chrono::Utc>, chrono::DateTime<chrono::Utc>)> =
        sqlx::query_as(
            "SELECT request_id, COALESCE(source_pial_shard_id, ''), initiated_at, expires_at
               FROM nexus_link_requests
              WHERE target_pial_shard_id = $1
                AND status = 'pending'
                AND expires_at > NOW()
              ORDER BY initiated_at DESC",
        )
        .bind(shard)
        .fetch_all(&state.pool)
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let requests: Vec<Value> = rows.iter().map(|(rid, src, init, exp)| json!({
        "request_id":          rid,
        "source_pial_shard_id": src,
        "initiated_at":        init,
        "expires_at":          exp,
    })).collect();

    Ok(Json(json!({ "requests": requests, "count": requests.len() })))
}

// ── GET /v1/nexus/personas ────────────────────────────────────────────────────
// List all personas for the authenticated NEXUS.
// Caller identifies via their pial_shard_id (passed as query param).
async fn list_personas(
    State(state): State<Arc<NexusState>>,
    axum::extract::Query(params): axum::extract::Query<std::collections::HashMap<String, String>>,
) -> NexusRes<Value> {
    let shard = params.get("pial_shard_id")
        .ok_or_else(|| err(StatusCode::BAD_REQUEST, "pial_shard_id required"))?;

    let nexus_id: Option<Uuid> = sqlx::query_scalar(
        "SELECT nexus_id FROM nexus_persona_links WHERE pial_shard_id = $1",
    )
    .bind(shard)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let nexus_id = nexus_id
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "shard not linked to any nexus"))?;

    let rows: Vec<(String, String, Option<String>, bool)> = sqlx::query_as(
        "SELECT pial_shard_id, persona_type, display_label, is_primary
         FROM nexus_persona_links WHERE nexus_id = $1
         ORDER BY is_primary DESC, linked_at ASC",
    )
    .bind(nexus_id)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let personas: Vec<Value> = rows
        .iter()
        .map(|(shard_id, ptype, label, primary)| json!({
            "pial_shard_id": shard_id,
            "persona_type":  ptype,
            "display_label": label,
            "is_primary":    primary,
        }))
        .collect();

    // Return tier from nexus_identities (never nexus_id).
    let tier: i16 = sqlx::query_scalar(
        "SELECT verity_tier FROM nexus_identities WHERE nexus_id = $1",
    )
    .bind(nexus_id)
    .fetch_one(&state.pool)
    .await
    .unwrap_or(1);

    Ok(Json(json!({
        "personas": personas,
        "tier":     tier,
        "count":    personas.len(),
    })))
}

// ── DELETE /v1/nexus/personas/:shard ──────────────────────────────────────────
// Unlink a non-primary persona.
async fn unlink_persona(
    State(state): State<Arc<NexusState>>,
    Path(target_shard): Path<String>,
    axum::extract::Query(params): axum::extract::Query<std::collections::HashMap<String, String>>,
) -> NexusRes<Value> {
    let caller_shard = params.get("caller_shard_id")
        .ok_or_else(|| err(StatusCode::BAD_REQUEST, "caller_shard_id required"))?;

    // Verify caller and target belong to the same nexus.
    let caller_nexus: Option<Uuid> = sqlx::query_scalar(
        "SELECT nexus_id FROM nexus_persona_links WHERE pial_shard_id = $1",
    )
    .bind(caller_shard)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let nexus_id = caller_nexus
        .ok_or_else(|| err(StatusCode::FORBIDDEN, "caller not linked to any nexus"))?;

    // Ensure target is not primary.
    let is_primary: bool = sqlx::query_scalar(
        "SELECT COALESCE(
             (SELECT is_primary FROM nexus_persona_links
              WHERE nexus_id = $1 AND pial_shard_id = $2),
             false
         )",
    )
    .bind(nexus_id)
    .bind(&target_shard)
    .fetch_one(&state.pool)
    .await
    .unwrap_or(false);

    if is_primary {
        return Err(err(StatusCode::FORBIDDEN, "cannot unlink the primary persona"));
    }

    let deleted: u64 = sqlx::query(
        "DELETE FROM nexus_persona_links
         WHERE nexus_id = $1 AND pial_shard_id = $2 AND is_primary = false",
    )
    .bind(nexus_id)
    .bind(&target_shard)
    .execute(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?
    .rows_affected();

    if deleted == 0 {
        return Err(err(StatusCode::NOT_FOUND, "persona not found or not unlinkable"));
    }

    let _ = sqlx::query(
        "INSERT INTO nexus_audit_log
             (nexus_id, action, pial_shard_id, performed_by)
         VALUES ($1, 'persona_unlinked', $2, 'user')",
    )
    .bind(nexus_id)
    .bind(&target_shard)
    .execute(&state.pool)
    .await;

    Ok(Json(json!({ "result": "unlinked" })))
}

// ── GET /v1/nexus/tier/:shard ─────────────────────────────────────────────────
// Verified-tier lookup for a PIAL shard (the PIAL record in feed-engine is authoritative).
// Returns tier_source: "nexus" if in NEXUS, "pial" if from attestations only.
async fn get_tier(
    State(state): State<Arc<NexusState>>,
    Path(shard): Path<String>,
) -> NexusRes<Value> {
    // First: check nexus_persona_links JOIN nexus_identities.
    let nexus_row: Option<(i16, bool)> = sqlx::query_as(
        "SELECT ni.verity_tier, ni.is_suspended
         FROM nexus_persona_links npl
         JOIN nexus_identities ni ON ni.nexus_id = npl.nexus_id
         WHERE npl.pial_shard_id = $1",
    )
    .bind(&shard)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    if let Some((tier, is_suspended)) = nexus_row {
        return Ok(Json(json!({
            "pial_shard_id": shard,
            "tier":          tier,
            "tier_source":   "nexus",
            "is_suspended":  is_suspended,
        })));
    }

    // Fallback: query attestations creator_tier (attestations.pial_id is the raw UUID,
    // not the shard. Shard lookup requires a mapping the caller must provide via metadata.
    // Return tier 0 with tier_source "pial" to indicate no NEXUS record found.)
    Ok(Json(json!({
        "pial_shard_id": shard,
        "tier":          0,
        "tier_source":   "pial",
        "is_suspended":  false,
    })))
}

// ── AML types ─────────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct AmlRecordReq {
    pub withdrawal_uaet: i64,
}

// ── GET /v1/nexus/aml/:shard ──────────────────────────────────────────────────
// Returns the current 30-day rolling aggregate for the NEXUS containing this
// PIAL shard. Returns 404 if the shard is not linked to any NEXUS.
// Called by Ain Soph before processing a withdrawal.
async fn aml_get(
    State(state): State<Arc<NexusState>>,
    Path(shard): Path<String>,
) -> NexusRes<Value> {
    let row: Option<(Uuid, i64, bool)> = sqlx::query_as(
        "SELECT na.nexus_id, COALESCE(agg.total_withdrawn_uaet, 0), COALESCE(agg.flag_for_edd, false)
         FROM nexus_persona_links na
         LEFT JOIN nexus_aml_aggregates agg
           ON agg.nexus_id = na.nexus_id
           AND agg.period_start <= NOW()
           AND agg.period_end   >= NOW()
         WHERE na.pial_shard_id = $1",
    )
    .bind(&shard)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let (nexus_id, total, flagged) = row
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "shard not linked to any nexus"))?;

    Ok(Json(json!({
        "nexus_id_hash":         sha256_hex(&nexus_id.to_string()),
        "total_withdrawn_uaet":  total,
        "flag_for_edd":          flagged,
        "window_days":           30,
    })))
}

// ── POST /v1/nexus/aml/:shard ─────────────────────────────────────────────────
// Records a completed withdrawal and returns updated aggregate + EDD flag.
// Called by Ain Soph after a withdrawal is approved.
// $10,000 USD threshold = 1_000_000 in the units used here.
async fn aml_record(
    State(state): State<Arc<NexusState>>,
    Path(shard): Path<String>,
    Json(req): Json<AmlRecordReq>,
) -> NexusRes<Value> {
    const AML_THRESHOLD: i64 = 1_000_000;

    let nexus_id: Option<Uuid> = sqlx::query_scalar(
        "SELECT nexus_id FROM nexus_persona_links WHERE pial_shard_id = $1",
    )
    .bind(&shard)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let nexus_id = nexus_id
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "shard not linked to any nexus"))?;

    // Upsert the rolling 30-day window aggregate.
    sqlx::query(
        "INSERT INTO nexus_aml_aggregates (nexus_id, period_start, period_end, total_withdrawn_uaet)
         VALUES ($1, date_trunc('day', NOW()), date_trunc('day', NOW()) + INTERVAL '30 days', $2)
         ON CONFLICT (nexus_id, period_start) DO UPDATE SET
           total_withdrawn_uaet = nexus_aml_aggregates.total_withdrawn_uaet + EXCLUDED.total_withdrawn_uaet",
    )
    .bind(nexus_id)
    .bind(req.withdrawal_uaet)
    .execute(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    // Re-fetch updated total.
    let new_total: i64 = sqlx::query_scalar(
        "SELECT COALESCE(total_withdrawn_uaet, 0) FROM nexus_aml_aggregates
         WHERE nexus_id = $1 AND period_start <= NOW() AND period_end >= NOW()
         ORDER BY period_start DESC LIMIT 1",
    )
    .bind(nexus_id)
    .fetch_one(&state.pool)
    .await
    .unwrap_or(req.withdrawal_uaet);

    let flag = new_total >= AML_THRESHOLD;
    if flag {
        sqlx::query(
            "UPDATE nexus_aml_aggregates SET flag_for_edd = true, flagged_at = NOW()
             WHERE nexus_id = $1 AND period_start <= NOW() AND period_end >= NOW()",
        )
        .bind(nexus_id)
        .execute(&state.pool)
        .await
        .ok();

        let _ = sqlx::query(
            "INSERT INTO nexus_audit_log (nexus_id, action, performed_by)
             VALUES ($1, 'aml_edd_flag_set', 'system')",
        )
        .bind(nexus_id)
        .execute(&state.pool)
        .await;
    }

    Ok(Json(json!({
        "nexus_id_hash":        sha256_hex(&nexus_id.to_string()),
        "total_withdrawn_uaet": new_total,
        "flag_for_edd":         flag,
        "window_days":          30,
    })))
}

fn sha256_hex(input: &str) -> String {
    use sha2::{Digest, Sha256};
    hex::encode(Sha256::digest(input.as_bytes()))
}
