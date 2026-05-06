/// AMP v1 wire protocol types.
///
/// Every AMP message is composed of three layers:
///
///   ┌─────────────────────────────────────────────────────────────┐
///   │  ENVELOPE   (routing + metadata, authenticated + encrypted) │
///   ├─────────────────────────────────────────────────────────────┤
///   │  KEY CAPSULE (ratchet header for key derivation)           │
///   ├─────────────────────────────────────────────────────────────┤
///   │  PAYLOAD CHUNKS  [ chunk_0 | chunk_1 | ... ]               │
///   │  Each chunk: independently encrypted, content-hashed        │
///   └─────────────────────────────────────────────────────────────┘
///
/// Security level per layer:
///   Envelope:    authenticated with sender's identity — relay cannot forge routing
///   Key Capsule: contains ratchet public key; ratchet state is client-side only
///   Chunks:      encrypted with per-chunk key derived from the ratchet message key
///
/// The relay stores the full AmpFrame but can read only:
///   - recipient identity (needed for routing)
///   - security mode
///   - message type
///   - total size (unavoidable)
///   - timestamp + TTL

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

// ── Message types ─────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum MsgType {
    /// Plain text or rich-text message.
    Text        = 1,
    /// Binary media (image, audio, video, file).
    Media       = 2,
    /// Ain Soph token transfer request (signed, not executed by relay).
    TokenTransfer = 3,
    /// Delivery acknowledgement (DELIVERED / READ receipts).
    Ack         = 4,
    /// Group message (encrypted with Sender Key).
    Group       = 5,
    /// System / protocol message (key rotation, prekey replenishment request).
    System      = 6,
}

// ── Security modes ────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum SecurityMode {
    /// Minimum hops, lowest latency. No DH ratchet advancement.
    Fast,
    /// Full Double Ratchet with relay-based delivery. Default.
    #[default]
    Secure,
    /// Multi-path routing, delete-after-read, 24h TTL, no device persistence.
    Paranoid,
}

impl SecurityMode {
    pub fn from_str(s: &str) -> Self {
        match s.to_uppercase().as_str() {
            "FAST"     => Self::Fast,
            "PARANOID" => Self::Paranoid,
            _          => Self::Secure,
        }
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Fast     => "FAST",
            Self::Secure   => "SECURE",
            Self::Paranoid => "PARANOID",
        }
    }

    pub fn ttl_secs(&self) -> i64 {
        match self {
            Self::Fast     => 7 * 24 * 3600,  // 7 days
            Self::Secure   => 7 * 24 * 3600,  // 7 days
            Self::Paranoid => 24 * 3600,       // 24 hours — burn after reading
        }
    }

    /// PoW difficulty for first-contact messages (higher = more CPU cost for spammers).
    pub fn pow_difficulty(&self) -> u8 {
        match self {
            Self::Fast     => 12,
            Self::Secure   => 16,
            Self::Paranoid => 20,
        }
    }
}

// ── Delivery status ───────────────────────────────────────────────────────────

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum DeliveryStatus {
    /// Relay accepted the message.
    Sent,
    /// Recipient device fetched the message from relay.
    Delivered,
    /// Recipient explicitly marked as read (privacy: optional, off by default).
    Read,
    /// Message TTL expired before delivery.
    Expired,
}

// ── AMP wire frame ────────────────────────────────────────────────────────────

/// Full AMP v1 message frame stored on relay and transmitted to recipient.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AmpFrame {
    pub version:       u8,            // = 1
    pub msg_type:      MsgType,
    pub security_mode: SecurityMode,
    pub envelope:      Envelope,
    pub key_capsule:   KeyCapsule,
    pub chunks:        Vec<PayloadChunk>,
}

/// Routing envelope — authenticated but NOT encrypted (relay needs recipient to route).
/// All user-visible metadata is removed — no subject, no thread ID, no group name.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Envelope {
    /// Recipient's AMP identity (BASE58 BLAKE3 hash of their public key).
    pub recipient:     String,
    /// Sender's AMP identity. Relay validates this exists in identity registry.
    pub sender:        String,
    /// Server-assigned message ID (relay sets this, overrides client value).
    pub msg_id:        Uuid,
    /// Client-provided timestamp (relay records its own for ordering).
    pub client_ts:     DateTime<Utc>,
    /// Total chunk count — receiver knows when it has all chunks.
    pub total_chunks:  u32,
    /// For group messages: the group ID (BASE58 hash of group creation key).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub group_id:      Option<String>,
    /// X3DH fields — present only in first message of a new session.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub x3dh_ek_pub:   Option<String>,  // BASE64 ephemeral public key
    #[serde(skip_serializing_if = "Option::is_none")]
    pub x3dh_spk_id:   Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub x3dh_opk_id:   Option<i32>,
    /// Proof-of-work for unknown senders (challenge = recipient_identity || sender_identity).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pow_nonce:     Option<u64>,
}

/// Key Capsule — contains the ratchet header that the recipient needs to derive the message key.
/// The actual message key is NEVER transmitted; it is derived by both parties from ratchet state.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct KeyCapsule {
    /// Sender's current ratchet public key (advances on each DH ratchet step).
    pub dh_public_b64: String,
    /// Message number in the current sending chain.
    pub msg_n:         u32,
    /// Number of messages in the previous sending chain.
    pub prev_n:        u32,
}

impl KeyCapsule {
    /// Compute the AAD that binds this capsule to AEAD operations.
    /// Changing any ratchet header field invalidates all chunk authentication tags.
    pub fn aad(&self, envelope: &Envelope) -> Vec<u8> {
        format!(
            "AMP:v1:{}:{}:{}:{}:{}:{}",
            envelope.recipient, envelope.sender,
            envelope.msg_id, self.dh_public_b64,
            self.msg_n, self.prev_n,
        ).into_bytes()
    }
}

/// A single encrypted payload chunk.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PayloadChunk {
    /// Zero-based index in the overall message.
    pub index:        u32,
    /// BLAKE3 hash of the plaintext chunk (hex string).
    /// Receiver verifies this after decryption — detects corruption AND tampering.
    pub content_hash: String,
    /// ChaCha20-Poly1305 ciphertext: BASE64(nonce || ct || tag).
    pub ciphertext:   String,
}

// ── Token transfer payload ────────────────────────────────────────────────────

/// Plaintext structure inside a TOKEN_TRANSFER message's chunk.
/// This is the payload that gets encrypted — relay never sees token amounts.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TokenTransferPayload {
    /// Ain Soph idempotency key (prevents double-spend).
    pub idempotency_key: String,
    /// Amount in cents.
    pub amount_cents:    i64,
    /// "tip" | "subscription" | "payment" (determines AethyrRank reward signal).
    pub transfer_type:   String,
    /// Optional memo visible to recipient only (stays encrypted).
    pub memo:            String,
}

// ── Ack payload ───────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AckPayload {
    pub original_msg_id: Uuid,
    pub status:          DeliveryStatus,
    pub ts:              DateTime<Utc>,
}

// ── Group message support ─────────────────────────────────────────────────────

/// Distribution message: Alice distributes her Sender Key to a new group member.
/// Sent as a private SECURE message to each member individually.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SenderKeyDistribution {
    pub group_id:    String,
    /// Sender Key chain key (32 bytes, BASE64).
    pub chain_key:   String,
    /// Current iteration index in the Sender Key chain.
    pub iteration:   u32,
    /// Sender's signing key public part (for sender key message auth).
    pub signing_key: String,
}
