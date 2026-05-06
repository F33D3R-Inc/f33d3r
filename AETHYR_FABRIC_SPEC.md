# Aethyr Fabric Layer — Specification v1

**Classification:** Internal Engineering  
**Status:** Draft  
**Revision:** 2026-04-24

---

## 1. Architecture Overview

The Aethyr Fabric is the economic routing and event propagation substrate beneath all F33D3R brains. It does not provide
application logic. It provides:

- Deterministic AET settlement across any brain-to-brain interaction
- Routing incentive signals for Aethyr relay nodes
- Identity trust propagation using the PIAL anchor
- Ranked signal injection into AethyrRank without coupling
- Event ordering guarantees across async brain boundaries

```
┌──────────────────────────────────────────────────────────────────┐
│                        APPLICATION LAYER                         │
│   Feed (Nantar) · Messaging (Vovin) · Music (Zior) · Wallet UI   │
└───────────────────────────┬──────────────────────────────────────┘
                            │ submitEvent()
┌───────────────────────────▼──────────────────────────────────────┐
│                     AETHYR FABRIC LAYER                          │
│                                                                  │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────────────┐ │
│  │ Event Bus   │  │ AET Ledger   │  │ Routing Engine           │ │
│  │ (ordered,   │  │ (Ain Soph)   │  │ (Karma-weighted,         │ │
│  │  deduped)   │  │              │  │  latency-optimised)      │ │
│  └──────┬──────┘  └──────┬───────┘  └──────────────────────────┘ │
│         │                │                                       │
│  ┌──────▼──────┐  ┌──────▼───────┐  ┌──────────────────────────┐ │
│  │ Trust Layer │  │ Karma Engine │  │ Glyph Identity (PIAL)    │ │
│  │ (Elohim     │  │ (relay rep + │  │ Session binding,         │ │
│  │  Veni PDE)  │  │  incentive)  │  │ capability derivation    │ │
│  └─────────────┘  └──────────────┘  └──────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
                            │ async signals only
┌───────────────────────────▼──────────────────────────────────────┐
│                    INFRASTRUCTURE LAYER                          │
│   PostgreSQL · Redis · Aethyr Relay Nodes · PIAL Registry        │
└──────────────────────────────────────────────────────────────────┘
```

**Key constraint:** The Fabric never owns application state. It owns settlement, routing, and trust signals. Brains own
their own data.

---

## 2. Core Data Model

### 2.1 AET Transfer Object

```rust
struct AetTransfer {
    id: Uuid,          // globally unique, idempotency key
    from_pial: Uuid,          // sender PIAL root
    to_pial: Uuid,          // recipient PIAL root
    amount_units: i64,           // 1 AET = 100 units
    fee_units: i64,           // deducted before credit
    tx_type: TxType,        // Tip | Subscription | Purchase | Reward | Governance
    reference: Option<Uuid>,  // post_id, track_id, etc. — links payment to content
    nonce: u64,           // monotonic per sender PIAL, replay protection
    signed_at: DateTime<Utc>,
    signature: [u8; 64],      // Ed25519 over (from+to+amount+nonce)
}

enum TxType { Tip, Subscription, Purchase, Reward, NodeReward, GovernanceStake }
```

### 2.2 Routing Event (Aethyr Node hop)

```rust
struct RoutingEvent {
    id: Uuid,
    node_id: Uuid,          // Aethyr relay node identity
    packet_id: Uuid,          // the message/content packet being routed
    hop_index: u8,            // position in routing path
    latency_ms: u32,           // measured delivery latency for this hop
    delivered: bool,
    karma_delta: i32,           // +positive on success, -negative on failure
    created_at: DateTime<Utc>,
}
```

### 2.3 Ranking Signal Event

```rust
struct RankingSignal {
    signal_type: SignalType,
    actor_pial: Uuid,          // who performed the action
    target_pial: Option<Uuid>,  // creator being ranked (if applicable)
    content_id: Option<Uuid>,  // post/track being ranked
    weight: f64,           // 0.0–2.0, computed by tx_type + actor karma
    surface: String,        // "feed" | "music" | "messages"
    created_at: DateTime<Utc>,
}

enum SignalType {
    Tip,          // weight 0.8 — voluntary economic signal
    Subscription, // weight 2.0 — sustained economic commitment
    Like,         // weight 0.1 — engagement signal
    ViewComplete, // weight 0.05 — attention signal
    Reply,        // weight 0.15 — engagement depth signal
    Repost,       // weight 0.2 — distribution signal
    Report,       // weight -0.3 — negative signal (moderated by Zodacare first)
}
```

