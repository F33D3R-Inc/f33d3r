# VOVIN V4 — Architecture Specification

**Status:** Specification — implementation begins Sprint 2  
**Brain:** aethyr-msg (Vovin), port 8092  
**Author:** F33D3R Engineering  
**Date:** 2026-05-13

---

## Executive Summary

Vovin V4 is a complete upgrade of F33D3R's messaging brain. The cryptographic foundation, relay architecture, feature set, database schema, and client persistence layer all change. V3 sessions continue working throughout. No forced migration, no downtime.

**What changes:**

| Layer | V3 | V4 |
|-------|----|----|
| Session establishment | X3DH | PQXDH (X25519 + Kyber-768) |
| Ratchet | Double Ratchet | Triple Ratchet + SPQR |
| DH curve | ECDH-P256 | X25519 |
| Server metadata | Stores sender identity | Sealed Sender — server sees destination only |
| WebSocket | In-RAM registry (dies on restart) | Redis-backed, restart-safe |
| Offline delivery | Dropped silently | Redis offline queue, 30-day TTL |
| Multi-device | 5 devices registered, not fanned to | Fan-out to all active devices |
| Presence | None | Online/away/offline, per-conversation |
| Typing indicators | Basic | Fire-and-forget, auto-expire 10s |
| Delivery receipts | sent only | sent / delivered / read (3-state) |
| Message features | Text + voice note | Reactions, replies, edits, deletion, media, GIF, disappearing, search, forwarding |
| Group key agreement | Sender Keys (no redistribution on member change) | Sender Keys + rotation on member removal |

---

## Architecture Laws (Vovin-Specific)

These laws supplement the global F33D3R architecture laws and are Vovin-specific:

1. **Vovin owns exactly one database:** `f33d3r_msg`. It never reads `f33d3r_feed`.
2. **PIAL shard IDs, not PIAL UUIDs.** Vovin derives `shard_id = HKDF-SHA256(PIAL_UUID, "vovin-v4", salt)`. The raw PIAL UUID is never stored.
3. **Ciphertext only.** Vovin stores encrypted blobs. It cannot read message content. Message metadata stored is the minimum needed for routing and delivery.
4. **Sealed Sender.** Server does not store sender identity. It stores destination shard and ciphertext only.
5. **Media goes through Vovin, not Caeor.** Caeor handles feed media. Vovin handles message media. Vovin stores encrypted blobs in MinIO `vovin-media/` bucket. Caeor is never called for messages.
6. **Capability check before conversation creation.** Vovin calls Elohim Veni to verify `CapMessaging` before creating any conversation.
7. **Verity tier gates media size.** Before media uploads above 10 MB, Vovin checks Verity tier. Tier 1: 25 MB, Tier 2: 100 MB, Tier 3: 500 MB.
8. **Keys are generated client-side.** No key material ever transits the server. Vovin stores ciphertext and IVs only.

---

## System Topology (Vovin's view)

```
Browser (HTMX + WebCrypto)
    │
    │ WSS (messages) + HTTPS (API)
    │
    ▼
Caddy :443
    │
    ▼
Nantar :8081  ← sole edge brain, handles auth, proxies to Vovin
    │
    ├── X-Pial-Identity header injected
    ├── X-Vovin-Identity header injected (existing)
    │
    ▼
Vovin (aethyr-msg) :8092
    ├── PostgreSQL  f33d3r_msg     ← message store, prekeys, schema
    ├── Redis       vovin:*        ← presence, offline queue, typing
    └── MinIO       vovin-media/*  ← encrypted media blobs

Vovin → Elohim Veni :8093    (capability check — sync, blocks request)
Vovin → Verity :8095         (KYC tier check — sync, blocks media upload)
Vovin → Kafka                (async — conversation events, for AethyrRank)
```

---

## Part 1 — Cryptographic Upgrade Path: V3 → V4

### 1.1 Current State (V3)

Vovin V3 uses the AMP v1 protocol stack:

- **Session establishment:** X3DH (X25519 + HKDF-SHA256)
- **Ratchet:** Double Ratchet (X25519 DH + symmetric KDF chain)
- **Encryption:** ChaCha20-Poly1305 (AEAD) with 12-byte random nonce
- **Key derivation:** HKDF-SHA256 for root KDF, BLAKE3 for chain KDF
- **Identity signing:** ECDSA-P256 (Glyph key)
- **Identity hash:** BLAKE3(public_key_bytes) → base58

