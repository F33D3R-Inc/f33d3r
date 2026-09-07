mod auth;
mod db;
mod manhattan_outbox;
mod models;
mod nexus;
mod nexus_sweep;
mod observ;

use axum::{
    extract::{FromRef, Path, State},
    http::StatusCode,
    response::Json,
    routing::{get, post},
    Router,
};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use sqlx::PgPool;
use std::sync::Arc;
use tracing::{info, warn};
use uuid::Uuid;

use auth::{Caller, InternalApiKey};
use manhattan_client::Manhattan;
use models::*;

#[derive(Clone)]
struct AppState {
    pool: PgPool,
    /// The naming plane, reached by name. Verity owns no identity — every PIAL
    /// its tables carry is a reference resolved through here rather than a
    /// foreign key into a brain that owns it.
    manhattan: Manhattan,
    /// The service-to-service secret. Every route that changes what a person is
    /// permitted to do requires it — see `auth`.
    internal_api_key: InternalApiKey,
}

// The `Caller` extractor reaches the configured key through `FromRef`. The
// router state is `Arc<AppState>`, so the impl is on the Arc.
impl FromRef<std::sync::Arc<AppState>> for InternalApiKey {
    fn from_ref(state: &std::sync::Arc<AppState>) -> InternalApiKey {
        state.internal_api_key.clone()
    }
}

type Res<T> = Result<Json<T>, (StatusCode, Json<Value>)>;

fn err(status: StatusCode, msg: &str) -> (StatusCode, Json<Value>) {
    (status, Json(json!({ "error": msg })))
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    observ::init("verity")?;

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");
    let port = std::env::var("PORT").unwrap_or_else(|_| "8095".into());

    // Verity decides who may earn money. A brain that cannot tell a peer from a
    // stranger must not start and then quietly accept both — an empty key would
    // make every caller a trusted service and reopen the tier-forgery hole this
    // guard exists to close. Same posture ain-soph takes on the ledger.
    let internal_api_key = InternalApiKey::from_env();
    if internal_api_key.is_empty() {
        anyhow::bail!(
            "INTERNAL_API_KEY must be set for Verity: it authenticates the \
             service-to-service lane on every route that sets a tier, records a \
             KYC result, or writes a compliance decision"
        );
    }

    let pool = sqlx::PgPool::connect(&database_url).await?;
    db::migrate(&pool).await?;
    info!("Verity schema up to date");

    let manhattan = Manhattan::from_env("verity")?;
    manhattan_outbox::spawn(database_url.clone(), manhattan.clone());
    nexus_sweep::spawn(pool.clone(), nexus_sweep::interval_from_env());

    let state = Arc::new(AppState {
        pool: pool.clone(),
        manhattan,
        internal_api_key: internal_api_key.clone(),
    });
    let nexus_state = Arc::new(nexus::NexusState {
        pool,
        internal_api_key,
    });

    let app = Router::new()
        .route("/metrics", get(observ::metrics_handler))
        .route("/health", get(health))
        // Decision engine — called by Elohim Veni / Nantar
        .route("/v1/decide", post(decide))
        // KYC submission pipeline
        .route("/v1/kyc/submit", post(kyc_submit))
        .route("/v1/kyc/:pial_id", get(kyc_status))
        // Profile summary
        .route("/v1/profile/:pial_id", get(profile))
        // Risk signal ingestion — called by Zodacare / Elohim Veni
        .route("/v1/risk/signal", post(risk_signal))
        // Admin overrides (compliance / moderation team)
        .route("/v1/admin/tier", post(admin_set_tier))
        // ── Compliance (18 USC 2257 + CSAM) ──────────────────────────────
        // 2257 record management — called when creator enables adult content
        .route("/v1/compliance/2257/record", post(compliance_2257_create))
        .route("/v1/compliance/2257/:pial_id", get(compliance_2257_get))
        // 2257 statement text for display on adult content
        .route(
            "/v1/compliance/2257/statement/:pial_id",
            get(compliance_2257_statement),
        )
        // CSAM scan — called by Caeor after every media upload
        .route("/v1/compliance/csam/scan", post(compliance_csam_scan))
        .route("/v1/compliance/csam/:hash", get(compliance_csam_status))
        .with_state(state)
        .merge(nexus::nexus_router(nexus_state))
        .layer(axum::middleware::from_fn(observ::http_middleware));

    // Binds 0.0.0.0 inside the container so peer brains on the internal docker
    // network reach it as `verity:8095`. LAN exposure is removed at the host
    // boundary instead: prod publishes no port for this brain, and local now
    // publishes it on `127.0.0.1:8095` rather than `0.0.0.0` — see the compose
    // files. Binding loopback here would break service-to-service.
    let addr = format!("0.0.0.0:{port}");
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    info!("Verity listening on {addr}");
    axum::serve(listener, app).await?;
    Ok(())
}

