use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::protocol::{MsgType, SecurityMode};
use crate::x3dh::PrekeyBundle;

// ── Identity endpoints ────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct RegisterIdentityReq {
    /// User UUID from feed-engine (stable, 36 chars).
    pub identity:       String,
    /// URL-safe BASE64 of ECDH-P256 public key.
    pub public_key_b64: String,
    /// Optional human-readable handle for routing lookups.
    pub handle:         Option<String>,
    /// Optional device ID — identifies this browser/device instance.
    /// Allows per-device key tracking without exposing device fingerprints.
    pub device_id:      Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct RegisterSpkReq {
    pub identity:       String,
    pub key_id:         i32,
    pub public_key_b64: String,
    /// BLAKE3-keyed(identity_key_bytes, spk_bytes) — prevents injection.
    pub signature_b64:  String,
}

#[derive(Debug, Deserialize)]
pub struct UploadOtpkReq {
    pub identity: String,
    pub prekeys:  Vec<OtpkEntry>,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct OtpkEntry {
    pub key_id:     i32,
    pub public_key_b64: String,
}

#[derive(Debug, Serialize)]
pub struct IdentityResp {
    pub identity:       String,
    pub public_key_b64: String,
    pub handle:         Option<String>,
    pub created_at:     DateTime<Utc>,
}

// ── V1 DM relay ───────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct SendDmReq {
    pub sender:               String,
    pub sender_handle:        Option<String>,
    pub recipient:            String,
    pub sender_pub:           String,
    pub ciphertext:           String,
    pub iv:                   String,
    pub msg_version:          Option<i32>,    // 1 = ECDH, 2 = Double Ratchet
    pub ratchet_pub:          Option<String>, // DR ratchet DH public key (v2 only)
    pub recipient_device_id:  Option<String>, // target device; NULL = all devices
}

#[derive(Debug, Serialize)]
pub struct DmResp {
    pub id:            Uuid,
    pub sender:        String,
    pub sender_handle: Option<String>,
    pub sender_pub:    String,
    pub ciphertext:    String,
    pub iv:            String,
    pub msg_version:   i32,
    pub ratchet_pub:   Option<String>,
    pub created_at:    DateTime<Utc>,
}

#[derive(sqlx::FromRow)]
pub struct DmRow {
    pub id:            Uuid,
    pub sender:        String,
    pub sender_handle: Option<String>,
    pub sender_pub:    String,
    pub ciphertext:    String,
    pub iv:            String,
    pub msg_version:   i32,
    pub ratchet_pub:   Option<String>,
    pub created_at:    DateTime<Utc>,
}

/// Query params for GET /v1/dm/:identity
#[derive(Debug, Deserialize)]
pub struct FetchDmQuery {
    pub device_id: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct PrekeyBundleResp {
    pub bundle: PrekeyBundle,
}

// ── Message send/receive ──────────────────────────────────────────────────────

/// Client sends this to deliver a message to the relay.
#[derive(Debug, Deserialize)]
pub struct SendMessageReq {
    pub recipient:      String,
    pub sender:         String,
    pub security_mode:  SecurityMode,
    pub msg_type:       MsgType,
    // Ratchet header
    pub dh_public_b64:  String,
    pub msg_n:          i32,
    pub prev_n:         i32,
    // X3DH initial session fields (present only for first message to a new session)
    pub x3dh_ek_b64:    Option<String>,
    pub x3dh_spk_id:    Option<i32>,
    pub x3dh_opk_id:    Option<i32>,
    // Payload chunks (independently encrypted)
    pub chunks:         Vec<ChunkReq>,
    // PoW for first-contact messages from unknown senders
    pub pow_nonce:      Option<u64>,
}

/// One encrypted chunk within a SendMessageReq.
#[derive(Debug, Serialize, Deserialize, Clone)]
pub struct ChunkReq {
    pub index:        u32,
    pub content_hash: String,   // BLAKE3 hex of plaintext chunk (for integrity)
    pub ciphertext:   String,   // BASE64(nonce || ct || tag)
}

#[derive(Debug, Serialize)]
pub struct SendMessageResp {
    pub message_id: Uuid,
    pub status:     String,     // "SENT"
    pub expires_at: DateTime<Utc>,
}

/// Message returned when recipient polls the relay.
#[derive(Debug, Serialize)]
pub struct PendingMessageResp {
    pub id:            Uuid,
    pub sender:        String,
    pub security_mode: String,
    pub msg_type:      String,
    pub dh_public_b64: String,
    pub msg_n:         i32,
    pub prev_n:        i32,
    pub x3dh_ek_b64:   Option<String>,
    pub x3dh_spk_id:   Option<i32>,
    pub x3dh_opk_id:   Option<i32>,
    pub chunks:        Vec<ChunkReq>,
    pub expires_at:    DateTime<Utc>,
    pub created_at:    DateTime<Utc>,
}

/// Acknowledge receipt, optionally with read status.
#[derive(Debug, Deserialize)]
pub struct AckReq {
    pub message_id: Uuid,
    /// "DELIVERED" or "READ"
    pub status:     String,
    /// Recipient identity — required for authorisation (only recipient can ack).
    pub identity:   String,
}

// ── Groups ────────────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct CreateGroupReq {
    pub group_id:    String,
    /// Creator's AMP identity.
    pub creator:     String,
    /// Initial member identities (including creator).
    pub members:     Vec<String>,
}