This is a good protocol. V4 does not replace it from weakness — it adds post-quantum resistance and metadata hiding.

### 1.2 V4 Upgrades

Four independent cryptographic upgrades ship in V4:

**Upgrade 1 — PQXDH replaces X3DH**  
Session establishment adds a Kyber-768 KEM alongside the X25519 DH. An adversary must break both to recover the session key. Protects against harvest-now-decrypt-later attacks.

**Upgrade 2 — Triple Ratchet + SPQR replaces Double Ratchet**  
Adds a third ratchet (Sparse Post-Quantum Ratchet) that fires every N=50 messages using Kyber-768. Provides post-quantum forward secrecy for the full session lifetime, not just session establishment.

**Upgrade 3 — X25519 replaces ECDH-P256 for DH operations**  
X25519 is faster, constant-time by design, and is the DH primitive used by Signal, WhatsApp, and iMessage. ECDSA-P256 Glyph signing is unchanged.

**Upgrade 4 — Sealed Sender (new)**  
The server no longer knows who sent a message. Sender identity is encrypted inside the message envelope. The server sees only: destination shard ID + ciphertext + timestamps.

### 1.3 Migration Strategy: Zero Downtime

The migration runs in four phases, each independently deployable:

**Phase 1 — Server accepts V4 prekey bundles (no client change)**  
- `prekey_bundles` table gains `kyber_prekey_pub`, `kyber_prekey_sig`, `protocol_version` columns
- Server returns `protocol_version` in bundle responses
- All existing V3 clients continue working unchanged

**Phase 2 — Clients generate V4 bundles for new vault setups**  
- New vault setup generates X25519 identity key + Kyber-768 prekey
- Uploads `protocol_version: 4` bundle
- V3 clients see V3 bundles from peers; V4 clients see V4 bundles
- Session initiation path chosen based on peer's `protocol_version`

**Phase 3 — Sealed Sender schema migration**  
- `messages` table: `sender_pial` column removed from new rows
- `sender_pial` still populated for rows inserted before Phase 3 (V3 rows)
- New rows: destination-only, Sealed Sender envelope format
- V3 clients are sent an upgrade prompt (not forced)

**Phase 4 — V3 deprecation**  
- After 90 days from Phase 3: V3 bundle upload deprecated
- V3 sessions continue until natural expiry (last message + 90 days)
- V3 session path removed from client in final cleanup

**Throughout all phases:** Existing sessions never forcibly renegotiated. No user-visible interruption.

### 1.4 SPQR Attack Window Justification

SPQR fires every N=50 messages. Analysis of what this means:

```
Attack scenario: Attacker breaks the classical X25519 DH ratchet.
Without SPQR: attacker recovers all past and future messages (until next DH ratchet step).
With SPQR at N=50: attacker recovers at most 50 messages before SPQR fires and forward
secrecy is restored via Kyber-768.

At typical F33D3R usage (10–30 msgs/session), N=50 means SPQR fires ~every 1–2 conversations.
Attack window: 50 messages × average message size (200 bytes plaintext) = 10 KB of data.

For a social platform at 10k users: this is an acceptable window.
For a war journalist or activist: N=10 is advisable (configurable per conversation in future).

CPU cost of Kyber-768 KEM on CCX23:
  Encapsulate: ~0.25 ms
  Decapsulate: ~0.22 ms
  At N=50: amortised 0.25/50 = 0.005 ms per message
  At N=10: amortised 0.025 ms per message — still imperceptible

Kyber-768 ciphertext size: ~1,088 bytes added to message envelope per SPQR fire.
At N=50: 1,088 bytes / 50 messages = 21.8 bytes average overhead per message.
Acceptable for a messaging protocol.
```

N=50 is the default. N is a session parameter, not a global constant — enabling future per-conversation configurability without a protocol change.

---

## Part 2 — Relay Architecture

### 2.1 Problem with V3 Relay

The current push registry is `Arc<Mutex<HashMap<identity, tokio::sync::broadcast::Sender>>>`.

- Dies on Vovin restart (all connections lost, no reconnect notification)
- Cannot fan out to multiple devices per user
- No offline queue — messages sent while user is offline are silently lost until they poll
- No presence layer

### 2.2 V4 Relay: Redis-Backed Fan-Out

```
Sender (Browser A)
    │
    ▼
Vovin receives send_message()
    ├── 1. Store encrypted message in PostgreSQL (async, background)
    ├── 2. PUBLISH to Redis channel: message:{recipient_shard_id}
    │
    ▼
Redis
    └── Nantar WS Gateway (subscriber on message:{recipient_shard_id})
            │
            ├── Recipient online → push via WebSocket
            └── Recipient offline → LPUSH vovin:offline:{shard_id}
```