// ── Health ────────────────────────────────────────────────────────────────────
//
// The naming plane is reported, never depended on: if Manhattan is unreachable
// the registrations queue in the outbox, the API answers normally, and names
// simply stop resolving elsewhere. The backlog is reported alongside it because
// depth alone says a queue is behind — the age of the head and the error it
// last failed with say whether it is moving at all.
//
// A failed backlog query is reported, not swallowed, but it does not fail the
// check: this endpoint is the container healthcheck other brains gate their
// start-up on, and turning it into a database liveness probe is a change to
// make deliberately, not as a side effect of adding a counter to it.

async fn health(State(state): State<Arc<AppState>>) -> Json<Value> {
    let configured = state.manhattan.configured();

    match db::outbox_backlog(&state.pool).await {
        Ok(backlog) => Json(json!({
            "status":                              "ok",
            "service":                             "verity",
            "manhattan":                           configured,
            "manhattan_outbox_pending":            backlog.pending,
            "manhattan_outbox_oldest_age_seconds": backlog.oldest_age_seconds,
            "manhattan_outbox_head_error":         backlog.head_last_error,
            // Rows the drain gave up on, and persona links with no name to
            // express them yet. Both are naming-plane writes that have not
            // happened, and neither is allowed to be invisible.
            "manhattan_outbox_quarantined":        backlog.quarantined,
            "deferred_persona_links":
                db::deferred_persona_links(&state.pool).await.ok(),
        })),
        Err(e) => {
            warn!(error = %e, "manhattan outbox backlog query failed");
            Json(json!({
                "status":                   "degraded",
                "service":                  "verity",
                "manhattan":                configured,
                "manhattan_outbox_pending": Value::Null,
                "manhattan_outbox_error":   e.to_string(),
            }))
        }
    }
}

// ── POST /v1/decide ───────────────────────────────────────────────────────────
// Core decision endpoint. Computes current tier, age access, and risk.

async fn decide(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    Json(req): Json<DecisionReq>,
) -> Res<DecisionResp> {
    // A decision writes an attestation that downstream gates read as authority.
    // Only a brain may ask for one; an account cannot decide its own eligibility.
    caller.require_service()?;

    let risk = db::get_risk_profile(&state.pool, req.pial_id).await;

    // Pull best existing attestation across all contexts for this PIAL
    let best_tier: i32 = sqlx::query_scalar(
        "SELECT COALESCE(MAX(creator_tier), 0) FROM attestations
         WHERE pial_id = $1 AND status = 'approved'
         AND (expires_at IS NULL OR expires_at > NOW())",
    )
    .bind(req.pial_id)
    .fetch_one(&state.pool)
    .await
    .unwrap_or(0);

    let best_age: String = sqlx::query_scalar(
        "SELECT COALESCE(
            (SELECT age_band FROM attestations
             WHERE pial_id = $1 AND status = 'approved' AND age_band != 'unknown'
             AND (expires_at IS NULL OR expires_at > NOW())
             ORDER BY created_at DESC LIMIT 1),
            'unknown'
         )",
    )
    .bind(req.pial_id)
    .fetch_one(&state.pool)
    .await
    .unwrap_or_else(|_| "unknown".to_string());

    // Compute composite risk score
    let composite_risk = compute_risk(&risk);

    // Determine what's allowed based on tier + risk
    let (status, payout_enabled, nsfw_access, required_actions) =
        evaluate(&req.context, best_tier, &best_age, composite_risk, &risk);

    let ts = chrono::Utc::now();
    let decision_hash = make_hash(req.pial_id, &status, &best_age, best_tier, ts);

    // Persist attestation
    sqlx::query(
        "INSERT INTO attestations
            (pial_id, context, status, age_band, creator_tier,
             payout_enabled, nsfw_access, risk_score, required_actions, decision_hash)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)",
    )
    .bind(req.pial_id)
    .bind(&req.context)
    .bind(&status)
    .bind(&best_age)
    .bind(best_tier)
    .bind(payout_enabled)
    .bind(nsfw_access)
    .bind(composite_risk)
    .bind(&required_actions)
    .bind(&decision_hash)
    .execute(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    db::audit(
        &state.pool,
        req.pial_id,
        "decision",
        Some(&req.context),
        None,
        Some(best_tier),
        json!({
            "status": status, "risk": composite_risk
        }),
    )
    .await;

    Ok(Json(DecisionResp {
        pial_id: req.pial_id,
        status,
        age_band: best_age,
        creator_tier: best_tier,
        payout_enabled,
        nsfw_access,
        risk_score: composite_risk,
        required_actions,
        decision_hash,
        evaluated_at: ts,
    }))
}

