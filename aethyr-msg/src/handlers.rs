/// AMP v1 HTTP API handlers.
///
/// All endpoints use JSON. Binary framing (bincode) is available for native clients
/// via the Accept: application/octet-stream header (future: AMP v2).
///
/// Message lifecycle:
///   1. Sender calls POST /v1/identity to register (idempotent).
///   2. Sender fetches Bob's prekey bundle: GET /v1/prekeys/:identity.
///   3. Sender performs X3DH locally, initialises Double Ratchet.
///   4. Sender encrypts payload into chunks.
///   5. Sender calls POST /v1/messages.
///   6. Relay validates PoW (if first contact), rate limits, stores frame.
///   7. Recipient polls GET /v1/messages/:identity  — OR  receives push via WS.
///   8. Recipient decrypts chunks with ratchet, verifies content hashes.
///   9. Recipient calls DELETE /v1/messages/:id to ack delivery.

use std::sync::Arc;
use axum::{
    extract::{Path, Query, State, WebSocketUpgrade},
    extract::ws::{Message, WebSocket},
    http::StatusCode,
    response::Response,
    Json,
};
use chrono::{DateTime, Utc, Duration};
use serde::Deserialize;
use sqlx::PgPool;
use tracing::{info, warn};
use uuid::Uuid;

use crate::brain::BrainClient;
use crate::models::*;
use crate::relay::{RateLimiter, PushRegistry, verify_first_contact_pow};

/// Shared application state injected into all handlers.
#[derive(Clone)]
pub struct AppState {
    pub pool:         PgPool,
    pub rate_limiter: Arc<RateLimiter>,
    pub push:         Arc<PushRegistry>,
    pub brain:        Arc<BrainClient>,
}

// ── GET /health ───────────────────────────────────────────────────────────────

pub async fn health() -> Json<StatusResp> { Json(StatusResp { status: "ok" }) }

// ── GET /v1/stats ─────────────────────────────────────────────────────────────

pub async fn stats(State(s): State<AppState>) -> Json<serde_json::Value> {
    let pending: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM messages WHERE status = 'SENT' AND expires_at > NOW()"
    )
    .fetch_one(&s.pool).await.unwrap_or(0);

    let identities: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM identities")
        .fetch_one(&s.pool).await.unwrap_or(0);

    Json(serde_json::json!({
        "status":     "online",
        "protocol":   "AMP_v1",
        "pending_messages": pending,
        "registered_identities": identities,
    }))
}

// ── POST /v1/identity ─────────────────────────────────────────────────────────