**Key design decisions:**

- Store THEN publish. Delivery happens after persistence. If Redis fails, message is safe in PostgreSQL.
- Publish FIRST for delivery, persist async. This is the V4 order: deliver in < 50ms, persist in background. Client IDB is authoritative anyway.
- **Decision: publish first, persist in background.** Rationale: the client's IDB copy is the user's authoritative store. Server persistence is backup. P99 delivery under 50ms is more important than guaranteed ordering with PG write latency.

### 2.3 Connection Lifecycle

```
1. Client opens WSS to Caddy → Nantar /api/ws
2. Nantar authenticates session cookie → derives PIAL → derives shard_id
3. Nantar subscribes to Redis: message:{shard_id}, notify:{shard_id}
4. Nantar registers: HSET vovin:conn:{shard_id} ws_id connected_at  (TTL: 35s)
5. Nantar drains offline queue: LRANGE vovin:offline:{shard_id} 0 -1
   → push each item to WebSocket in order
   → DEL vovin:offline:{shard_id}
6. Live: Redis pub/sub events pushed to browser as they arrive
7. Client pings every 25s → Nantar refreshes Redis TTL
8. 30s no ping → connection stale → remove from registry
9. Client disconnects → Nantar unsubscribes Redis, removes presence key
```

### 2.4 Multi-Device Fan-Out

Up to 5 registered devices per user. Each device has its own WebSocket connection. All devices subscribe to the same Redis channel (`message:{shard_id}`). A single `PUBLISH` delivers to all connected devices simultaneously.