// ── POST /v1/kyc/submit ───────────────────────────────────────────────────────

async fn kyc_submit(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    Json(req): Json<KycSubmitReq>,
) -> Res<KycSubmitResp> {
    // A KYC submission can promote the subject to tier 2 (age-verified, NSFW
    // access). It is forwarded by feed-engine only after a real eKYC pass, so it
    // is service-to-service; an account submitting its own "passed gov_id" is the
    // forgery this gate refuses.
    caller.require_service()?;

    let valid_types = ["gov_id", "liveness", "phone", "email", "manual"];
    if !valid_types.contains(&req.submission_type.as_str()) {
        return Err(err(StatusCode::BAD_REQUEST, "invalid submission_type"));
    }

    let age_band = req.age_band.as_deref().unwrap_or("unknown");
    let confidence = req.confidence.unwrap_or(0.0);
    let provider = req.provider.as_deref().unwrap_or("internal");

    // Fail immediately if confidence is too low for gov_id
    let status = if req.submission_type == "gov_id" && confidence < 0.6 {
        "failed"
    } else {
        "passed"
    };

    let id: Uuid = sqlx::query_scalar(
        "INSERT INTO kyc_submissions
            (pial_id, submission_type, status, document_hash, age_band,
             confidence, provider, processed_at, metadata)
         VALUES ($1,$2,$3,$4,$5,$6,$7,NOW(),$8)
         RETURNING id",
    )
    .bind(req.pial_id)
    .bind(&req.submission_type)
    .bind(status)
    .bind(req.document_hash.as_deref())
    .bind(age_band)
    .bind(confidence)
    .bind(provider)
    .bind(req.metadata.unwrap_or(json!({})))
    .fetch_one(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    // If gov_id passed with a usable age band, promote tier to 2
    if status == "passed" && req.submission_type == "gov_id" && age_band != "unknown" {
        let _ = sqlx::query(
            "INSERT INTO attestations
                (pial_id, context, status, age_band, creator_tier, payout_enabled, nsfw_access,
                 risk_score, required_actions, decision_hash)
             VALUES ($1, 'age_gate', 'approved', $2, 2, false, true, 0.0, '{}', $3)",
        )
        .bind(req.pial_id)
        .bind(age_band)
        .bind(make_hash(
            req.pial_id,
            "approved",
            age_band,
            2,
            chrono::Utc::now(),
        ))
        .execute(&state.pool)
        .await;

        db::audit(
            &state.pool,
            req.pial_id,
            "tier_change",
            Some("kyc_gov_id"),
            Some(0),
            Some(2),
            json!({
                "age_band": age_band, "confidence": confidence
            }),
        )
        .await;
    }

    // Track failed attempts in risk profile
    if status == "failed" {
        let mut risk = db::get_risk_profile(&state.pool, req.pial_id).await;
        risk.failed_kyc_attempts += 1;
        risk.risk_score = (risk.risk_score + 0.1).min(1.0);
        db::upsert_risk_profile(&state.pool, &risk).await;
    }

    Ok(Json(KycSubmitResp {
        submission_id: id,
        status: status.to_string(),
        message: if status == "passed" {
            "Verification accepted".into()
        } else {
            "Verification failed — check document quality and retry".into()
        },
    }))
}

// ── GET /v1/kyc/:pial_id ─────────────────────────────────────────────────────

async fn kyc_status(State(state): State<Arc<AppState>>, Path(pial_id): Path<Uuid>) -> Res<Value> {
    let rows: Vec<(
        String,
        String,
        Option<String>,
        Option<f32>,
        chrono::DateTime<chrono::Utc>,
    )> = sqlx::query_as(
        "SELECT submission_type, status, age_band, confidence, created_at
             FROM kyc_submissions WHERE pial_id = $1
             ORDER BY created_at DESC LIMIT 20",
    )
    .bind(pial_id)
    .fetch_all(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let submissions: Vec<Value> = rows
        .iter()
        .map(|(t, s, ab, c, ts)| {
            json!({
                "type": t, "status": s,
                "age_band": ab, "confidence": c,
                "submitted_at": ts
            })
        })
        .collect();

    Ok(Json(
        json!({ "pial_id": pial_id, "submissions": submissions }),
    ))
}

// ── GET /v1/profile/:pial_id ──────────────────────────────────────────────────

async fn profile(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<Uuid>,
) -> Res<ProfileResp> {
    let risk = db::get_risk_profile(&state.pool, pial_id).await;

    let best_tier: i32 = sqlx::query_scalar(
        "SELECT COALESCE(MAX(creator_tier), 0) FROM attestations
         WHERE pial_id = $1 AND status = 'approved'
         AND (expires_at IS NULL OR expires_at > NOW())",
    )
    .bind(pial_id)
    .fetch_one(&state.pool)
    .await
    .unwrap_or(0);

    let best_age: String = sqlx::query_scalar(
        "SELECT COALESCE(
            (SELECT age_band FROM attestations
             WHERE pial_id = $1 AND status = 'approved' AND age_band != 'unknown'
             ORDER BY created_at DESC LIMIT 1),
            'unknown'
         )",
    )
    .bind(pial_id)
    .fetch_one(&state.pool)
    .await
    .unwrap_or_else(|_| "unknown".to_string());

    let kyc_count: i64 =
        sqlx::query_scalar("SELECT COUNT(*) FROM kyc_submissions WHERE pial_id = $1")
            .bind(pial_id)
            .fetch_one(&state.pool)
            .await
            .unwrap_or(0);

    let last_decision: Option<chrono::DateTime<chrono::Utc>> = sqlx::query_scalar(
        "SELECT created_at FROM attestations WHERE pial_id = $1 ORDER BY created_at DESC LIMIT 1",
    )
    .bind(pial_id)
    .fetch_optional(&state.pool)
    .await
    .unwrap_or(None);

    let composite_risk = compute_risk(&risk);
    let payout_enabled = best_tier >= TIER_CREATOR && composite_risk < RISK_REVIEW;
    let nsfw_access = best_age == "18+" || best_age == "21+";

    Ok(Json(ProfileResp {
        pial_id,
        creator_tier: best_tier,
        age_band: best_age,
        payout_enabled,
        nsfw_access,
        risk_score: composite_risk,
        kyc_submissions: kyc_count,
        last_decision,
    }))
}

// ── POST /v1/risk/signal ──────────────────────────────────────────────────────

async fn risk_signal(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    Json(req): Json<RiskSignalReq>,
) -> Res<Value> {
    // Risk feeds the deny thresholds in the decision engine. An account clearing
    // its own risk score is the abuse; only a brain (zodacare, elohim-veni,
    // feed-engine) reports signals.
    caller.require_service()?;

    let mut risk = db::get_risk_profile(&state.pool, req.pial_id).await;

    if let Some(dc) = req.device_count {
        risk.device_count = dc;
    }
    if let Some(d) = req.failed_kyc_delta {
        risk.failed_kyc_attempts += d;
    }
    if let Some(ip) = req.ip_reputation {
        risk.ip_reputation_score = ip;
    }
    if let Some(v) = req.velocity_score {
        risk.velocity_score = v;
    }
    if let Some(dup) = req.duplicate_detected {
        risk.duplicate_detected = dup;
    }

    risk.risk_score = compute_risk(&risk);
    db::upsert_risk_profile(&state.pool, &risk).await;

    db::audit(
        &state.pool,
        req.pial_id,
        "risk_update",
        None,
        None,
        None,
        json!({
            "new_score": risk.risk_score
        }),
    )
    .await;

    Ok(Json(
        json!({ "pial_id": req.pial_id, "risk_score": risk.risk_score }),
    ))
}

// ── POST /v1/admin/tier ───────────────────────────────────────────────────────

async fn admin_set_tier(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    Json(req): Json<AdminTierReq>,
) -> Res<Value> {
    // The admin override sets any account's tier directly, payout and all. It is
    // the single most dangerous route in this brain: it must never be reachable
    // without the service credential. feed-engine performs the operator
    // authorisation on its side and forwards here holding the internal key.
    caller.require_service()?;

    if req.tier < 0 || req.tier > 3 {
        return Err(err(StatusCode::BAD_REQUEST, "tier must be 0–3"));
    }

    let ts = chrono::Utc::now();
    let hash = make_hash(req.pial_id, "approved", "unknown", req.tier, ts);

    sqlx::query(
        "INSERT INTO attestations
            (pial_id, context, status, age_band, creator_tier, payout_enabled, nsfw_access,
             risk_score, required_actions, decision_hash, metadata)
         VALUES ($1,'admin_override','approved','unknown',$2,$3,$4,0.0,'{}', $5, $6)",
    )
    .bind(req.pial_id)
    .bind(req.tier)
    .bind(req.tier >= TIER_CREATOR)
    .bind(req.tier >= TIER_AGE)
    .bind(hash)
    .bind(json!({ "admin": req.admin_id, "reason": req.reason }))
    .execute(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    db::audit(
        &state.pool,
        req.pial_id,
        "tier_change",
        Some("admin_override"),
        None,
        Some(req.tier),
        json!({
            "admin": req.admin_id, "reason": req.reason
        }),
    )
    .await;

    Ok(Json(
        json!({ "pial_id": req.pial_id, "tier": req.tier, "status": "set" }),
    ))
}

// ── Decision engine ───────────────────────────────────────────────────────────

fn evaluate(
    context: &str,
    tier: i32,
    age_band: &str,
    risk: f32,
    profile: &models::RiskProfile,
) -> (String, bool, bool, Vec<String>) {
    let mut required: Vec<String> = Vec::new();
    let age_ok = age_band == "18+" || age_band == "21+";

    if profile.duplicate_detected {
        return ("denied".into(), false, false, vec!["manual_review".into()]);
    }
    if risk >= RISK_REVIEW {
        return ("denied".into(), false, false, vec!["manual_review".into()]);
    }

    match context {
        "onboarding" => {
            if tier < TIER_BASIC {
                required.push("verify_email".into());
            }
            let status = if risk < RISK_SAFE {
                "approved"
            } else {
                "pending_review"
            };
            (status.into(), false, false, required)
        }
        "age_gate" => {
            if !age_ok {
                required.push("upload_id".into());
                required.push("liveness_check".into());
                return ("pending_review".into(), false, false, required);
            }
            ("approved".into(), false, true, required)
        }
        "creator_signup" => {
            if !age_ok {
                required.push("upload_id".into());
            }
            if tier < TIER_AGE {
                required.push("liveness_check".into());
            }
            if required.is_empty() && tier >= TIER_CREATOR {
                ("approved".into(), false, true, required)
            } else {
                ("pending_review".into(), false, age_ok, required)
            }
        }
        "payout_activation" => {
            if tier < TIER_CREATOR {
                required.push("complete_creator_verification".into());
            }
            if !age_ok {
                required.push("upload_id".into());
            }
            if risk >= RISK_SAFE {
                required.push("manual_review".into());
            }
            if required.is_empty() {
                ("approved".into(), true, true, required)
            } else {
                ("denied".into(), false, age_ok, required)
            }
        }
        _ => {
            // Generic context — allow if basic tier met and risk is acceptable
            let status = if tier >= TIER_BASIC && risk < RISK_SAFE {
                "approved"
            } else {
                "pending_review"
            };
            (status.into(), false, age_ok, required)
        }
    }
}

fn compute_risk(p: &models::RiskProfile) -> f32 {
    let mut score: f32 = 0.0;
    score += (1.0 - p.ip_reputation_score) * 0.30;
    score += p.velocity_score * 0.25;
    score += (p.failed_kyc_attempts as f32 * 0.05).min(0.25);
    if p.duplicate_detected {
        score += 0.40;
    }
    if p.device_count > 5 {
        score += ((p.device_count - 5) as f32 * 0.02).min(0.10);
    }
    score.min(1.0)
}

fn make_hash(
    pial_id: Uuid,
    status: &str,
    age_band: &str,
    tier: i32,
    ts: chrono::DateTime<chrono::Utc>,
) -> String {
    let mut h = Sha256::new();
    h.update(pial_id.to_string().as_bytes());
    h.update(status.as_bytes());
    h.update(age_band.as_bytes());
    h.update(tier.to_string().as_bytes());
    h.update(ts.timestamp_millis().to_string().as_bytes());
    hex::encode(h.finalize())
}

// ── POST /v1/compliance/2257/record ──────────────────────────────────────────
// Creates or retrieves a 2257 record for a creator who has completed gov_id KYC.
// Must be called when a creator enables adult content.
async fn compliance_2257_create(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    Json(req): Json<models::Create2257Req>,
) -> Res<Value> {
    // Writes the 18 USC 2257 record that lets a creator publish adult content.
    // feed-engine forwards it after the creator completes gov_id KYC; it is not
    // something an account asserts about itself.
    caller.require_service()?;

    // Check for existing active record — idempotent
    if let Some(existing) = db::get_2257_record(&state.pool, req.pial_id).await {
        return Ok(Json(json!({
            "record_id":         existing.id,
            "creator_pial_id":   existing.creator_pial_id,
            "age_band":          existing.age_band,
            "verification_date": existing.verification_date,
            "statement_hash":    existing.statement_hash,
            "record_location":   existing.record_location,
            "already_existed":   true,
        })));
    }

    // Require a passed gov_id KYC submission with confirmed age band
    let kyc: Option<(Uuid, String, Option<String>)> = sqlx::query_as(
        "SELECT id, age_band, document_hash FROM kyc_submissions
         WHERE pial_id = $1 AND submission_type = 'gov_id'
           AND status = 'passed' AND age_band IN ('18+','21+')
         ORDER BY created_at DESC LIMIT 1",
    )
    .bind(req.pial_id)
    .fetch_optional(&state.pool)
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    let (kyc_id, age_band, doc_hash) = kyc.ok_or_else(|| {
        err(StatusCode::PRECONDITION_FAILED,
            "Creator must complete gov_id KYC with confirmed 18+ age band before enabling adult content")
    })?;

    let document_hash = doc_hash.unwrap_or_else(|| format!("manual_{}", req.pial_id));

    // Build the cryptographic statement hash — anchor for the record-keeper statement
    let ts = chrono::Utc::now();
    let mut h = Sha256::new();
    h.update(req.pial_id.to_string().as_bytes());
    h.update(age_band.as_bytes());
    h.update(ts.timestamp_millis().to_string().as_bytes());
    h.update(document_hash.as_bytes());
    let statement_hash = hex::encode(h.finalize());

    let record = models::Record2257 {
        id: Uuid::new_v4(),
        creator_pial_id: req.pial_id,
        kyc_submission_id: Some(kyc_id),
        age_band: age_band.clone(),
        document_type: "gov_id".into(),
        document_hash,
        verification_date: ts,
        record_keeper: "F33D3R Platform".into(),
        record_location: "https://f33d3r.com/legal/2257".into(),
        statement_hash: statement_hash.clone(),
        is_active: true,
        revoked_at: None,
        revocation_reason: None,
        created_at: ts,
    };

    let id = db::create_2257_record(&state.pool, &record)
        .await
        .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    db::audit(
        &state.pool,
        req.pial_id,
        "2257_record_created",
        Some("adult_content_enable"),
        None,
        None,
        json!({ "age_band": age_band }),
    )
    .await;

    info!("2257 record created for {}", req.pial_id);

    Ok(Json(json!({
        "record_id":         id,
        "creator_pial_id":   req.pial_id,
        "age_band":          age_band,
        "verification_date": ts,
        "statement_hash":    statement_hash,
        "record_location":   "https://f33d3r.com/legal/2257",
        "already_existed":   false,
    })))
}

// ── GET /v1/compliance/2257/:pial_id ─────────────────────────────────────────
async fn compliance_2257_get(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<Uuid>,
) -> Res<Value> {
    match db::get_2257_record(&state.pool, pial_id).await {
        Some(r) => Ok(Json(json!({
            "has_record":        true,
            "record_id":         r.id,
            "creator_pial_id":   r.creator_pial_id,
            "age_band":          r.age_band,
            "verification_date": r.verification_date,
            "statement_hash":    r.statement_hash,
            "record_location":   r.record_location,
            "is_active":         r.is_active,
        }))),
        None => Ok(Json(json!({ "has_record": false, "pial_id": pial_id }))),
    }
}

// ── GET /v1/compliance/2257/statement/:pial_id ────────────────────────────────
// Returns the 18 USC 2257 record-keeper statement for display on adult content.
async fn compliance_2257_statement(
    State(state): State<Arc<AppState>>,
    Path(pial_id): Path<Uuid>,
) -> Res<Value> {
    let rec = db::get_2257_record(&state.pool, pial_id)
        .await
        .ok_or_else(|| err(StatusCode::NOT_FOUND, "No 2257 record for this creator"))?;

    let statement = format!(
        "18 U.S.C. 2257 Record-Keeping Requirements Compliance Statement\n\n\
         All models, actors, actresses and other persons that appear in any \
         visual depiction of actual or simulated sexually explicit conduct \
         appearing or otherwise contained in this content were eighteen (18) \
         years of age or older at the time of the creation of such depictions.\n\n\
         Records required pursuant to 18 U.S.C. 2257 for any material contained \
         herein are kept by the Custodian of Records:\n\n\
         F33D3R Platform\n\
         Custodian: F33D3R Platform Compliance Team\n\
         {}\n\n\
         Statement hash: {}",
        rec.record_location, rec.statement_hash
    );

    Ok(Json(json!({
        "pial_id":         pial_id,
        "statement":       statement,
        "statement_hash":  rec.statement_hash,
        "record_location": rec.record_location,
    })))
}

// ── POST /v1/compliance/csam/scan ─────────────────────────────────────────────
// CSAM hash check. Called by Caeor after every media upload.
//
// In production: integrate with NCMEC CyberTipline API or Microsoft PhotoDNA.
// Local dev stub: always returns clean, logs the scan for audit trail.
async fn compliance_csam_scan(
    State(state): State<Arc<AppState>>,
    caller: Caller,
    Json(req): Json<models::CsamScanReq>,
) -> Res<Value> {
    // The CSAM verdict decides whether an upload is allowed to exist. Caeor calls
    // it after every media upload; a caller must not be able to self-certify a
    // hash as clean, so it is service-to-service.
    caller.require_service()?;

    // Check cache — avoid re-scanning known hashes
    if let Some(existing) = db::get_csam_scan(&state.pool, &req.content_hash).await {
        if existing.result == "flagged" {
            return Err(err(
                StatusCode::UNAVAILABLE_FOR_LEGAL_REASONS,
                "Content hash matches known CSAM — upload rejected",
            ));
        }
        return Ok(Json(json!({
            "scan_id":      existing.id,
            "result":       existing.result,
            "cached":       true,
        })));
    }

    // ── Stub implementation ───────────────────────────────────────────────────
    // Production: replace this block with NCMEC PhotoDNA API call.
    // The interface contract: POST hash → get "clean" | "flagged"
    // If flagged: send CyberTip to NCMEC, preserve content for law enforcement.
    // NCMEC CyberTipline API: https://www.missingkids.org/gethelpnow/cybertipline
    let result = "clean";
    let match_count = 0i32;
    // ── End stub ─────────────────────────────────────────────────────────────

    let scan_id = db::record_csam_scan(
        &state.pool,
        &req.content_hash,
        req.uploader_pial,
        result,
        match_count,
        req.media_url.as_deref(),
        req.media_type.as_deref(),
    )
    .await
    .map_err(|e| err(StatusCode::INTERNAL_SERVER_ERROR, &e.to_string()))?;

    Ok(Json(json!({
        "scan_id": scan_id,
        "result":  result,
        "cached":  false,
    })))
}

// ── GET /v1/compliance/csam/:hash ─────────────────────────────────────────────
async fn compliance_csam_status(
    State(state): State<Arc<AppState>>,
    Path(content_hash): Path<String>,
) -> Res<Value> {
    match db::get_csam_scan(&state.pool, &content_hash).await {
        Some(s) => Ok(Json(json!({
            "scan_id":    s.id,
            "result":     s.result,
            "scanned_at": s.scanned_at,
        }))),
        None => Ok(Json(json!({ "result": "not_scanned" }))),
    }
}