#[derive(Debug, Deserialize)]
pub struct AddGroupMemberReq {
    pub group_id:  String,
    pub requester: String,
    pub new_member: String,
}

#[derive(Debug, Serialize)]
pub struct GroupInfoResp {
    pub group_id:     String,
    pub member_count: i32,
    pub members:      Vec<String>,
    pub created_at:   DateTime<Utc>,
}

// ── WebSocket push ────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct PushEvent {
    pub r#type:     String,    // "new_message" | "ack" | "key_replenish"
    pub message_id: Option<String>,
    pub ts:         DateTime<Utc>,
}

// ── DB rows ───────────────────────────────────────────────────────────────────

#[derive(sqlx::FromRow)]
pub struct IdentityRow {
    pub identity:       String,
    pub public_key_b64: String,
    pub handle:         Option<String>,
    pub created_at:     DateTime<Utc>,
}

#[derive(sqlx::FromRow)]
pub struct MessageRow {
    pub id:             Uuid,
    pub sender:         String,
    pub security_mode:  String,
    pub msg_type:       String,
    pub dh_public_b64:  String,
    pub msg_n:          i32,
    pub prev_n:         i32,
    pub x3dh_ek_b64:    Option<String>,
    pub x3dh_spk_id:    Option<i32>,
    pub x3dh_opk_id:    Option<i32>,
    pub chunks:         serde_json::Value,
    pub expires_at:     DateTime<Utc>,
    pub created_at:     DateTime<Utc>,
}

// ── Relay nodes ───────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct RegisterRelayReq {
    pub node_id:       String,   // Ed25519 pubkey hash (unique node ID)
    pub endpoint:      String,   // reachable address: wss://host:port
    pub glyph_pub_b64: String,   // ECDSA-P256 public key for signing
    pub region:        Option<String>,
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct RelayNodeResp {
    pub node_id:       String,
    pub endpoint:      String,
    pub region:        String,
    pub status:        String,
    pub karma:         f64,
    pub avg_latency_ms: f32,
    pub last_seen_at:  DateTime<Utc>,
}

#[derive(Debug, Deserialize)]
pub struct RelayHeartbeatReq {
    pub node_id:       String,
    pub latency_ms:    Option<i32>,  // self-reported avg latency
}

#[derive(Debug, Deserialize)]
pub struct KarmaEventReq {
    pub node_id:    String,
    pub event_type: String,   // delivery_ok | delivery_fail
    pub latency_ms: Option<i32>,
    pub packet_id:  Option<String>,
}

#[derive(Debug, Serialize)]
pub struct RouteResp {
    pub nodes:   Vec<RelayNodeResp>,
    pub mode:    String,
}

// ── Identity with Glyph ───────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct RegisterIdentityWithGlyphReq {
    pub identity:          String,
    pub public_key_b64:    String,
    pub handle:            Option<String>,
    pub device_id:         Option<String>,
    pub glyph_public_key:  Option<String>,  // ECDSA-P256 public key
    pub glyph_algorithm:   Option<String>,  // default: ECDSA-P256
}

// ── Ratchet session ───────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct SaveRatchetReq {
    pub local_identity:  String,
    pub remote_identity: String,
    pub state_b64:       String,  // encrypted ratchet state from client
}

#[derive(Debug, Serialize, sqlx::FromRow)]
pub struct RatchetSessionRow {
    pub state_b64:  String,
    pub updated_at: DateTime<Utc>,
}

// ── Vovin v2: Multi-device + AFF models ──────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct RegisterDeviceReq {
    pub pial_id:        String,
    pub device_id:      String,
    pub public_key_b64: String,
    pub glyph_pub_b64:  Option<String>,
    pub handle:         Option<String>,
    pub device_name:    Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct AffChunkReq {
    pub pial_id:        String,
    pub file_id:        Option<uuid::Uuid>,
    pub chunk_index:    i32,
    pub chunk_hash:     String,
    pub ciphertext_b64: String,
    pub nonce_b64:      String,
    pub filename:       Option<String>,
    pub mime_type:      Option<String>,
    pub size_bytes:     Option<i64>,
    pub chunk_count:    Option<i32>,
    pub hash_root:      Option<String>,
}

// ── Misc ──────────────────────────────────────────────────────────────────────

#[derive(Debug, Serialize)]
pub struct CountResp { pub count: u64 }

#[allow(dead_code)]
#[derive(Debug, Serialize)]
pub struct StatusResp { pub status: &'static str }