Device-specific behaviour: devices track their own last-read position in IDB. `read` receipts are per-device. A message read on one device is NOT automatically marked read on others (like iMessage's per-device read state).

### 2.5 Offline Queue

Redis List: `vovin:offline:{shard_id}`

```
Enqueue:  LPUSH vovin:offline:{shard_id} {event_json}
           LTRIM vovin:offline:{shard_id} 0 999        (cap at 1000)
           EXPIRE vovin:offline:{shard_id} 2592000       (30 days)

Drain:    LRANGE vovin:offline:{shard_id} 0 -1
           → deliver in order (oldest first: LRANGE returns newest-first, so RPOPLPUSH pattern)
           DEL vovin:offline:{shard_id}
```

Overflow handling: when queue exceeds 1000, oldest messages are dropped. On reconnect after overflow, a system message is injected: `"Some messages could not be delivered while you were offline."` The count of dropped messages is included.

---

## Part 3 — Feature Implementation Priority

Features ordered by impact/effort ratio. Implement in this order.

### Sprint 2 (infrastructure)

1. **Redis-backed WS registry** — eliminates restart-lost-connections problem. Unblocks everything else.
2. **Offline queue** — messages reliably delivered after reconnect. Most-visible user-facing improvement.
3. **Multi-device fan-out** — required for any user with 2+ browser sessions.
4. **Delivery receipts (3-state)** — sent/delivered/read. Low complexity, high user expectation.
5. **Presence** — online/away/offline per conversation. Redis TTL-based.

### Sprint 3 (crypto upgrade)

6. **PQXDH prekey bundles** — server-side schema first, then client V4 bundle upload
7. **Triple Ratchet + SPQR** — client-side ratchet upgrade, parallel to V3 path
8. **X25519 identity keys** — new vault setups only, V3 vaults unchanged
9. **Sealed Sender** — server schema migration (Phase 3 above)

### Sprint 4 (feature parity with iMessage/Signal)

10. **Message reactions** — wire type `reaction`, toggle logic, IDB aggregation
11. **Message replies** — `reply_to_id` reference, client quoted-preview rendering
12. **Message editing** — `edit` wire type, 15-minute server-side window enforcement
13. **Message deletion** — delete-for-me (IDB only) + delete-for-everyone (`delete` wire type, 60-min window)
14. **Disappearing messages** — per-conversation timer, server-side purge job, client-side IDB purge

### Sprint 5 (media + search)

15. **Media messages** — client-side AES-256-GCM encryption, MinIO upload via Vovin, thumbnail generation
16. **Voice notes** — MediaRecorder → Opus/WebM → client-side encrypt → Vovin AFF → waveform visualization
17. **GIF messages** — treated as media, platform GIF library in MinIO
18. **Message search** — client-side only, IDB inverted index, no server involvement
19. **Link previews** — Vovin fetches OG tags server-side (SSRF-protected), encrypted into message body
20. **Message forwarding** — re-encrypt to new conversation, `forwarded: true` flag

### Sprint 6 (group + safety)

21. **Group key rotation on member removal** — Sender Key redistribution
22. **Safety numbers** — HKDF derivation, QR + numeric display, key-change warnings
23. **Group conversations** — full group message flow with Sender Keys

---

## Part 4 — CCX23 Capacity Analysis

**Hardware:** Hetzner CCX23 — 4 vCPU AMD, 16 GB RAM, 160 GB NVMe, 400 Mbps network

### 4.1 WebSocket Connection Memory

```
Rust/Tokio WebSocket connection overhead:
  Tokio task stack:      8 KB (default, configurable down to 4 KB)
  WebSocket buffers:     8 KB (4 KB read + 4 KB write ring buffer)
  Redis subscription:    ~2 KB (channel name + subscriber state)
  Per-connection total:  ~18 KB

10,000 connections × 18 KB = 180 MB

Vovin process total:
  Connections:          180 MB
  Vovin heap/code:       60 MB
  PostgreSQL connection pool (16 connections × ~10 MB): 160 MB
  Redis connection pool:  10 MB
  Headroom:              90 MB
  Total:               ~500 MB

Docker Compose memory_limit: 2 GB (hard ceiling, 4× actual usage)
```

180 MB for 10k connections. This is not the bottleneck.

### 4.2 CPU Bottleneck Analysis

```
X25519 DH (per new session):  ~0.01 ms
Kyber-768 encap/decap:        ~0.25 ms
ChaCha20-Poly1305 (per msg):  ~0.003 ms/KB (hardware AES is faster but CCX23 has AESNI)

At 10k concurrent users, assume peak: 1,000 messages/second

Per-message CPU:
  Ratchet advance (HKDF):  ~0.05 ms
  AEAD decrypt (verify):   ~0.01 ms
  Total per message:       ~0.06 ms

1,000 msg/s × 0.06 ms = 60 ms total CPU per second = 6% of 1 core
PQXDH sessions (1% of messages are new sessions): 10/s × 0.25 ms = 2.5 ms/s
Triple Ratchet SPQR fires (1/50 messages): 20/s × 0.25 ms = 5 ms/s

Total crypto CPU at 1,000 msg/s: ~70 ms/s = 1.75% of 4 cores

Redis round-trips (presence lookup per message): 10k/s × 0.1 ms = 1s of CPU
This is the real bottleneck: Redis I/O, not crypto.
```

**CCX23 conclusion:** CCX23 handles 10k concurrent users with significant headroom. CPU is underutilized at messaging workloads. The binding constraint is:
1. Redis round-trip latency (solution: batch Redis calls where possible)
2. PostgreSQL write throughput for message persistence (solution: async writes)
3. Network throughput for media uploads (400 Mbps / 10k users = 40 Kbps per user — fine for text, tight for concurrent video uploads)

### 4.3 File Descriptor Limit

Each WebSocket connection = 1 file descriptor. Default Linux ulimit is 1,024. This must be raised:

```
/etc/security/limits.conf:
  * soft nofile 65536
  * hard nofile 65536

Docker Compose service:
  ulimits:
    nofile:
      soft: 65536
      hard: 65536
```

See `docs/LINUX_TUNING.md` for complete configuration.

### 4.4 CCX23 → AX52 Upgrade Trigger

**Upgrade to AX52 (8c/16t Ryzen 7 7700, 128 GB RAM) when ANY of:**

```
VovinConnectionsHigh:    active WS connections > 8,000 sustained 10 min
VovinMemoryHigh:         Vovin RSS > 1.5 GB sustained 30 min
VovinDeliveryLatencyHigh: p99 delivery latency > 500 ms sustained 5 min
VovinRedisRoundtripHigh: Redis p99 > 10 ms (indicates Redis needs separation)
```

At 10k users with the V4 architecture: CCX23 is sufficient. The upgrade trigger is 8k concurrent WebSocket connections — that's 80% of target before headroom runs out.

**Do not upgrade preemptively.** The Tokio async model makes 10k connections trivial in memory. The Kyber-768 SPQR overhead on CCX23 CPU is less than 2%. Upgrade when metrics say to, not when spec says so.

---

## Part 5 — Observability

Prometheus namespace: `vovin_`

### Metrics Catalog

```
vovin_connections_active                          — gauge, current WS connections
vovin_connections_total{result}                   — counter, accepted|rejected
vovin_messages_sent_total{type,conv_type}         — counter, text|media|reaction|..., dm|group
vovin_messages_delivered_total{method}            — counter, websocket|offline_queue
vovin_delivery_latency_ms                         — histogram, p50/p95/p99
vovin_offline_queue_depth{shard_bucket}           — gauge, bucketed by shard prefix
vovin_prekey_bundle_count{protocol_version}       — gauge, v3|v4 bundles in registry
vovin_prekey_one_time_remaining_low_total         — counter, fires when OTPKs < 10
vovin_media_upload_bytes_total                    — counter
vovin_typing_events_total                         — counter
vovin_pqxdh_sessions_total                        — counter, tracks V4 adoption
vovin_triple_ratchet_spqr_fires_total             — counter, SPQR step frequency
vovin_redis_roundtrip_ms                          — histogram, Redis operation latency
vovin_group_fanout_recipients{size_bucket}        — histogram, recipients per group message
```

### Alert Rules

```yaml
- alert: VovinConnectionsHigh
  expr: vovin_connections_active > 8000
  for: 10m
  annotations:
    summary: "Vovin WS connections >80% capacity — evaluate CCX23→AX52 upgrade"

- alert: VovinDeliveryLatencyHigh
  expr: histogram_quantile(0.99, vovin_delivery_latency_ms) > 500
  for: 5m
  annotations:
    summary: "Vovin p99 delivery latency >500ms"

- alert: VovinPreKeyDepleted
  expr: vovin_prekey_one_time_remaining_low_total > 0
  for: 0m
  annotations:
    summary: "User has <10 one-time prekeys — push replenishment request"

- alert: VovinOfflineQueueHigh
  expr: vovin_offline_queue_depth > 500
  for: 0m
  annotations:
    summary: "A user's offline queue >500 messages — investigate"

- alert: VovinRedisRoundtripHigh
  expr: histogram_quantile(0.99, vovin_redis_roundtrip_ms) > 10
  for: 5m
  annotations:
    summary: "Redis p99 >10ms — consider Redis instance separation"
```

---

## Part 6 — Security Model

### 6.1 Threat Model

| Threat | Mitigation |
|--------|-----------|
| Network eavesdropping | TLS at Caddy; E2E encryption inside TLS |
| Compromised server | Sealed Sender hides social graph; ciphertext only stored |
| Quantum adversary in future | PQXDH + Triple Ratchet SPQR |
| Compromised ratchet key | Forward secrecy (DR) + break-in recovery (DH ratchet) |
| Key substitution / MITM | Safety numbers + key-change warnings |
| SSRF via link preview | IP block list, 3s timeout, OG-only fetch |
| PIAL enumeration | PIAL shard IDs (HKDF-derived) never raw UUIDs |
| Media access without session | Client-side encrypted blobs in MinIO; key never on server |
| Mass message deletion | Server-side 60-min window; tombstones preserve ordering |

### 6.2 What the Server Knows (V4 with Sealed Sender)

After full V4 deployment:

```
Server knows: who has accounts (PIAL shard IDs)
Server knows: when conversations were created
Server knows: when messages were sent and to which conversation
Server knows: message sizes (approximate, from ciphertext length)
Server knows: media upload sizes

Server does NOT know: who sent any specific message (Sealed Sender)
Server does NOT know: message content
Server does NOT know: reaction content
Server does NOT know: media content
Server does NOT know: group membership (stored in DB but participants are shard IDs)
```

This is comparable to Signal's metadata leakage profile with sealed sender enabled.

---

## Related Documents

- `docs/VOVIN_V4_CRYPTO_SPEC.md` — PQXDH, Triple Ratchet, Sealed Sender, WebCrypto API implementation
- `docs/VOVIN_V4_DB_SCHEMA.sql` — Complete f33d3r_msg PostgreSQL schema
- `docs/VOVIN_V4_RELAY_SPEC.md` — WebSocket lifecycle, Redis key schema, fan-out algorithm
- `docs/VOVIN_V4_CLIENT_SPEC.md` — IDB schema, WebCrypto key management, offline sync, HTMX integration
- `docs/VOVIN_V4_FEATURES_SPEC.md` — Wire format and IDB state machine for all 15 features
- `docs/LINUX_TUNING.md` — sysctl, ulimit, Docker Compose resource limits for 10k connections

---

*Implementation begins Sprint 2. Review required before any code is written.*
