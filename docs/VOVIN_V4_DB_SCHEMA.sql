-- ═══════════════════════════════════════════════════════════════════════════════
-- VOVIN V4 — f33d3r_msg Schema
-- Spec: docs/VOVIN_V4_DB_SCHEMA.sql
-- Status: Not yet applied — specification only
-- ═══════════════════════════════════════════════════════════════════════════════

-- Run on: f33d3r_msg database (NOT f33d3r_feed)
-- Migration strategy: non-destructive — existing V3 tables renamed, new tables created

-- ── Extensions ────────────────────────────────────────────────────────────────

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";  -- for trigram search on handles

-- ── CONVERSATIONS ─────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS conversations (
  conversation_id    UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
  conversation_type  VARCHAR(10)  NOT NULL CHECK (conversation_type IN ('dm','group')),
  created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
  disappearing_timer VARCHAR(10)  NOT NULL DEFAULT 'off'
                     CHECK (disappearing_timer IN ('off','1h','24h','7d','30d')),
  last_message_at    TIMESTAMPTZ,
  -- Group-only fields
  group_name         VARCHAR(128),
  group_avatar_path  VARCHAR(512)
);

CREATE INDEX idx_conversations_last_message_at ON conversations (last_message_at DESC NULLS LAST);

-- ── CONVERSATION_PARTICIPANTS ─────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS conversation_participants (
  conversation_id    UUID        NOT NULL REFERENCES conversations(conversation_id) ON DELETE CASCADE,
  pial_shard_id      VARCHAR(64) NOT NULL,
  joined_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_read_msg_id   UUID,           -- nullable — no messages read yet
  notifications_muted BOOLEAN   NOT NULL DEFAULT FALSE,
  muted_until        TIMESTAMPTZ,    -- nullable — permanent mute if muted=true and this is null
  pinned_at          TIMESTAMPTZ,    -- nullable — set when conversation is pinned
  PRIMARY KEY (conversation_id, pial_shard_id)
);

CREATE INDEX idx_cp_pial ON conversation_participants (pial_shard_id, pinned_at DESC NULLS LAST, conversation_id);

-- ── MESSAGES ──────────────────────────────────────────────────────────────────
-- Partitioned by conversation_id hash for 10k concurrent users.
-- Sealed Sender: sender_shard_id is NOT stored — zero metadata leakage.

CREATE TABLE IF NOT EXISTS messages (
  message_id         UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  conversation_id    UUID        NOT NULL REFERENCES conversations(conversation_id) ON DELETE CASCADE,
  -- NO sender_shard_id (Sealed Sender — server never knows who sent)
  message_type       VARCHAR(20) NOT NULL CHECK (message_type IN (
                       'text','reaction','reply','edit','delete',
                       'pin','unpin','voice_note','media','forward',
                       'system','typing_start','typing_stop'
                     )),
  ciphertext         BYTEA       NOT NULL,
  iv                 BYTEA       NOT NULL,
  server_timestamp   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  reply_to_id        UUID        REFERENCES messages(message_id) ON DELETE SET NULL,
  is_tombstone       BOOLEAN     NOT NULL DEFAULT FALSE, -- soft delete
  expires_at         TIMESTAMPTZ,                        -- disappearing messages
  -- Media attachment (if any)
  media_storage_path VARCHAR(512),
  media_enc_size     BIGINT,
  media_type         VARCHAR(64)
) PARTITION BY HASH (conversation_id);

