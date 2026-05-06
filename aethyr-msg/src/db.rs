/// AMP v1 relay database schema.
///
/// Design principles:
///   - Relay stores ONLY ciphertext. No plaintext anywhere.
///   - Identity registry is the only "user data" — just a public key map.
///   - Message rows contain opaque blobs with routing metadata only.
///   - All tables have TTL / expiry mechanisms for data minimisation.
///   - Schema is additive-only for forward compatibility.

use anyhow::Result;
use sqlx::PgPool;

const SCHEMA: &str = r#"
-- ── Identity registry ────────────────────────────────────────────────────────
-- Maps AMP identity strings to X25519 public keys.
-- Identity = user UUID from feed-engine (36 chars, stable, globally unique)
CREATE TABLE IF NOT EXISTS identities (
    identity      TEXT        PRIMARY KEY,
    public_key_b64 TEXT       NOT NULL,   -- URL-safe base64 ECDH-P256 public key
    handle        TEXT,                   -- human-readable handle for routing
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_identities_seen   ON identities(last_seen_at);
-- Additive migrations for columns added after initial deploy
ALTER TABLE identities ADD COLUMN IF NOT EXISTS handle    TEXT;
ALTER TABLE identities ADD COLUMN IF NOT EXISTS device_id TEXT;
CREATE INDEX IF NOT EXISTS idx_identities_handle   ON identities(handle) WHERE handle IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_identities_device   ON identities(identity, device_id) WHERE device_id IS NOT NULL;

-- V1 DM relay: lightweight store-and-forward for ECDH-AES-GCM encrypted messages.
-- No X3DH/ratchet headers needed — sender includes ephemeral pub key per message.
CREATE TABLE IF NOT EXISTS direct_messages (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    sender      TEXT        NOT NULL,   -- sender identity (UUID)
    sender_handle TEXT,                 -- human-readable, display only
    recipient   TEXT        NOT NULL,   -- recipient identity (UUID)
    sender_pub  TEXT        NOT NULL,   -- base64 ECDH public key for this message
    ciphertext  TEXT        NOT NULL,   -- base64 AES-GCM ciphertext
    iv          TEXT        NOT NULL,   -- base64 AES-GCM IV (12 bytes)
    status      TEXT        NOT NULL DEFAULT 'SENT',
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '7 days',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_dm_recipient ON direct_messages(recipient, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_dm_sender    ON direct_messages(sender, created_at DESC);

-- ── Signed prekeys ────────────────────────────────────────────────────────────
-- One SPK per identity (rotated every 1–4 weeks by client).
CREATE TABLE IF NOT EXISTS signed_prekeys (
    identity      TEXT        NOT NULL REFERENCES identities(identity) ON DELETE CASCADE,
    key_id        INT         NOT NULL,
    public_key_b64 TEXT       NOT NULL,
    signature_b64  TEXT       NOT NULL,   -- BLAKE3-keyed MAC proves authorship
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (identity, key_id)
);

-- ── One-time prekeys ──────────────────────────────────────────────────────────
-- Consumed on first use (deleted after claim). Client replenishes when count drops.
CREATE TABLE IF NOT EXISTS one_time_prekeys (
    id             BIGSERIAL   PRIMARY KEY,
    identity       TEXT        NOT NULL REFERENCES identities(identity) ON DELETE CASCADE,
    key_id         INT         NOT NULL,
    public_key_b64 TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(identity, key_id)
);
CREATE INDEX IF NOT EXISTS idx_otpk_identity ON one_time_prekeys(identity, id ASC);

-- ── Message queue ─────────────────────────────────────────────────────────────
-- Stores encrypted AMP frames until delivered or TTL expires.
-- The relay can read: recipient, sender, security_mode, msg_type, size, timestamps.
-- The relay CANNOT read: plaintext, chunk contents, token amounts, group names.
CREATE TABLE IF NOT EXISTS messages (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    recipient       TEXT        NOT NULL,        -- for routing; NEVER decrypted
    sender          TEXT        NOT NULL,
    security_mode   TEXT        NOT NULL DEFAULT 'SECURE',
    msg_type        TEXT        NOT NULL DEFAULT 'TEXT',
    -- Ratchet header (KeyCapsule) — public key only, no secrets
    dh_public_b64   TEXT        NOT NULL,
    msg_n           INT         NOT NULL DEFAULT 0,
    prev_n          INT         NOT NULL DEFAULT 0,
    -- X3DH fields for new sessions
    x3dh_ek_b64     TEXT,
    x3dh_spk_id     INT,
    x3dh_opk_id     INT,
    -- Chunked payload stored as JSONB array of { index, content_hash, ciphertext }
    chunks          JSONB       NOT NULL DEFAULT '[]',
    -- Delivery tracking
    status          TEXT        NOT NULL DEFAULT 'SENT',  -- SENT | DELIVERED | READ | EXPIRED
    -- TTL
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ,
    read_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_msg_recipient    ON messages(recipient, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_msg_sender       ON messages(sender, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_msg_status       ON messages(status, created_at);
CREATE INDEX IF NOT EXISTS idx_msg_expires      ON messages(expires_at);

-- ── Groups ────────────────────────────────────────────────────────────────────
-- Group metadata. Names and descriptions are stored encrypted as a JSONB blob.
CREATE TABLE IF NOT EXISTS groups (
    group_id        TEXT        PRIMARY KEY,  -- BASE58(BLAKE3(creator_identity || creation_nonce))
    member_count    INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Group membership mapping (identity → group).
CREATE TABLE IF NOT EXISTS group_members (
    group_id        TEXT        NOT NULL REFERENCES groups(group_id) ON DELETE CASCADE,
    identity        TEXT        NOT NULL,
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, identity)
);
CREATE INDEX IF NOT EXISTS idx_gm_identity ON group_members(identity);

-- ── Rate limiting counters ────────────────────────────────────────────────────
-- Simple sliding window counter. Cleared by TTL sweep.
CREATE TABLE IF NOT EXISTS rate_limits (
    identity        TEXT        NOT NULL,
    window_start    TIMESTAMPTZ NOT NULL,
    message_count   INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (identity, window_start)
);
CREATE INDEX IF NOT EXISTS idx_rl_window ON rate_limits(window_start);

-- ── Known contacts (first-contact PoW tracking) ───────────────────────────────
-- Records that sender:recipient have exchanged at least one message.
-- Senders in this table are exempt from proof-of-work.
CREATE TABLE IF NOT EXISTS known_contacts (
    sender      TEXT        NOT NULL,
    recipient   TEXT        NOT NULL,
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (sender, recipient)
);

-- ── Glyph identity signing keys ───────────────────────────────────────────────
-- Each PIAL can register an ECDSA-P256 signing key (Glyph).
-- The Glyph key signs all identity operations; relay verifies before acting.
ALTER TABLE identities ADD COLUMN IF NOT EXISTS glyph_public_key TEXT;
ALTER TABLE identities ADD COLUMN IF NOT EXISTS glyph_algorithm  TEXT DEFAULT 'ECDSA-P256';

-- ── Relay nodes ───────────────────────────────────────────────────────────────
-- Aethyr relay nodes that forward messages between identities.
-- Each node stakes AET and earns rewards for reliable delivery.
CREATE TABLE IF NOT EXISTS relay_nodes (
    node_id         TEXT        PRIMARY KEY,      -- Ed25519 public key hash
    endpoint        TEXT        NOT NULL,          -- ws://host:port or https://host:port
    glyph_pub_b64   TEXT        NOT NULL,          -- node's signing public key (ECDSA-P256)
    region          TEXT        NOT NULL DEFAULT 'global',
    status          TEXT        NOT NULL DEFAULT 'active',   -- active|suspended|offline
    registered_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_relay_status ON relay_nodes(status, last_seen_at DESC);

-- ── Karma scores ──────────────────────────────────────────────────────────────
-- Per-node karma: updated on every delivery event.
-- Karma determines routing priority (higher = preferred).
CREATE TABLE IF NOT EXISTS karma_scores (
    node_id         TEXT        PRIMARY KEY REFERENCES relay_nodes(node_id) ON DELETE CASCADE,
    score           FLOAT8      NOT NULL DEFAULT 50.0,
    deliveries_ok   BIGINT      NOT NULL DEFAULT 0,
    deliveries_fail BIGINT      NOT NULL DEFAULT 0,
    avg_latency_ms  FLOAT4      NOT NULL DEFAULT 100.0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Karma events ──────────────────────────────────────────────────────────────
-- Immutable append-only log of delivery events.
CREATE TABLE IF NOT EXISTS karma_events (
    id              BIGSERIAL   PRIMARY KEY,
    node_id         TEXT        NOT NULL REFERENCES relay_nodes(node_id) ON DELETE CASCADE,
    event_type      TEXT        NOT NULL,  -- delivery_ok | delivery_fail | latency_update
    latency_ms      INT,
    packet_id       TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_karma_events_node ON karma_events(node_id, created_at DESC);

-- ── Ratchet session registry ──────────────────────────────────────────────────
-- Stores encrypted ratchet state snapshots per (local_identity, remote_identity).
-- Contents are end-to-end encrypted; relay cannot read ratchet state.
CREATE TABLE IF NOT EXISTS ratchet_sessions (
    local_identity  TEXT        NOT NULL,
    remote_identity TEXT        NOT NULL,
    state_b64       TEXT        NOT NULL,   -- base64 encrypted RatchetStateSnapshot
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (local_identity, remote_identity)
);

-- ── Vovin v2: Device registry (multi-device) ──────────────────────────────────
-- Each PIAL maps to one or more devices, each with its own keypair.
-- Fixes: registering a second device no longer overwrites the first device's key.
-- Messages are sent to ALL active devices of a PIAL.
CREATE TABLE IF NOT EXISTS devices (
    device_id       TEXT        NOT NULL,
    pial_id         TEXT        NOT NULL,
    public_key_b64  TEXT        NOT NULL,   -- ECDH public key for this device
    glyph_pub_b64   TEXT,                   -- Glyph signing key (optional)
    handle          TEXT,
    device_name     TEXT,                   -- "iPhone", "MacBook", etc.
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    registered_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pial_id, device_id)
);
CREATE INDEX IF NOT EXISTS idx_devices_pial   ON devices(pial_id, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_devices_handle ON devices(handle) WHERE handle IS NOT NULL;

-- ── Aethyr File Fabric (AFF) ──────────────────────────────────────────────────
-- Files are encrypted on device, chunked, and stored as ciphertext blobs.
-- Messages carry only a file_id pointer — server never sees plaintext content.
CREATE TABLE IF NOT EXISTS file_manifests (
    file_id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    uploader_pial   TEXT        NOT NULL,
    filename        TEXT        NOT NULL,
    mime_type       TEXT        NOT NULL DEFAULT 'application/octet-stream',
    size_bytes      BIGINT      NOT NULL DEFAULT 0,
    chunk_count     INT         NOT NULL DEFAULT 0,
    chunks_received INT         NOT NULL DEFAULT 0,
    hash_root       TEXT        NOT NULL DEFAULT '',  -- SHA-256(all chunk hashes in order)
    encryption      TEXT        NOT NULL DEFAULT 'AES-256-GCM',
    status          TEXT        NOT NULL DEFAULT 'uploading'
                    CHECK (status IN ('uploading','complete','expired','failed')),
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '7 days',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_files_pial   ON file_manifests(uploader_pial, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_files_status ON file_manifests(status, expires_at);

CREATE TABLE IF NOT EXISTS file_chunks (
    chunk_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id         UUID        NOT NULL REFERENCES file_manifests(file_id) ON DELETE CASCADE,
    chunk_index     INT         NOT NULL,
    chunk_hash      TEXT        NOT NULL,   -- SHA-256 of the encrypted chunk bytes
    ciphertext_b64  TEXT        NOT NULL,   -- base64(AES-256-GCM encrypted chunk)
    nonce_b64       TEXT        NOT NULL,   -- base64(12-byte GCM nonce)
    size_bytes      INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(file_id, chunk_index)
);
CREATE INDEX IF NOT EXISTS idx_chunks_file ON file_chunks(file_id, chunk_index ASC);
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    // Additive migrations — safe to run repeatedly.
    sqlx::raw_sql(r#"
        ALTER TABLE direct_messages ADD COLUMN IF NOT EXISTS msg_version INT NOT NULL DEFAULT 1;
        ALTER TABLE direct_messages ADD COLUMN IF NOT EXISTS ratchet_pub TEXT;
        ALTER TABLE direct_messages ADD COLUMN IF NOT EXISTS recipient_device_id TEXT;
        CREATE INDEX IF NOT EXISTS idx_dm_device
            ON direct_messages(recipient, recipient_device_id)
            WHERE recipient_device_id IS NOT NULL;
    "#).execute(pool).await?;
    Ok(())
}

/// Hard-delete direct_messages past their TTL.
pub async fn expire_dms(pool: &PgPool) -> Result<u64> {
    let r = sqlx::query("DELETE FROM direct_messages WHERE expires_at < NOW()")
        .execute(pool)
        .await?;
    Ok(r.rows_affected())
}

/// Hard-delete messages past their TTL. Returns count deleted.
pub async fn expire_messages(pool: &PgPool) -> Result<u64> {
    let r = sqlx::query("DELETE FROM messages WHERE expires_at < NOW()")
        .execute(pool)
        .await?;
    Ok(r.rows_affected())
}

/// Clean rate limit records older than 1 hour.
pub async fn clean_rate_limits(pool: &PgPool) -> Result<()> {
    sqlx::query("DELETE FROM rate_limits WHERE window_start < NOW() - interval '1 hour'")
        .execute(pool)
        .await?;
    Ok(())
}

/// Count pending messages for an identity (used for relay capacity decisions).
#[allow(dead_code)]
pub async fn pending_count(pool: &PgPool, identity: &str) -> Result<i64> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM messages WHERE recipient = $1 AND status = 'SENT' AND expires_at > NOW()"
    )
    .bind(identity)
    .fetch_one(pool)
    .await?;
    Ok(row.0)
}