pub async fn register_identity(
    State(s): State<AppState>,
    Json(req): Json<RegisterIdentityReq>,
) -> Result<Json<IdentityResp>, StatusCode> {
    if req.identity.len() < 3 || req.public_key_b64.is_empty() {
        return Err(StatusCode::BAD_REQUEST);
    }

    // ON CONFLICT DO NOTHING: first-registered key wins.
    // Multi-device support is via the /v1/devices endpoint.
    // Never overwrite a key — that breaks messages encrypted for the original device.
    sqlx::query(
        r#"INSERT INTO identities (identity, public_key_b64, handle, device_id)
           VALUES ($1, $2, $3, $4)
           ON CONFLICT (identity) DO UPDATE SET
               handle       = COALESCE(EXCLUDED.handle, identities.handle),
               last_seen_at = NOW()"#
    )
    .bind(&req.identity)
    .bind(&req.public_key_b64)
    .bind(&req.handle)
    .bind(&req.device_id)
    .execute(&s.pool)
    .await
    .map_err(|e| { warn!("register identity: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    info!("identity registered: {}…", &req.identity[..8.min(req.identity.len())]);
    Ok(Json(IdentityResp {
        identity:       req.identity.clone(),
        public_key_b64: req.public_key_b64.clone(),
        handle:         req.handle.clone(),
        created_at:     Utc::now(),
    }))
}

// ── POST /v1/identity/:id/glyph — register Glyph signing key ─────────────────
// Associates an ECDSA-P256 public key with this PIAL identity.
// The Glyph key is used to sign all identity operations for cryptographic binding.

pub async fn register_glyph(
    State(s): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<crate::models::RegisterIdentityWithGlyphReq>,
) -> StatusCode {
    if req.glyph_public_key.is_none() { return StatusCode::BAD_REQUEST; }
    let r = sqlx::query(
        r#"UPDATE identities
           SET glyph_public_key = $1,
               glyph_algorithm  = $2,
               last_seen_at     = NOW()
           WHERE identity = $3"#
    )
    .bind(&req.glyph_public_key)
    .bind(req.glyph_algorithm.as_deref().unwrap_or("ECDSA-P256"))
    .bind(&id)
    .execute(&s.pool).await;

    match r {
        Ok(r) if r.rows_affected() > 0 => {
            info!(identity = %id, "glyph key registered");
            StatusCode::NO_CONTENT
        }
        Ok(_) => StatusCode::NOT_FOUND,
        Err(e) => { warn!("register_glyph: {e}"); StatusCode::INTERNAL_SERVER_ERROR }
    }
}

// ── GET /v1/identity/:id ──────────────────────────────────────────────────────

pub async fn get_identity(
    State(s): State<AppState>,
    Path(id): Path<String>,
) -> Result<Json<IdentityResp>, StatusCode> {
    let row = sqlx::query_as::<_, IdentityRow>(
        "SELECT identity, public_key_b64, handle, created_at FROM identities WHERE identity = $1"
    )
    .bind(&id)
    .fetch_optional(&s.pool)
    .await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?
    .ok_or(StatusCode::NOT_FOUND)?;

    let pool = s.pool.clone();
    let id2  = id.clone();
    tokio::spawn(async move {
        let _ = sqlx::query("UPDATE identities SET last_seen_at = NOW() WHERE identity = $1")
            .bind(&id2).execute(&pool).await;
    });

    Ok(Json(IdentityResp {
        identity:       row.identity,
        public_key_b64: row.public_key_b64,
        handle:         row.handle,
        created_at:     row.created_at,
    }))
}

// ── GET /v1/identity/by-handle/:handle ───────────────────────────────────────

pub async fn get_identity_by_handle(
    State(s): State<AppState>,
    Path(handle): Path<String>,
) -> Result<Json<IdentityResp>, StatusCode> {
    let row = sqlx::query_as::<_, IdentityRow>(
        "SELECT identity, public_key_b64, handle, created_at FROM identities WHERE handle = $1 LIMIT 1"
    )
    .bind(&handle)
    .fetch_optional(&s.pool)
    .await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?
    .ok_or(StatusCode::NOT_FOUND)?;

    Ok(Json(IdentityResp {
        identity:       row.identity,
        public_key_b64: row.public_key_b64,
        handle:         row.handle,
        created_at:     row.created_at,
    }))
}

// ── POST /v1/dm ───────────────────────────────────────────────────────────────

pub async fn send_dm(
    State(s): State<AppState>,
    Json(req): Json<crate::models::SendDmReq>,
) -> Result<Json<serde_json::Value>, StatusCode> {
    if req.sender.is_empty() || req.recipient.is_empty()
        || req.ciphertext.is_empty() || req.sender_pub.is_empty() {
        return Err(StatusCode::BAD_REQUEST);
    }

    let id: (uuid::Uuid,) = sqlx::query_as(
        "INSERT INTO direct_messages
             (sender, sender_handle, recipient, sender_pub, ciphertext, iv,
              msg_version, ratchet_pub, recipient_device_id)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
         RETURNING id"
    )
    .bind(&req.sender)
    .bind(&req.sender_handle)
    .bind(&req.recipient)
    .bind(&req.sender_pub)
    .bind(&req.ciphertext)
    .bind(&req.iv)
    .bind(req.msg_version.unwrap_or(1))
    .bind(&req.ratchet_pub)
    .bind(&req.recipient_device_id)
    .fetch_one(&s.pool)
    .await
    .map_err(|e| { warn!("send_dm: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    Ok(Json(serde_json::json!({ "id": id.0, "status": "SENT" })))
}

// ── GET /v1/dm/:identity ──────────────────────────────────────────────────────

pub async fn fetch_dms(
    State(s): State<AppState>,
    Path(identity): Path<String>,
    Query(params): Query<crate::models::FetchDmQuery>,
) -> Result<Json<Vec<crate::models::DmResp>>, StatusCode> {
    let rows = sqlx::query_as::<_, crate::models::DmRow>(
        "SELECT id, sender, sender_handle, sender_pub, ciphertext, iv,
                msg_version, ratchet_pub, created_at
         FROM direct_messages
         WHERE recipient = $1
           AND (recipient_device_id IS NULL OR $2::text IS NULL OR recipient_device_id = $2)
           AND expires_at > NOW()
         ORDER BY created_at ASC
         LIMIT 200"
    )
    .bind(&identity)
    .bind(&params.device_id)
    .fetch_all(&s.pool)
    .await
    .map_err(|e| { warn!("fetch_dms: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    Ok(Json(rows.into_iter().map(|r| crate::models::DmResp {
        id:            r.id,
        sender:        r.sender,
        sender_handle: r.sender_handle,
        sender_pub:    r.sender_pub,
        ciphertext:    r.ciphertext,
        iv:            r.iv,
        msg_version:   r.msg_version,
        ratchet_pub:   r.ratchet_pub,
        created_at:    r.created_at,
    }).collect()))
}

// ── POST /v1/prekeys/signed ───────────────────────────────────────────────────

pub async fn register_spk(
    State(s):  State<AppState>,
    Json(req): Json<RegisterSpkReq>,
) -> Result<StatusCode, StatusCode> {
    // Verify the SPK signature before storing
    let ik_row = sqlx::query_as::<_, IdentityRow>(
        "SELECT identity, public_key_b64, created_at FROM identities WHERE identity = $1"
    )
    .bind(&req.identity)
    .fetch_optional(&s.pool)
    .await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?
    .ok_or(StatusCode::NOT_FOUND)?;

    let ik_bytes = base64_decode(&ik_row.public_key_b64)
        .ok_or(StatusCode::BAD_REQUEST)?;
    let spk_bytes = base64_decode(&req.public_key_b64)
        .ok_or(StatusCode::BAD_REQUEST)?;

    if crate::x3dh::verify_spk_signature(&ik_bytes, &spk_bytes, &req.signature_b64).is_err() {
        return Err(StatusCode::UNPROCESSABLE_ENTITY);
    }

    sqlx::query(
        "INSERT INTO signed_prekeys (identity, key_id, public_key_b64, signature_b64)
         VALUES ($1, $2, $3, $4)
         ON CONFLICT (identity, key_id) DO UPDATE SET
             public_key_b64 = EXCLUDED.public_key_b64,
             signature_b64  = EXCLUDED.signature_b64,
             updated_at     = NOW()"
    )
    .bind(&req.identity).bind(req.key_id)
    .bind(&req.public_key_b64).bind(&req.signature_b64)
    .execute(&s.pool)
    .await
    .map_err(|e| { warn!("register spk: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    Ok(StatusCode::NO_CONTENT)
}

// ── POST /v1/prekeys/onetime ──────────────────────────────────────────────────

pub async fn upload_otpk(
    State(s):  State<AppState>,
    Json(req): Json<UploadOtpkReq>,
) -> Result<Json<CountResp>, StatusCode> {
    let mut tx = s.pool.begin().await.map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;
    let mut count = 0u64;
    for pk in &req.prekeys {
        if sqlx::query(
            "INSERT INTO one_time_prekeys (identity, key_id, public_key_b64)
             VALUES ($1, $2, $3) ON CONFLICT (identity, key_id) DO NOTHING"
        )
        .bind(&req.identity).bind(pk.key_id).bind(&pk.public_key_b64)
        .execute(&mut *tx).await.is_ok() { count += 1; }
    }
    tx.commit().await.map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;
    Ok(Json(CountResp { count }))
}

// ── GET /v1/prekeys/:identity ─────────────────────────────────────────────────
// Returns a full PrekeyBundle (IK + SPK + optional OPK). Consuming the OPK is atomic.

pub async fn get_prekey_bundle(
    State(s): State<AppState>,
    Path(identity): Path<String>,
) -> Result<Json<PrekeyBundleResp>, StatusCode> {
    // 1. Fetch identity key
    let ik_row = sqlx::query_as::<_, IdentityRow>(
        "SELECT identity, public_key_b64, created_at FROM identities WHERE identity = $1"
    )
    .bind(&identity)
    .fetch_optional(&s.pool).await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?
    .ok_or(StatusCode::NOT_FOUND)?;

    // 2. Fetch latest signed prekey
    let spk: Option<(i32, String, String)> = sqlx::query_as(
        "SELECT key_id, public_key_b64, signature_b64
         FROM signed_prekeys WHERE identity = $1
         ORDER BY updated_at DESC LIMIT 1"
    )
    .bind(&identity)
    .fetch_optional(&s.pool).await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    let (spk_id, spk_pub, spk_sig) = spk.ok_or(StatusCode::NOT_FOUND)?;

    // 3. Atomically claim one OPK (optional)
    let opk: Option<(i32, String)> = sqlx::query_as(
        r#"WITH claimed AS (
               SELECT id, key_id, public_key_b64
               FROM one_time_prekeys
               WHERE identity = $1
               ORDER BY id ASC LIMIT 1
               FOR UPDATE SKIP LOCKED
           )
           DELETE FROM one_time_prekeys USING claimed
           WHERE one_time_prekeys.id = claimed.id
           RETURNING claimed.key_id, claimed.public_key_b64"#
    )
    .bind(&identity)
    .fetch_optional(&s.pool).await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    // Warn client if OPK count is low (so they can replenish)
    let otpk_count: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM one_time_prekeys WHERE identity = $1"
    )
    .bind(&identity)
    .fetch_one(&s.pool).await.unwrap_or(0);

    use crate::x3dh::PrekeyBundle;
    let bundle = PrekeyBundle {
        identity:      ik_row.identity,
        ik_public:     ik_row.public_key_b64,
        spk_public:    spk_pub,
        spk_id,
        spk_signature: spk_sig,
        opk_public:    opk.as_ref().map(|o| o.1.clone()),
        opk_id:        opk.as_ref().map(|o| o.0),
    };

    if otpk_count < 5 {
        warn!("identity {}… has only {} OPKs remaining — push replenish request", &identity[..8], otpk_count);
        // In production: push a System message to the identity asking for key replenishment
    }

    Ok(Json(PrekeyBundleResp { bundle }))
}

// ── POST /v1/messages ─────────────────────────────────────────────────────────

pub async fn send_message(
    State(s):  State<AppState>,
    Json(req): Json<SendMessageReq>,
) -> Result<Json<SendMessageResp>, StatusCode> {
    if req.recipient.is_empty() || req.sender.is_empty() || req.chunks.is_empty() {
        return Err(StatusCode::BAD_REQUEST);
    }

    // Rate limit check
    let is_known = known_contact(&s.pool, &req.sender, &req.recipient).await;
    let mode = req.security_mode;
    if !s.rate_limiter.allow(&req.sender, is_known, mode) {
        return Err(StatusCode::TOO_MANY_REQUESTS);
    }

    // First-contact PoW verification
    if !is_known {
        match req.pow_nonce {
            None => {
                warn!("first-contact message from {}… missing PoW", &req.sender[..8]);
                return Err(StatusCode::PAYMENT_REQUIRED); // 402 = "PoW required"
            }
            Some(nonce) => {
                if !verify_first_contact_pow(&req.sender, &req.recipient, nonce, mode) {
                    warn!("PoW failed from {}…", &req.sender[..8]);
                    return Err(StatusCode::FORBIDDEN);
                }
                // Record as known contact
                let _ = sqlx::query(
                    "INSERT INTO known_contacts (sender, recipient) VALUES ($1, $2) ON CONFLICT DO NOTHING"
                )
                .bind(&req.sender).bind(&req.recipient)
                .execute(&s.pool).await;
            }
        }
    }

    let msg_id     = Uuid::new_v4();
    let ttl        = Duration::seconds(mode.ttl_secs());
    let expires_at = Utc::now() + ttl;

    let chunks_json = serde_json::to_value(&req.chunks)
        .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    sqlx::query(
        r#"INSERT INTO messages
           (id, recipient, sender, security_mode, msg_type,
            dh_public_b64, msg_n, prev_n,
            x3dh_ek_b64, x3dh_spk_id, x3dh_opk_id,
            chunks, expires_at)
           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)"#
    )
    .bind(msg_id)
    .bind(&req.recipient).bind(&req.sender)
    .bind(mode.as_str()).bind(format!("{:?}", req.msg_type))
    .bind(&req.dh_public_b64).bind(req.msg_n).bind(req.prev_n)
    .bind(&req.x3dh_ek_b64).bind(req.x3dh_spk_id).bind(req.x3dh_opk_id)
    .bind(&chunks_json).bind(expires_at)
    .execute(&s.pool)
    .await
    .map_err(|e| { warn!("send message: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    info!(
        sender    = &req.sender[..8.min(req.sender.len())],
        recipient = &req.recipient[..8.min(req.recipient.len())],
        mode      = mode.as_str(),
        chunks    = req.chunks.len(),
        "message queued"
    );

    // Push notification to online recipient (falls through if offline)
    if s.push.notify(&req.recipient, &msg_id.to_string()) {
        info!("push delivered to online recipient");
    }

    // Emit brain signal (fire-and-forget)
    let brain = Arc::clone(&s.brain);
    let sender = req.sender.clone();
    tokio::spawn(async move {
        brain.emit_signal(&sender, &sender, "message_sent").await;
    });

    Ok(Json(SendMessageResp {
        message_id: msg_id,
        status:     "SENT".into(),
        expires_at,
    }))
}

// ── GET /v1/messages/:identity ────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct FetchQuery {
    /// Pagination cursor: return only messages after this UUID.
    after: Option<String>,
    limit: Option<i64>,
}

pub async fn fetch_messages(
    State(s):       State<AppState>,
    Path(identity): Path<String>,
    Query(q):       Query<FetchQuery>,
) -> Result<Json<Vec<PendingMessageResp>>, StatusCode> {
    let limit = q.limit.unwrap_or(50).min(100);

    // Parse the `after` cursor into a UUID for keyset pagination.
    let after_id: Option<Uuid> = q.after
        .as_deref()
        .map(|s| s.parse::<Uuid>())
        .transpose()
        .map_err(|_| StatusCode::BAD_REQUEST)?;

    let rows = if let Some(cursor) = after_id {
        // Composite keyset pagination: (created_at, id) handles timestamp ties and
        // survives cursor-message deletion (we compare against the values, not the row).
        sqlx::query_as::<_, MessageRow>(
            r#"SELECT m.id, m.sender, m.security_mode, m.msg_type,
                      m.dh_public_b64, m.msg_n, m.prev_n,
                      m.x3dh_ek_b64, m.x3dh_spk_id, m.x3dh_opk_id,
                      m.chunks, m.expires_at, m.created_at
               FROM messages m
               JOIN (SELECT created_at AS cur_ts, id AS cur_id
                     FROM messages WHERE id = $3) AS c ON TRUE
               WHERE m.recipient = $1
                 AND m.status = 'SENT'
                 AND m.expires_at > NOW()
                 AND (m.created_at, m.id) > (c.cur_ts, c.cur_id)
               ORDER BY m.created_at ASC, m.id ASC
               LIMIT $2"#
        )
        .bind(&identity).bind(limit).bind(cursor)
        .fetch_all(&s.pool)
        .await
    } else {
        sqlx::query_as::<_, MessageRow>(
            r#"SELECT id, sender, security_mode, msg_type,
                      dh_public_b64, msg_n, prev_n,
                      x3dh_ek_b64, x3dh_spk_id, x3dh_opk_id,
                      chunks, expires_at, created_at
               FROM messages
               WHERE recipient = $1
                 AND status = 'SENT'
                 AND expires_at > NOW()
               ORDER BY created_at ASC, id ASC
               LIMIT $2"#
        )
        .bind(&identity).bind(limit)
        .fetch_all(&s.pool)
        .await
    }
    .map_err(|e| { warn!("fetch messages: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    let messages = rows.into_iter().map(|r| {
        let chunks: Vec<ChunkReq> = serde_json::from_value(r.chunks).unwrap_or_default();
        PendingMessageResp {
            id:            r.id,
            sender:        r.sender,
            security_mode: r.security_mode,
            msg_type:      r.msg_type,
            dh_public_b64: r.dh_public_b64,
            msg_n:         r.msg_n,
            prev_n:        r.prev_n,
            x3dh_ek_b64:   r.x3dh_ek_b64,
            x3dh_spk_id:   r.x3dh_spk_id,
            x3dh_opk_id:   r.x3dh_opk_id,
            chunks,
            expires_at:    r.expires_at,
            created_at:    r.created_at,
        }
    }).collect();

    Ok(Json(messages))
}

// ── DELETE /v1/messages/:id (ack) ─────────────────────────────────────────────

pub async fn ack_message(
    State(s):  State<AppState>,
    Path(id):  Path<Uuid>,
    Json(req): Json<AckReq>,
) -> StatusCode {
    // Path and body must agree on which message is being acked.
    if req.message_id != id {
        return StatusCode::BAD_REQUEST;
    }

    let status = match req.status.as_str() {
        "DELIVERED" => "DELIVERED",
        "READ"      => "READ",
        _           => return StatusCode::BAD_REQUEST,
    };

    let result = sqlx::query(
        "UPDATE messages SET status = $1, delivered_at = NOW()
         WHERE id = $2 AND recipient = $3 AND status != 'EXPIRED'"
    )
    .bind(status).bind(id).bind(&req.identity)
    .execute(&s.pool).await;

    match result {
        Ok(r) if r.rows_affected() > 0 => {
            // PARANOID: delete immediately after ack
            let is_paranoid = sqlx::query_scalar::<_, String>(
                "SELECT security_mode FROM messages WHERE id = $1"
            )
            .bind(id).fetch_optional(&s.pool).await.unwrap_or(None)
                == Some("PARANOID".to_string());

            if is_paranoid {
                let _ = sqlx::query("DELETE FROM messages WHERE id = $1")
                    .bind(id).execute(&s.pool).await;
            }

            StatusCode::NO_CONTENT
        }
        Ok(_) => StatusCode::NOT_FOUND,
        Err(e) => { warn!("ack: {e}"); StatusCode::INTERNAL_SERVER_ERROR }
    }
}

// ── POST /v1/groups ───────────────────────────────────────────────────────────

pub async fn create_group(
    State(s):  State<AppState>,
    Json(req): Json<CreateGroupReq>,
) -> Result<Json<GroupInfoResp>, StatusCode> {
    if req.group_id.is_empty() || req.members.is_empty() {
        return Err(StatusCode::BAD_REQUEST);
    }

    let mut tx = s.pool.begin().await.map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    sqlx::query(
        "INSERT INTO groups (group_id, member_count) VALUES ($1, $2) ON CONFLICT DO NOTHING"
    )
    .bind(&req.group_id).bind(req.members.len() as i32)
    .execute(&mut *tx).await
    .map_err(|e| { warn!("create group: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    for member in &req.members {
        let _ = sqlx::query(
            "INSERT INTO group_members (group_id, identity) VALUES ($1, $2) ON CONFLICT DO NOTHING"
        )
        .bind(&req.group_id).bind(member)
        .execute(&mut *tx).await;
    }

    tx.commit().await.map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    // Brain signal: group created
    let brain = Arc::clone(&s.brain);
    let creator = req.creator.clone();
    let gid = req.group_id.clone();
    tokio::spawn(async move {
        brain.emit_signal(&creator, &gid, "group_active").await;
    });

    Ok(Json(GroupInfoResp {
        group_id:     req.group_id,
        member_count: req.members.len() as i32,
        members:      req.members,
        created_at:   Utc::now(),
    }))
}

// ── POST /v1/groups/:group_id/members ────────────────────────────────────────

pub async fn add_group_member(
    State(s):  State<AppState>,
    Json(req): Json<AddGroupMemberReq>,
) -> Result<StatusCode, StatusCode> {
    // Only existing members can add new members.
    let is_member: bool = sqlx::query_scalar(
        "SELECT EXISTS(SELECT 1 FROM group_members WHERE group_id = $1 AND identity = $2)"
    )
    .bind(&req.group_id).bind(&req.requester)
    .fetch_one(&s.pool).await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    if !is_member {
        return Err(StatusCode::FORBIDDEN);
    }

    let mut tx = s.pool.begin().await.map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    sqlx::query(
        "INSERT INTO group_members (group_id, identity) VALUES ($1, $2) ON CONFLICT DO NOTHING"
    )
    .bind(&req.group_id).bind(&req.new_member)
    .execute(&mut *tx).await
    .map_err(|e| { warn!("add group member: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    sqlx::query(
        "UPDATE groups SET member_count = (SELECT COUNT(*) FROM group_members WHERE group_id = $1)
         WHERE group_id = $1"
    )
    .bind(&req.group_id)
    .execute(&mut *tx).await
    .map_err(|e| { warn!("update group count: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    tx.commit().await.map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    info!(
        group  = req.group_id.as_str(),
        member = req.new_member.as_str(),
        "group member added"
    );
    Ok(StatusCode::NO_CONTENT)
}

// ── GET /v1/groups/:group_id/members ─────────────────────────────────────────

pub async fn get_group_members(
    State(s):    State<AppState>,
    Path(gid):   Path<String>,
) -> Result<Json<GroupInfoResp>, StatusCode> {
    let members: Vec<(String,)> = sqlx::query_as(
        "SELECT identity FROM group_members WHERE group_id = $1 ORDER BY joined_at ASC"
    )
    .bind(&gid)
    .fetch_all(&s.pool)
    .await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    if members.is_empty() { return Err(StatusCode::NOT_FOUND); }

    let created_at: DateTime<Utc> = sqlx::query_scalar(
        "SELECT created_at FROM groups WHERE group_id = $1"
    )
    .bind(&gid)
    .fetch_one(&s.pool).await
    .map_err(|_| StatusCode::NOT_FOUND)?;

    let member_ids: Vec<String> = members.into_iter().map(|m| m.0).collect();
    Ok(Json(GroupInfoResp {
        group_id:     gid,
        member_count: member_ids.len() as i32,
        members:      member_ids,
        created_at,
    }))
}

// ── WS /v1/push/:identity ─────────────────────────────────────────────────────
// WebSocket endpoint for real-time push. Client connects once; relay pushes
// message IDs as they arrive. Client then polls /v1/messages to fetch full frames.

pub async fn ws_push(
    State(s):    State<AppState>,
    Path(id):    Path<String>,
    upgrade:     WebSocketUpgrade,
) -> Response {
    upgrade.on_upgrade(move |ws| handle_ws(ws, id, s.push))
}

async fn handle_ws(mut ws: WebSocket, identity: String, push: Arc<PushRegistry>) {
    let mut rx = push.subscribe(identity.clone());
    loop {
        tokio::select! {
            msg = rx.recv() => {
                match msg {
                    Ok(msg_id) => {
                        let ev = PushEvent {
                            r#type:     "new_message".into(),
                            message_id: Some(msg_id),
                            ts:         Utc::now(),
                        };
                        let json = serde_json::to_string(&ev).unwrap_or_default();
                        if ws.send(Message::Text(json)).await.is_err() {
                            break;
                        }
                    }
                    Err(_) => break,
                }
            }
            msg = ws.recv() => {
                match msg {
                    Some(Ok(Message::Close(_))) | None => break,
                    _ => {}
                }
            }
        }
    }
    push.unsubscribe(&identity);
}

// ── Relay node registration ───────────────────────────────────────────────────
// POST /v1/relay/nodes — register a relay node with its endpoint + glyph key.
// Relay nodes earn AET karma rewards for reliable message delivery.

pub async fn register_relay_node(
    State(s):  State<AppState>,
    Json(req): Json<crate::models::RegisterRelayReq>,
) -> Result<Json<crate::models::RelayNodeResp>, StatusCode> {
    if req.node_id.is_empty() || req.endpoint.is_empty() || req.glyph_pub_b64.is_empty() {
        return Err(StatusCode::BAD_REQUEST);
    }

    sqlx::query(
        r#"INSERT INTO relay_nodes (node_id, endpoint, glyph_pub_b64, region)
           VALUES ($1, $2, $3, $4)
           ON CONFLICT (node_id) DO UPDATE SET
             endpoint     = EXCLUDED.endpoint,
             glyph_pub_b64 = EXCLUDED.glyph_pub_b64,
             region       = COALESCE(EXCLUDED.region, relay_nodes.region),
             last_seen_at  = NOW(),
             status        = 'active'"#
    )
    .bind(&req.node_id).bind(&req.endpoint)
    .bind(&req.glyph_pub_b64)
    .bind(req.region.as_deref().unwrap_or("global"))
    .execute(&s.pool).await
    .map_err(|e| { warn!("register_relay_node: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    // Initialise karma score row if not present
    sqlx::query(
        "INSERT INTO karma_scores (node_id) VALUES ($1) ON CONFLICT (node_id) DO NOTHING"
    )
    .bind(&req.node_id).execute(&s.pool).await.ok();

    info!(node = %req.node_id, endpoint = %req.endpoint, "relay node registered");

    Ok(Json(crate::models::RelayNodeResp {
        node_id:        req.node_id,
        endpoint:       req.endpoint,
        region:         req.region.unwrap_or_else(|| "global".into()),
        status:         "active".into(),
        karma:          50.0,
        avg_latency_ms: 100.0,
        last_seen_at:   Utc::now(),
    }))
}

// ── Relay node heartbeat ───────────────────────────────────────────────────────
// POST /v1/relay/nodes/:id/heartbeat — update liveness + self-reported latency.

pub async fn relay_heartbeat(
    State(s):    State<AppState>,
    Path(id):    Path<String>,
    Json(req):   Json<crate::models::RelayHeartbeatReq>,
) -> StatusCode {
    let r = sqlx::query(
        "UPDATE relay_nodes SET last_seen_at = NOW(), status = 'active' WHERE node_id = $1"
    )
    .bind(&id).execute(&s.pool).await;

    if r.map(|r| r.rows_affected() == 0).unwrap_or(true) {
        return StatusCode::NOT_FOUND;
    }

    if let Some(lat) = req.latency_ms {
        // Exponential moving average for latency
        let _ = sqlx::query(
            "UPDATE karma_scores SET avg_latency_ms = avg_latency_ms * 0.8 + $1 * 0.2,
             updated_at = NOW() WHERE node_id = $2"
        )
        .bind(lat as f32).bind(&id).execute(&s.pool).await;
    }

    StatusCode::NO_CONTENT
}

// ── GET /v1/relay/nodes — list active relay nodes sorted by karma ─────────────

pub async fn list_relay_nodes(
    State(s): State<AppState>,
) -> Result<Json<Vec<crate::models::RelayNodeResp>>, StatusCode> {
    // Mark nodes offline if not seen in 120s
    let _ = sqlx::query(
        "UPDATE relay_nodes SET status = 'offline'
         WHERE status = 'active' AND last_seen_at < NOW() - INTERVAL '120 seconds'"
    )
    .execute(&s.pool).await;

    let rows = sqlx::query_as::<_, (String, String, String, String, f64, f32, DateTime<Utc>)>(
        r#"SELECT r.node_id, r.endpoint, r.region, r.status,
                  COALESCE(k.score, 50.0), COALESCE(k.avg_latency_ms, 100.0), r.last_seen_at
           FROM relay_nodes r
           LEFT JOIN karma_scores k USING (node_id)
           WHERE r.status = 'active'
           ORDER BY COALESCE(k.score, 50.0) DESC
           LIMIT 20"#
    )
    .fetch_all(&s.pool).await
    .map_err(|e| { warn!("list_relay_nodes: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    Ok(Json(rows.into_iter().map(|(node_id, endpoint, region, status, karma, lat, seen)| {
        crate::models::RelayNodeResp { node_id, endpoint, region, status, karma, avg_latency_ms: lat, last_seen_at: seen }
    }).collect()))
}

// ── GET /v1/relay/route/:identity — pick best relay nodes for delivery ─────────

pub async fn get_route(
    State(s):    State<AppState>,
    Path(_identity): Path<String>,
) -> Result<Json<crate::models::RouteResp>, StatusCode> {
    // For now: return top 3 karma nodes. Future: use recipient's region preference.
    let nodes = sqlx::query_as::<_, (String, String, String, String, f64, f32, DateTime<Utc>)>(
        r#"SELECT r.node_id, r.endpoint, r.region, r.status,
                  COALESCE(k.score, 50.0), COALESCE(k.avg_latency_ms, 100.0), r.last_seen_at
           FROM relay_nodes r
           LEFT JOIN karma_scores k USING (node_id)
           WHERE r.status = 'active'
           ORDER BY COALESCE(k.score, 50.0) / GREATEST(COALESCE(k.avg_latency_ms,100.0), 1) DESC
           LIMIT 3"#
    )
    .fetch_all(&s.pool).await
    .map_err(|e| { warn!("get_route: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    let node_list: Vec<_> = nodes.into_iter().map(|(node_id, endpoint, region, status, karma, lat, seen)| {
        crate::models::RelayNodeResp { node_id, endpoint, region, status, karma, avg_latency_ms: lat, last_seen_at: seen }
    }).collect();

    let mode = if node_list.is_empty() { "direct" } else { "relay" };

    Ok(Json(crate::models::RouteResp { nodes: node_list, mode: mode.into() }))
}

// ── POST /v1/relay/karma — record delivery event, update karma score ───────────

pub async fn record_karma_event(
    State(s):  State<AppState>,
    Json(req): Json<crate::models::KarmaEventReq>,
) -> StatusCode {
    // Append event
    let _ = sqlx::query(
        "INSERT INTO karma_events (node_id, event_type, latency_ms, packet_id)
         VALUES ($1, $2, $3, $4)"
    )
    .bind(&req.node_id).bind(&req.event_type)
    .bind(req.latency_ms).bind(&req.packet_id)
    .execute(&s.pool).await;

    // Update karma score
    let (score_delta, latency_bonus): (f64, f64) = match req.event_type.as_str() {
        "delivery_ok" => {
            let lat = req.latency_ms.unwrap_or(200) as f64;
            let bonus = if lat < 50.0 { 0.5 } else if lat < 100.0 { 0.2 } else if lat > 500.0 { -0.2 } else { 0.0 };
            (1.0 + bonus, bonus)
        }
        "delivery_fail" => (-5.0, 0.0),
        _ => return StatusCode::BAD_REQUEST,
    };
    let _ = latency_bonus; // used in calculation above

    let _ = sqlx::query(
        r#"INSERT INTO karma_scores (node_id, score, deliveries_ok, deliveries_fail)
           VALUES ($1, GREATEST(0, LEAST(100, 50.0 + $2)), 0, 0)
           ON CONFLICT (node_id) DO UPDATE SET
             score          = GREATEST(0, LEAST(100, karma_scores.score + $2)),
             deliveries_ok  = karma_scores.deliveries_ok  + CASE WHEN $3 = 'delivery_ok'   THEN 1 ELSE 0 END,
             deliveries_fail = karma_scores.deliveries_fail + CASE WHEN $3 = 'delivery_fail' THEN 1 ELSE 0 END,
             updated_at      = NOW()"#
    )
    .bind(&req.node_id).bind(score_delta).bind(&req.event_type)
    .execute(&s.pool).await;

    StatusCode::NO_CONTENT
}

// ── Ratchet state persistence ──────────────────────────────────────────────────
// Client stores encrypted ratchet state on the relay so it survives page reloads.
// The relay stores the blob but cannot decrypt it.

pub async fn save_ratchet_state(
    State(s):  State<AppState>,
    Json(req): Json<crate::models::SaveRatchetReq>,
) -> StatusCode {
    let r = sqlx::query(
        r#"INSERT INTO ratchet_sessions (local_identity, remote_identity, state_b64, updated_at)
           VALUES ($1, $2, $3, NOW())
           ON CONFLICT (local_identity, remote_identity) DO UPDATE SET
             state_b64  = EXCLUDED.state_b64,
             updated_at = NOW()"#
    )
    .bind(&req.local_identity).bind(&req.remote_identity).bind(&req.state_b64)
    .execute(&s.pool).await;

    if r.is_err() { StatusCode::INTERNAL_SERVER_ERROR } else { StatusCode::NO_CONTENT }
}

pub async fn get_ratchet_state(
    State(s): State<AppState>,
    Path((local, remote)): Path<(String, String)>,
) -> Result<Json<crate::models::RatchetSessionRow>, StatusCode> {
    sqlx::query_as::<_, crate::models::RatchetSessionRow>(
        "SELECT state_b64, updated_at FROM ratchet_sessions
         WHERE local_identity = $1 AND remote_identity = $2"
    )
    .bind(&local).bind(&remote)
    .fetch_optional(&s.pool).await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?
    .ok_or(StatusCode::NOT_FOUND)
    .map(Json)
}

// ─────────────────────────────────────────────────────────────────────────────
// Vovin v2: Multi-device + Aethyr File Fabric
// ─────────────────────────────────────────────────────────────────────────────

// ── POST /v1/devices — register a device (multi-device, non-destructive) ─────
// Each call is idempotent per (pial_id, device_id).
// Does NOT overwrite an existing device's public key.

pub async fn register_device(
    State(s): State<AppState>,
    Json(req): Json<crate::models::RegisterDeviceReq>,
) -> Result<Json<serde_json::Value>, StatusCode> {
    if req.pial_id.is_empty() || req.device_id.is_empty() || req.public_key_b64.is_empty() {
        return Err(StatusCode::BAD_REQUEST);
    }

    sqlx::query(
        r#"INSERT INTO devices (device_id, pial_id, public_key_b64, glyph_pub_b64, handle, device_name)
           VALUES ($1, $2, $3, $4, $5, $6)
           ON CONFLICT (pial_id, device_id) DO UPDATE SET
               last_seen_at = NOW(),
               handle       = COALESCE(EXCLUDED.handle, devices.handle),
               device_name  = COALESCE(EXCLUDED.device_name, devices.device_name)"#
    )
    .bind(&req.device_id)
    .bind(&req.pial_id)
    .bind(&req.public_key_b64)
    .bind(req.glyph_pub_b64.as_deref())
    .bind(req.handle.as_deref())
    .bind(req.device_name.as_deref())
    .execute(&s.pool)
    .await
    .map_err(|e| { warn!("register_device: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    // Also register in legacy identities table for backward compat
    // (first device wins — subsequent devices don't overwrite the key)
    let _ = sqlx::query(
        r#"INSERT INTO identities (identity, public_key_b64, handle, device_id)
           VALUES ($1, $2, $3, $4)
           ON CONFLICT (identity) DO UPDATE SET
               handle = COALESCE(EXCLUDED.handle, identities.handle),
               last_seen_at = NOW()"#
    )
    .bind(&req.pial_id)
    .bind(&req.public_key_b64)
    .bind(req.handle.as_deref())
    .bind(&req.device_id)
    .execute(&s.pool)
    .await;

    info!("device registered: {}:{}", &req.pial_id[..8.min(req.pial_id.len())], &req.device_id[..8.min(req.device_id.len())]);

    Ok(Json(serde_json::json!({
        "pial_id":   req.pial_id,
        "device_id": req.device_id,
        "status":    "registered"
    })))
}

// ── GET /v1/devices/:pial_id — list all registered devices for a PIAL ────────

pub async fn list_devices(
    State(s): State<AppState>,
    Path(pial_id): Path<String>,
) -> Result<Json<serde_json::Value>, StatusCode> {
    let rows: Vec<(String, String, String, DateTime<Utc>)> = sqlx::query_as(
        "SELECT device_id, device_name, public_key_b64, last_seen_at FROM devices
         WHERE pial_id = $1 ORDER BY last_seen_at DESC"
    )
    .bind(&pial_id)
    .fetch_all(&s.pool)
    .await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    let devices: Vec<serde_json::Value> = rows.iter().map(|(id, name, pub_key, seen)| serde_json::json!({
        "device_id":      id,
        "device_name":    name,
        "public_key_b64": pub_key,
        "last_seen_at":   seen
    })).collect();

    Ok(Json(serde_json::json!({ "pial_id": pial_id, "devices": devices })))
}

// ── POST /v1/aff/upload — create file manifest + upload chunk ─────────────────
// Body: { pial_id, filename, mime_type, size_bytes, chunk_count, hash_root,
//          chunk_index, nonce_b64, chunk_hash, ciphertext_b64 }

pub async fn aff_upload_chunk(
    State(s): State<AppState>,
    Json(req): Json<crate::models::AffChunkReq>,
) -> Result<Json<serde_json::Value>, StatusCode> {
    if req.pial_id.is_empty() || req.ciphertext_b64.is_empty() {
        return Err(StatusCode::BAD_REQUEST);
    }

    let file_id: uuid::Uuid = if let Some(fid) = req.file_id {
        // Uploading a chunk to an existing manifest
        fid
    } else {
        // First chunk — create manifest
        let id: (uuid::Uuid,) = sqlx::query_as(
            r#"INSERT INTO file_manifests
                (uploader_pial, filename, mime_type, size_bytes, chunk_count, hash_root)
               VALUES ($1, $2, $3, $4, $5, $6)
               RETURNING file_id"#
        )
        .bind(&req.pial_id)
        .bind(req.filename.as_deref().unwrap_or("file"))
        .bind(req.mime_type.as_deref().unwrap_or("application/octet-stream"))
        .bind(req.size_bytes.unwrap_or(0))
        .bind(req.chunk_count.unwrap_or(1))
        .bind(req.hash_root.as_deref().unwrap_or(""))
        .fetch_one(&s.pool)
        .await
        .map_err(|e| { warn!("aff manifest: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;
        id.0
    };

    // Insert the chunk
    sqlx::query(
        r#"INSERT INTO file_chunks (file_id, chunk_index, chunk_hash, ciphertext_b64, nonce_b64, size_bytes)
           VALUES ($1, $2, $3, $4, $5, $6)
           ON CONFLICT (file_id, chunk_index) DO NOTHING"#
    )
    .bind(file_id)
    .bind(req.chunk_index)
    .bind(&req.chunk_hash)
    .bind(&req.ciphertext_b64)
    .bind(&req.nonce_b64)
    .bind(req.ciphertext_b64.len() as i32 * 3 / 4) // approx bytes
    .execute(&s.pool)
    .await
    .map_err(|e| { warn!("aff chunk: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    // Update chunks_received count + mark complete if all received
    sqlx::query(
        r#"UPDATE file_manifests
           SET chunks_received = (SELECT COUNT(*) FROM file_chunks WHERE file_id = $1),
               status = CASE
                   WHEN (SELECT COUNT(*) FROM file_chunks WHERE file_id = $1) >= chunk_count
                   THEN 'complete' ELSE 'uploading'
               END
           WHERE file_id = $1"#
    )
    .bind(file_id)
    .execute(&s.pool)
    .await
    .map_err(|e| { warn!("aff update: {e}"); StatusCode::INTERNAL_SERVER_ERROR })?;

    let status: (String, i32, i32) = sqlx::query_as(
        "SELECT status, chunks_received, chunk_count FROM file_manifests WHERE file_id = $1"
    )
    .bind(file_id)
    .fetch_one(&s.pool)
    .await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    Ok(Json(serde_json::json!({
        "file_id":         file_id,
        "chunk_index":     req.chunk_index,
        "status":          status.0,
        "chunks_received": status.1,
        "chunk_count":     status.2,
    })))
}

// ── GET /v1/aff/:file_id — get file manifest ──────────────────────────────────

pub async fn aff_get_manifest(
    State(s): State<AppState>,
    Path(file_id): Path<uuid::Uuid>,
) -> Result<Json<serde_json::Value>, StatusCode> {
    let row: Option<(uuid::Uuid, String, String, String, i64, i32, i32, String, DateTime<Utc>)> = sqlx::query_as(
        "SELECT file_id, filename, mime_type, status, size_bytes, chunk_count, chunks_received, hash_root, expires_at
         FROM file_manifests WHERE file_id = $1"
    )
    .bind(file_id)
    .fetch_optional(&s.pool)
    .await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    let (fid, fname, mime, status, size, count, received, hash, expires) =
        row.ok_or(StatusCode::NOT_FOUND)?;

    Ok(Json(serde_json::json!({
        "file_id":         fid,
        "filename":        fname,
        "mime_type":       mime,
        "status":          status,
        "size_bytes":      size,
        "chunk_count":     count,
        "chunks_received": received,
        "hash_root":       hash,
        "expires_at":      expires,
    })))
}

// ── GET /v1/aff/:file_id/chunk/:index — download a chunk ─────────────────────

pub async fn aff_get_chunk(
    State(s): State<AppState>,
    Path((file_id, chunk_index)): Path<(uuid::Uuid, i32)>,
) -> Result<Json<serde_json::Value>, StatusCode> {
    let row: Option<(String, String, String, i32)> = sqlx::query_as(
        "SELECT ciphertext_b64, nonce_b64, chunk_hash, size_bytes FROM file_chunks
         WHERE file_id = $1 AND chunk_index = $2"
    )
    .bind(file_id)
    .bind(chunk_index)
    .fetch_optional(&s.pool)
    .await
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;

    let (ct, nonce, hash, size) = row.ok_or(StatusCode::NOT_FOUND)?;

    Ok(Json(serde_json::json!({
        "file_id":       file_id,
        "chunk_index":   chunk_index,
        "ciphertext_b64": ct,
        "nonce_b64":     nonce,
        "chunk_hash":    hash,
        "size_bytes":    size,
    })))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

async fn known_contact(pool: &PgPool, sender: &str, recipient: &str) -> bool {
    sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS(SELECT 1 FROM known_contacts WHERE sender = $1 AND recipient = $2)"
    )
    .bind(sender).bind(recipient)
    .fetch_one(pool).await.unwrap_or(false)
}

fn base64_decode(s: &str) -> Option<Vec<u8>> {
    use base64::{engine::general_purpose::URL_SAFE_NO_PAD as B64, Engine};
    B64.decode(s).ok()
}