### 2.4 Identity Glyph Packet

```rust
struct GlyphPacket {
    pial_id: Uuid,
    session_id: Uuid,          // current device session
    device_id: String,        // stable per-device identifier
    capabilities: Vec<String>,   // from Elohim Veni capability grant
    trust_score: f32,           // 0.0–1.0, decays on anomaly
    realm: u8,            // 1–5, from AethyrRank XP
    signed_at: DateTime<Utc>,
    ttl_secs: u32,           // Glyph expires; must be re-validated
    signature: [u8; 64],      // Ed25519, signed by PIAL root key
}
```

### 2.5 Session Trust State

```rust
struct SessionTrustState {
    session_id: Uuid,
    pial_id: Uuid,
    trust_score: f32,            // current score (exponential decay on anomaly)
    anomaly_count: u32,            // consecutive anomalies
    last_anomaly_at: Option<DateTime<Utc>>,
    restricted: bool,           // rate-limited by Elohim Veni
    capabilities: Vec<String>,    // cached from last Glyph validation
    cached_until: DateTime<Utc>,  // re-validate after this timestamp
}
```

### 2.6 Karma Score (Relay Node)

```rust
struct KarmaScore {
    node_id: Uuid,
    score: f64,       // 0.0–100.0, starts at 50.0
    deliveries_ok: u64,
    deliveries_fail: u64,
    avg_latency_ms: f32,
    last_updated: DateTime<Utc>,
}
```

---

## 3. Event Flow System

### 3.1 Message Send → Settlement → Ranking

```
User sends message (Vovin)
  │
  ├─ [Fabric] submitEvent(MessageSent { sender_pial, recipient_pial, latency_ms })
  │     │
  │     ├─ Event Bus: deduplicate by event_id, order by (pial, nonce)
  │     ├─ Routing Engine: select relay path by Karma score + latency
  │     └─ Karma Engine: update node karma on delivery confirmation
  │
  └─ [async, non-blocking] → AethyrRank signal:
        RankingSignal { type: MessageSent, weight: 0.05, actor: sender_pial }
```

### 3.2 Creator Content → Tip → Payout

```
User tips creator (Ain Soph /v1/tip)
  │
  ├─ [Ain Soph] validate PIAL, check balance, atomic debit/credit
  ├─ [Fabric] submitEvent(AetTransfer { type: Tip, reference: post_id })
  │     │
  │     ├─ Settlement: confirmed in <1s (single-writer ledger, no consensus needed)
  │     └─ RankingSignal emitted async: { type: Tip, weight: 0.8, target: creator_pial }
  │
  └─ [AethyrRank] receives signal → updates creator content score
       → creator's posts surface higher in feed (not immediate, next ranking cycle)
```

### 3.3 Node Relay → Karma → Reward

```
Aethyr Node routes packet
  │
  ├─ On success: submitEvent(RoutingEvent { delivered: true, latency_ms: X })
  │     ├─ Karma: score += latency_bonus(X)  // faster = higher karma
  │     └─ Reward: micro AET credit to node wallet (batched every 60s)
  │
  └─ On failure: submitEvent(RoutingEvent { delivered: false })
        └─ Karma: score -= 5.0, node deprioritised in routing table for 300s
```

### 3.4 Ordering Rules

- Events within a single PIAL stream are ordered by `(pial_id, nonce)`. Nonce is monotonically increasing per PIAL,
  managed by Ain Soph.
- Events across PIALs have no global order guarantee. Consumers must be idempotent.
- Conflict resolution: last-write-wins for account state (balance, trust_score). Event ledger is append-only, never
  mutated.

---

## 4. Consensus / Trust Model (no blockchain)

### 4.1 Why No Blockchain

- Target: <1s settlement. Ethereum L1: 12–60s. Even L2s: 2–4s.
- F33D3R is a **permissioned internal ledger**, not a public trustless network.
- Single-writer model with audit log provides equivalent tamper-evidence for internal use.

### 4.2 Validation Without Consensus