-- 8 hash partitions — sufficient for 10k users, expand at 100k
CREATE TABLE messages_p0 PARTITION OF messages FOR VALUES WITH (MODULUS 8, REMAINDER 0);
CREATE TABLE messages_p1 PARTITION OF messages FOR VALUES WITH (MODULUS 8, REMAINDER 1);
CREATE TABLE messages_p2 PARTITION OF messages FOR VALUES WITH (MODULUS 8, REMAINDER 2);
CREATE TABLE messages_p3 PARTITION OF messages FOR VALUES WITH (MODULUS 8, REMAINDER 3);
CREATE TABLE messages_p4 PARTITION OF messages FOR VALUES WITH (MODULUS 8, REMAINDER 4);
CREATE TABLE messages_p5 PARTITION OF messages FOR VALUES WITH (MODULUS 8, REMAINDER 5);
CREATE TABLE messages_p6 PARTITION OF messages FOR VALUES WITH (MODULUS 8, REMAINDER 6);
CREATE TABLE messages_p7 PARTITION OF messages FOR VALUES WITH (MODULUS 8, REMAINDER 7);

-- Indexes on the parent table (propagate to partitions automatically in PG 11+)
CREATE INDEX idx_messages_conv_ts ON messages (conversation_id, server_timestamp DESC);
CREATE INDEX idx_messages_expires  ON messages (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX idx_messages_reply    ON messages (reply_to_id) WHERE reply_to_id IS NOT NULL;

-- ── PREKEY_BUNDLES ────────────────────────────────────────────────────────────
-- Per-device prekey bundles. Each device has its own bundle.
-- V4 adds Kyber-768 fields.

CREATE TABLE IF NOT EXISTS prekey_bundles (
  bundle_id              UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  pial_shard_id          VARCHAR(64) NOT NULL,
  device_id              VARCHAR(64) NOT NULL,
  -- DH identity key (X25519 in V4, ECDH-P256 in V3 — stored as raw public key bytes)
  identity_key_pub       BYTEA       NOT NULL,
  -- Signed prekey (X25519 in V4)
  signed_prekey_pub      BYTEA       NOT NULL,
  signed_prekey_sig      BYTEA       NOT NULL,  -- ECDSA-P256 sig over signed_prekey_pub
  signed_prekey_id       INT         NOT NULL DEFAULT 0,
  -- Post-quantum prekey (Kyber-768, V4 only — NULL for V3 bundles)
  kyber_prekey_pub       BYTEA,
  kyber_prekey_sig       BYTEA,
  kyber_prekey_id        INT,
  -- One-time prekeys (X25519, stored as array of base64 encoded keys)
  -- Consumed one per session initiation, deleted after use
  otpk_count             INT         NOT NULL DEFAULT 0,
  -- Protocol version: 3 = V3 (X3DH + DR), 4 = V4 (PQXDH + TR + SPQR)
  protocol_version       SMALLINT    NOT NULL DEFAULT 4,
  -- Glyph signing public key (ECDSA-P256 SPKI, nullable for devices that skip it)
  glyph_pub_spki         BYTEA,
  created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_used_at           TIMESTAMPTZ,
  UNIQUE (pial_shard_id, device_id)
);

CREATE INDEX idx_pb_pial ON prekey_bundles (pial_shard_id, last_used_at DESC);

-- One-time prekeys stored separately (consumed individually)
CREATE TABLE IF NOT EXISTS one_time_prekeys (
  otpk_id       UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  pial_shard_id VARCHAR(64) NOT NULL,
  device_id     VARCHAR(64) NOT NULL,
  key_id        INT         NOT NULL,
  pub_key       BYTEA       NOT NULL,  -- X25519 public key
  used          BOOLEAN     NOT NULL DEFAULT FALSE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_otpk_pial_unused ON one_time_prekeys (pial_shard_id, device_id) WHERE NOT used;

-- ── BACKUP_ARCHIVES ───────────────────────────────────────────────────────────
-- Zero-knowledge: no pial_shard_id stored.
-- backup_id = HKDF(recovery_key, "F33DR_BACKUP_ID_V4") — only identifier.

CREATE TABLE IF NOT EXISTS backup_archives (
  backup_id        VARCHAR(64) PRIMARY KEY,  -- hex(HKDF(key, "backup-id"))
  blob_path        VARCHAR(512) NOT NULL,    -- MinIO: vovin-backups/{backup_id}.enc
  size_bytes       BIGINT       NOT NULL,
  media_cutoff_at  TIMESTAMPTZ,              -- 45 days ago at backup time
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
  -- No pial_shard_id — zero-knowledge by design
);

-- ── IDENTITY_REGISTRY ─────────────────────────────────────────────────────────
-- Maps PIAL → current active prekey bundle (for lookups during session init).

CREATE TABLE IF NOT EXISTS identity_registry (
  pial_shard_id        VARCHAR(64)  PRIMARY KEY,
  handle               VARCHAR(64)  NOT NULL,
  active_bundle_id     UUID         REFERENCES prekey_bundles(bundle_id),
  registered_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
  last_seen_at         TIMESTAMPTZ
);

CREATE INDEX idx_identity_handle ON identity_registry USING gin (handle gin_trgm_ops);

-- ── DM_DELIVERY_QUEUE ─────────────────────────────────────────────────────────
-- Offline message queue. Drained on reconnect.
-- Max 1000 per user — oldest dropped if exceeded.

CREATE TABLE IF NOT EXISTS dm_delivery_queue (
  queue_id          BIGSERIAL    PRIMARY KEY,
  recipient_pial    VARCHAR(64)  NOT NULL,
  message_id        UUID         NOT NULL,
  conversation_id   UUID         NOT NULL,
  enqueued_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
  expires_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW() + INTERVAL '30 days'
);

CREATE INDEX idx_queue_recipient ON dm_delivery_queue (recipient_pial, queue_id ASC);

-- Auto-cleanup: pg cron job (or application-level) deletes expired entries
CREATE INDEX idx_queue_expires ON dm_delivery_queue (expires_at);

-- ── TYPING_EVENTS (ephemeral — not persisted) ─────────────────────────────────
-- Typing indicators are fire-and-forget via WebSocket. Not stored in DB.
-- This table is a placeholder comment only.

-- ── READ_RECEIPTS ─────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS read_receipts (
  message_id     UUID        NOT NULL REFERENCES messages(message_id) ON DELETE CASCADE,
  reader_pial    VARCHAR(64) NOT NULL,
  read_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (message_id, reader_pial)
);

-- ── REACTIONS ─────────────────────────────────────────────────────────────────
-- Reactions to messages. Stored as ciphertext (E2E encrypted).

CREATE TABLE IF NOT EXISTS message_reactions (
  reaction_id    UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  message_id     UUID        NOT NULL REFERENCES messages(message_id) ON DELETE CASCADE,
  reactor_pial   VARCHAR(64) NOT NULL,
  emoji          VARCHAR(8)  NOT NULL,  -- emoji character (e.g. "❤️")
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (message_id, reactor_pial, emoji)  -- one of each emoji per user per message
);

CREATE INDEX idx_reactions_msg ON message_reactions (message_id);

-- ── FILE_MANIFESTS (AFF — Aethyr File Format) ─────────────────────────────────

CREATE TABLE IF NOT EXISTS file_manifests (
  file_id          UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  uploader_pial    VARCHAR(64) NOT NULL,
  filename         VARCHAR(255),
  mime_type        VARCHAR(128),
  size_bytes       BIGINT,
  chunk_count      INT         NOT NULL DEFAULT 1,
  chunks_received  INT         NOT NULL DEFAULT 0,
  hash_root        VARCHAR(256),
  status           VARCHAR(20) NOT NULL DEFAULT 'uploading'
                   CHECK (status IN ('uploading','complete','expired')),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at       TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '90 days'
);

CREATE INDEX idx_fm_expires ON file_manifests (expires_at) WHERE status != 'expired';

CREATE TABLE IF NOT EXISTS file_chunks (
  file_id         UUID  NOT NULL REFERENCES file_manifests(file_id) ON DELETE CASCADE,
  chunk_index     INT   NOT NULL,
  chunk_hash      VARCHAR(256),
  ciphertext_b64  TEXT  NOT NULL,
  nonce_b64       VARCHAR(64) NOT NULL,
  size_bytes      INT,
  PRIMARY KEY (file_id, chunk_index)
);

-- ── VOICE_FILES ────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS voice_files (
  voice_id    UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  pial_id     VARCHAR(64) NOT NULL,
  file_path   VARCHAR(512) NOT NULL,  -- filesystem path inside container
  size_bytes  INT,
  mime_type   VARCHAR(64)  NOT NULL DEFAULT 'audio/webm',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at  TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '90 days'
);

CREATE INDEX idx_voice_expires ON voice_files (expires_at);

-- ── RELAY_NODES ────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS relay_nodes (
  node_id         UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
  endpoint        VARCHAR(256) NOT NULL UNIQUE,
  region          VARCHAR(64),
  last_heartbeat  TIMESTAMPTZ,
  karma_score     INT         NOT NULL DEFAULT 0
);

-- ── RATCHET_STATE ──────────────────────────────────────────────────────────────
-- Encrypted ratchet state backup. Client-side is authoritative;
-- this is a cloud fallback for multi-device sync.
-- State is encrypted client-side before upload.

CREATE TABLE IF NOT EXISTS ratchet_state (
  local_identity   VARCHAR(64) NOT NULL,
  remote_identity  VARCHAR(64) NOT NULL,
  state_b64        TEXT        NOT NULL,  -- AES-256-GCM encrypted JSON, base64
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (local_identity, remote_identity)
);

-- ══════════════════════════════════════════════════════════════════════════════
-- MIGRATION FROM V3 SCHEMA
-- Non-destructive — V3 tables renamed with _v3 suffix, kept for 90 days
-- ══════════════════════════════════════════════════════════════════════════════

-- Step 1: Rename existing V3 tables (run in transaction)
-- ALTER TABLE messages         RENAME TO messages_v3;
-- ALTER TABLE identities       RENAME TO identities_v3;
-- ALTER TABLE devices          RENAME TO devices_v3;
-- ALTER TABLE direct_messages  RENAME TO direct_messages_v3;  (if exists)

-- Step 2: Create V4 tables (this file)

-- Step 3: Migrate data
-- INSERT INTO identity_registry SELECT pial_id, handle, NULL, registered_at, last_seen_at FROM identities_v3;
-- INSERT INTO prekey_bundles (pial_shard_id, device_id, identity_key_pub, ...)
--   SELECT pial_id, device_id, decode(public_key_b64, 'base64'), ... FROM devices_v3;

-- Step 4: After 90-day validation window, drop _v3 tables

-- ══════════════════════════════════════════════════════════════════════════════
-- CLEANUP JOBS (application-level or pg_cron)
-- ══════════════════════════════════════════════════════════════════════════════

-- Disappearing messages: run every 5 minutes
-- DELETE FROM messages WHERE expires_at < NOW() AND NOT is_tombstone;
-- UPDATE messages SET is_tombstone = TRUE, ciphertext = ''::BYTEA WHERE expires_at < NOW();
-- (Tombstoning preserves message ordering integrity)

-- AFF cleanup: run daily
-- UPDATE file_manifests SET status = 'expired' WHERE expires_at < NOW();
-- DELETE FROM file_chunks WHERE file_id IN (SELECT file_id FROM file_manifests WHERE status = 'expired');

-- Offline queue overflow: run on enqueue
-- DELETE FROM dm_delivery_queue WHERE queue_id IN (
--   SELECT queue_id FROM dm_delivery_queue
--   WHERE recipient_pial = $1
--   ORDER BY queue_id ASC
--   LIMIT (SELECT GREATEST(0, COUNT(*) - 1000) FROM dm_delivery_queue WHERE recipient_pial = $1)
-- );

-- Voice file cleanup: run daily
-- DELETE FROM voice_files WHERE expires_at < NOW();
-- (Also delete from filesystem/MinIO)