```
Transaction submitted to Ain Soph:
  1. Verify Ed25519 signature (sender PIAL root key)
  2. Verify nonce > last_nonce for sender PIAL (replay protection)
  3. SELECT FOR UPDATE on sender account (serialisable lock)
  4. Check balance >= amount
  5. Debit sender, credit recipient (atomic DB transaction)
  6. Append to ledger_entries (immutable)
  7. Return confirmation in <10ms
```

No external consensus. Tamper-evidence via: append-only ledger + SHA-256 chain hash on each ledger_entry (each entry
hashes the previous entry's hash).

### 4.3 Glyph Identity (Sybil Resistance)

- Each PIAL is created once at account bootstrap (Elohim Veni + feed-engine)
- PIAL root key is generated on device, public key anchored in `pial_roots`
- A new Sybil account requires a new PIAL with no history → starts at Realm 1, trust score 0.5, no capabilities
- Economic cost of Sybil: no AET balance, no ranking weight, no relay node eligibility
- Elohim Veni applies velocity checks: >5 new PIALs from same IP/device in 24h → quarantine

### 4.4 Karma Routing Priority

```
routing_priority(node) = karma_score(node) * (1 / avg_latency_ms(node))

karma_score update:
  on_delivery_success: score = min(100, score + 1.0 + latency_bonus)
  on_delivery_failure: score = max(0, score - 5.0)
  latency_bonus: if latency < 50ms: +0.5, if < 100ms: +0.2, if > 500ms: -0.2
```

---

## 5. Token Economy Engine (AET)

### 5.1 Supply Model

- **No mint on demand.** Total supply increases only via platform reward events.
- **Emission sources:**
    1. Creator rewards — AET minted when content passes quality threshold (AethyrRank score > 0.7) and receives verified
       engagement
    2. Node rewards — AET minted for relay nodes that maintain karma > 60 and uptime > 95%
    3. Governance participation — small AET grant for voting on proposals (prevents apathy, bounded by 1 vote/proposal)
- **Burn mechanisms:**
    1. Network fees (2.5% deposit, 3.0% transfer, 1.0% tip) — credited to `ain_soph_fees` account
    2. Spam/report burn — Elohim Veni can confiscate AET from PIAL accounts flagged for abuse

### 5.2 Fee Schedule

```
tx_type       fee_bps   rationale
─────────────────────────────────────
Deposit       250       2.5% — onramp cost
Transfer      300       3.0% — general P2P
Tip           100       1.0% — low friction, encourage creator economy
Subscription  150       1.5% — recurring, slightly lower
Purchase      200       2.0% — content purchase
Withdrawal    200       2.0% — offramp cost
```

### 5.3 Routing Reward Formula

```
node_reward_per_delivery = base_reward * karma_multiplier * latency_multiplier

base_reward     = 0.01 AET (1 unit in storage)
karma_mult      = karma_score / 100       // 0.0–1.0
latency_mult    = 1 + max(0, (200 - latency_ms) / 200)  // 1.0–2.0

Rewards batched: every 60s, sum delivered to node PIAL wallet
```

### 5.4 Creator Payout Loop

```
Content published → AethyrRank scores content
If score > 0.7 AND engagements > threshold:
  creator_reward = base_rate * engagement_weight * realm_multiplier
  realm_multiplier: R1=1.0, R2=1.2, R3=1.5, R4=2.0, R5=3.0
  
Tips: pass through directly (net of 1% fee)
Subscriptions: monthly recurring via Ain Soph scheduled transfer
```

### 5.5 Ranking Boost Cost Model

*Reserved for V2.* Creators may stake AET to boost content visibility. Staked AET is locked for 24h. No refund if
content is reported and actioned. Formula TBD during Thessalon engine design.

---

## 6. Latency Optimization

### 6.1 Sub-Second Settlement

- Ain Soph uses `SELECT FOR UPDATE` + PostgreSQL serializable isolation. A well-tuned Postgres on local NVMe commits
  in <5ms.
- No network hop between feed-engine and Ain Soph in production (same datacenter, ideally same host)
- Redis: balance cached per PIAL, TTL 5s. 99% of balance reads hit cache. Write-through on every mutation.
- Nonce sequence managed in Redis (INCR is atomic, <1ms). DB nonce is updated async.

### 6.2 Routing Decisions

- Routing table in Redis: `aethyr:routing:{pial_id}` → sorted set of relay nodes by priority score
- Updated every 30s from Karma Engine
- `routePacket()` is a single Redis ZREVRANGE call (<1ms) + async UDP dispatch to relay node

### 6.3 Feed Ranking Without Bottleneck

- AethyrRank runs on Rust, separate process
- Fabric ranking signals are **fire-and-forget** from feed-engine (`sendBeacon`)
- AethyrRank writes scores back to `post_metrics.rank_score` (denormalized)
- Feed query: `ORDER BY rank_score DESC` — no runtime ranking, pure DB index scan

### 6.4 Batching vs Streaming

```
Batched (60s window):    node rewards, XP accumulation, trending tag updates
Streamed (real-time):    AET transfers, session trust changes, PIAL capability updates
Debounced (300ms):       ranking signal flush (collect events, send as batch to AethyrRank)
```

---

## 7. Fabric Layer API

### 7.1 submitEvent()

```rust
// Fire-and-forget. Never blocks the caller.
async fn submit_event(event: FabricEvent) -> EventId;

enum FabricEvent {
    AetTransfer(AetTransfer),
    RoutingEvent(RoutingEvent),
    RankingSignal(RankingSignal),
    TrustUpdate { pial_id: Uuid, delta: f32, reason: String },
    KarmaUpdate { node_id: Uuid, delta: f32, reason: String },
}
```

### 7.2 validateGlyph()

```rust
// Called by every brain on each authenticated request.
// Result cached in Redis TTL=60s per session_id.
async fn validate_glyph(packet: &GlyphPacket) -> GlyphResult;

enum GlyphResult {
    Valid { trust_score: f32, capabilities: Vec<String> },
    Expired,         // re-authenticate
    Revoked,         // PIAL suspended by Elohim Veni
    Forged,          // signature invalid → block + report
}
```

### 7.3 routePacket()

```rust
// Returns an ordered list of relay nodes for this packet.
// Caller dispatches; Fabric does not own the network connection.
async fn route_packet(
    packet_id: Uuid,
    sender_pial: Uuid,
    recipient_pial: Uuid,
    mode: SecurityMode,  // Fast | Secure | Paranoid
) -> Vec<RelayNode>;

struct RelayNode {
    node_id: Uuid,
    endpoint: SocketAddr,
    karma: f64,
    expected_latency_ms: u32,
}
```

### 7.4 computeKarma()

```rust
async fn compute_karma(node_id: Uuid) -> KarmaScore;
// Also: update_karma(node_id, event: RoutingEvent) — internal use only
```

### 7.5 settleAET()

```rust
// Thin wrapper over Ain Soph /v1/transfer.
// Fabric adds: nonce management, signature verification, ranking signal emit.
async fn settle_aet(transfer: AetTransfer) -> SettlementResult;

enum SettlementResult {
    Confirmed { ledger_entry_id: Uuid, settled_at: DateTime<Utc> },
    InsufficientBalance,
    InvalidSignature,
    ReplayDetected,
}
```

### 7.6 updateRankingSignal()

```rust
// Debounced — signals are buffered 300ms then flushed to AethyrRank in batch.
async fn update_ranking_signal(signal: RankingSignal);

// Internal flush (called by Fabric runtime every 300ms):
async fn flush_ranking_signals(signals: Vec<RankingSignal>) {
    // POST to AethyrRank /feedback in a single request
}
```

---

## 8. Failure Modes & Recovery

### 8.1 Node Failure

- Relay node goes offline: Fabric routing table marks node as `unavailable` after 3 missed heartbeats (15s)
- In-flight packets: sender retries with next-highest karma node within 5s
- Karma penalty: node loses 10 karma points per minute offline (floor: 0)

### 8.2 Partition Tolerance

- Ain Soph (ledger): PostgreSQL with WAL replication. Primary accepts writes. If primary fails: promote replica, accept
  up to 5s data loss window (RPO=5s). AET in-flight during failover: idempotency key prevents double-credit on retry.
- Redis (cache): eviction-safe. All Redis state is reconstructible from Postgres. Cache miss → DB read, no data loss.

### 8.3 Replay Protection

- Nonce: per-PIAL monotonic counter stored in Redis (primary) and Postgres (durability)
- On submit: `INCR pial:{id}:nonce` in Redis returns expected nonce. If submitted nonce ≤ stored → reject with
  `ReplayDetected`
- Idempotency key on all AET transfers: `INSERT ... ON CONFLICT (idempotency_key) DO NOTHING`

### 8.4 Double-Spend Prevention

```
1. Redis INCR nonce (atomic) — reject if nonce reused
2. PostgreSQL SELECT FOR UPDATE on sender account — serializable
3. Balance check inside lock — reject if insufficient
4. Debit + credit in same transaction — atomic commit
```

No two concurrent transfers from the same PIAL can both succeed with the same balance.

### 8.5 Degraded Mode

If Fabric event bus is unreachable:

- AET settlements still complete (Ain Soph is independent)
- Ranking signals are dropped (AethyrRank falls back to engagement-only scoring)
- Routing falls back to round-robin across all known nodes
- Trust validation falls back to last cached Glyph (TTL extended to 300s in degraded mode)

---

## 9. Security Model

### 9.1 Cryptographic Identity Flow

```
Device generates Ed25519 keypair
  └─ Public key → registered with Elohim Veni at PIAL bootstrap
       └─ pial_roots.pial_id = SHA-256(public_key)  ← canonical PIAL
            └─ All Glyph packets signed with private key
                 └─ Fabric validates signature on every API call
```

### 9.2 Session Signing

- Session tokens (user_sessions) are SHA-256 hashes of a random 32-byte secret
- Secret stored nowhere (only hash in DB). Token in cookie.
- Token → session lookup → PIAL ID → Glyph validation
- Sessions expire 30 days or on explicit revoke (logout, suspicious activity)

### 9.3 Relay Attestation

- Relay nodes register with a node keypair (separate from user PIAL)
- Registration requires: valid Ed25519 pubkey + minimum 100 AET stake (slashable)
- Each routing event is signed by the relay node's key → Fabric verifies → Karma update
- Forged routing events (invalid signature) → immediate node suspension + stake slash

### 9.4 Sybil Resistance Strategy

```
Cost of a Sybil identity:
  1. New device required (hardware fingerprint differs)
  2. PIAL bootstrap takes 1 full synchronous round-trip (not batchable)
  3. New PIAL starts at: Realm 1, trust=0.5, zero AET, no capabilities
  4. Capabilities require time + behavior (AethyrRank XP is not purchasable)
  5. Elohim Veni velocity check: >3 PIALs/day per IP → quarantine
  6. AET-weighted governance: new Sybil has no voting weight
```

### 9.5 Attack Surface Analysis

```
Attack                    │ Mitigation
──────────────────────────┼──────────────────────────────────────────────
AET double-spend          │ SELECT FOR UPDATE + nonce + idempotency key
Glyph forgery             │ Ed25519 signature, verified on every call
Session hijack            │ SHA-256 token (not guessable), HttpOnly cookie
Replay attack             │ Monotonic nonce per PIAL, Redis-enforced
Sybil governance attack   │ Token-weighted voting, velocity limits
Node karma gaming         │ Stake requirement, signature verification
Feed ranking manipulation │ AethyrRank is observational, not controllable
Ranking signal injection  │ Signals only accepted from authenticated PIALs
```

---

## 10. Implementation Phases

### V1 — Current (Stabilise)

- [x] Ain Soph double-entry ledger, tips, transfers, governance voting
- [x] PIAL bootstrap + Elohim Veni capability grants
- [x] Vovin ECDH-AES-GCM messaging with WebSocket delivery
- [ ] Ed25519 PIAL signing (currently unsigned — identity trust is DB-only)
- [ ] Nonce system for AET transfers (idempotency key implemented, nonce not)
- [ ] Karma engine stub (tables exist, no compute yet)

### V2 — Feature Parity

- [ ] Relay node registration + stake system
- [ ] Karma compute + routing table in Redis
- [ ] Node reward batching (60s flush)
- [ ] Double Ratchet wiring (ratchet.rs exists, not connected to client)
- [ ] Multi-device PIAL key bundle
- [ ] Creator subscription recurring payments

### V3 — Full Fabric

- [ ] Ed25519 Glyph packets signed on-device (WebCrypto)
- [ ] Post-quantum migration hook (Kyber hybrid, future)
- [ ] Onion routing for PARANOID mode
- [ ] Live media brain (WebRTC signaling via Vovin)
- [ ] Thessalon creator economy engine
