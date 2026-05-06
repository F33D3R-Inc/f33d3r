# F33D3R — Brain Map

**Canonical reference for all brain responsibilities, inputs, outputs, and forbidden actions.**

Every engineer working on F33D3R must read this before modifying any brain. Violations of brain boundaries are caught at
CI time by the enforcement layer.

---

## Brain Registry

| Brain           | Service dir            | Port | DB               | Language    | Status       |
|-----------------|------------------------|------|------------------|-------------|--------------|
| Nantar          | feed-engine            | 8081 | f33d3r_feed      | Go 1.26.2   | ✅ Production |
| AethyrRank      | aethyrrank-engine      | 8080 | f33d3r_feed      | Rust 1.95.0 | ✅ Production |
| Zior            | zior-engine            | 8082 | —                | Rust 1.95.0 | ✅ Production |
| Vovin           | aethyr-msg             | 8092 | f33d3r_msg       | Rust 1.95.0 | ✅ Production |
| Ain Soph        | ain-soph               | 8089 | f33d3r_wallet    | Rust 1.95.0 | ✅ Production |
| Elohim Veni     | elohim-veni            | 8093 | f33d3r_security  | Rust 1.95.0 | ✅ Production |
| Zodacare        | zodacare               | 8090 | f33d3r_safety    | Rust 1.95.0 | ✅ Production |
| Schema Registry | aethyr-schema-registry | 8079 | f33d3r_registry  | Rust 1.95.0 | ✅ Supporting |
| Thessalon       | thessalon              | 8084 | f33d3r_commerce  | Rust 1.95.0 | ✅ Production |
| Caeor           | caeor                  | 8086 | —                | Rust 1.95.0 | ✅ Production |
| Loxion          | —                      | —    | —                | TBD         | 🔲 Planned   |
| Astraon         | astraon                | 8088 | f33d3r_analytics | Rust 1.95.0 | 🔲 Planned (Sprint 4) |

---

## Nantar (Feed Engine)

**Role:** User-facing UI and social layer. The only brain that talks to browsers directly.

**Owns:**

- All HTML rendering (HTMX SSR)
- User session management
- Social data: posts, replies, follows, likes, bookmarks, notifications
- PIAL root creation (canonical UUID source)
- Media upload storage
- Vovin proxy (browser → Vovin relay)
- AethyrRank client (ranked feed assembly)

**Inputs:**

- HTTP requests from browsers (authenticated via cookie)
- Ranked items from AethyrRank (`POST /rank`)
- Track data from Zior (`GET /v1/tracks`)
- Balance from Ain Soph (`GET /balance/:user_id`)
- Kafka events: `safety.events` (capability updates), `ranking.events`

**Outputs:**

- HTML/HTMX responses to browsers
- Events to Kafka: `content.events`, `identity.events`
- Reports to Zodacare (async)
- PIAL bootstrap to Elohim Veni (sync, on user signup only)

**Forbidden:**

- ❌ Reading from any DB except f33d3r_feed
- ❌ Calling Zodacare synchronously in request path (must be async)
- ❌ Making moderation decisions (Zodacare/Elohim Veni own this)
- ❌ Storing plaintext messages (Vovin owns this)
- ❌ Generating PIAL UUIDs other than on signup bootstrap

---

## AethyrRank (Ranking Engine)

**Role:** Score and rank content for every surface. Observational only — no control authority.

**Owns:**

- 7-stage ranking pipeline (pre-rank → neural → AESQ → safety → velocity → revenue → re-rank)
- LinUCB exploration model (per surface, checkpointed to f33d3r_feed)
- Session feature cache
- Safety barrier (hard constraint, not soft weight)

**Inputs:**

- `POST /rank` from Nantar (content pool + user state)
- `POST /feedback` from Nantar (engagement events for LinUCB training)
- Kafka: `content.events` (index new content), `safety.events` (update safety epsilon)

**Outputs:**

- Ranked item list with scores and explanations to Nantar
- Kafka: `ranking.events` (score updates, model checkpoints)
- LinUCB checkpoint writes to f33d3r_feed (`linucb_checkpoints` table, shared with Nantar)

**Forbidden:**

- ❌ Making enforcement decisions (Elohim Veni owns this)
- ❌ Calling Nantar, Vovin, Ain Soph, or Zodacare
- ❌ Using Jung/archetype scores for enforcement (inference shadow model only)
- ❌ Modifying user profiles or post data
- ❌ Storing user-identifiable data beyond session feature cache (expires 1h)

---

## Zior (Music Cortex)

**Role:** Audio intelligence. Transform raw audio into signals that AethyrRank can rank.

**Owns:**

- Audio upload processing pipeline (decode → analyze → vectorize)
- 8-axis psychological mapping for audio (same space as AethyrRank's user vectors)
- Track metadata store (in-memory, backed by f33d3r_feed via Nantar)
- Behavioral engagement tracking per track (plays, skips, completions)
- Micro-community clustering (tracks with cosine similarity > 0.80)

**Inputs:**

- Audio files via Nantar upload proxy
- Behavioral events from Nantar (`POST /v1/feedback/play`, `/v1/feedback/like`)
- Kafka: `content.events` (track published events)

**Outputs:**

- Track signals to AethyrRank via Kafka: `ranking.events`
- Cluster assignments
- Trending track scores

**Forbidden:**

- ❌ Making ranking decisions (AethyrRank owns ranking)
- ❌ Storing user personal data
- ❌ Calling Elohim Veni or Zodacare
- ❌ Exposing psychological axis labels to users (internal only)

---

## Vovin (Messaging Brain)

**Role:** E2E encrypted message relay. Never sees plaintext.

**Owns:**

- PIAL identity registry (public key registration and lookup)
- Encrypted message storage (ciphertext + IV only)
- WebSocket push for real-time delivery
- Prekey management (X3DH V2 roadmap)
- Group chat structure (V2 roadmap)

**Inputs:**

- Proxied HTTP requests from Nantar (with `X-Vovin-Identity` PIAL header)
- Client-encrypted ciphertext (ECDH-AES-GCM)

**Outputs:**

- Ciphertext delivery to recipients
- WebSocket push notifications
- Kafka: `messaging.events` (metadata only — no content)

**Forbidden:**

- ❌ Decrypting messages (keys never leave client)
- ❌ Reading message content for any purpose
- ❌ Calling AethyrRank, Nantar, or Ain Soph
- ❌ Exposing PIAL-to-PIAL message graph to any other brain
- ❌ Accepting direct browser connections (must go through Nantar proxy)

---

## Ain Soph (Wallet Brain)

**Role:** AET token ledger. Every credit/debit is a double-entry pair.

**Owns:**

- Account balances (f33d3r_wallet)
- Double-entry ledger (every transaction = debit + credit entries)
- Fee collection (2.5% deposit fee → `ain_soph_fees` float account)
- Idempotency enforcement (duplicate requests return original result)

**Inputs:**

- Balance queries from Nantar
- Deposit/transfer/withdraw requests from Nantar (on behalf of authenticated users)
- (Future) Thessalon: external payment gateway events

**Outputs:**

- Balance responses
- Kafka: `economy.events` (transaction completed, milestone rewards)

**Forbidden:**

- ❌ Creating negative balances (validated at write time)
- ❌ Modifying or deleting ledger entries (append-only)
- ❌ Calling any other brain except Schema Registry
- ❌ Making capability decisions

---

## Elohim Veni (Security Brain)

**Role:** PIAL capability enforcement. Deterministic, auditable, psychology-free.

**Owns:**

- PIAL state (capability tree, trust scores, enforcement state)
- Immutable decision log
- Capability grant/revoke/cooldown operations
- Trust score maintenance (exponential decay model)

**Inputs:**

- Bootstrap from Nantar (on user signup — canonical UUID provided by Nantar)
- Capability check requests from Nantar (via f33d3r_feed `pial_capabilities` table — cached)
- Action requests from Zodacare (warn/restrict/ban)
- Kafka: `safety.events`

**Outputs:**

- Capability state responses
- Decision log entries (immutable)
- Kafka: `safety.events` (capability change notifications)

**Forbidden:**

- ❌ Generating its own PIAL UUIDs (Nantar is the canonical source)
- ❌ Using psychological scores (AESQ, Jung, archetype) in any decision
- ❌ Calling AethyrRank, Vovin, Zior, or Ain Soph
- ❌ Making content ranking decisions
- ❌ Deleting or modifying audit log entries

---

## Zodacare (Safety Brain)

**Role:** Content moderation and risk scoring. Advisory — enforcement is Elohim Veni's job.

**Owns:**

- Content report storage and queue (f33d3r_safety)
- User risk score calculation (0–100)
- Moderation workflow (pending → resolved/escalated)
- Elohim Veni action triggers (when risk score exceeds threshold)

**Inputs:**

- Report submissions from Nantar (async)
- Kafka: `content.events` (monitor new content), `identity.events`

**Outputs:**

- Risk score updates
- Action requests to Elohim Veni (warn/restrict/ban)
- Kafka: `safety.events` (risk score changes, actions issued)

**Forbidden:**

- ❌ Directly modifying user capabilities (Elohim Veni owns this)
- ❌ Reading message content from Vovin
- ❌ Making ranking decisions
- ❌ Tombstoning PIALs directly (must go through Elohim Veni)

---

## Schema Registry (Supporting)

**Role:** Schema versioning contract enforcement across all brains.

**Owns:**

- Versioned JSON schemas for all event types and API contracts
- Backward-compatibility validation rules
- Brain registration and heartbeat tracking

**Inputs:**

- Schema registration from all brains on startup
- Compatibility check requests from CI pipeline

**Outputs:**

- Schema validation results
- Latest schema versions for consumers

**Forbidden:**

- ❌ Making routing decisions
- ❌ Storing business data
- ❌ Calling any brain except for health checks

---

## Dependency Matrix

Rows = caller. Columns = callee. ✅ = allowed. ❌ = forbidden.

|                 | Nantar | AethyrRank | Zior   | Vovin   | Ain Soph | Elohim Veni | Zodacare | Schema Reg |
|-----------------|--------|------------|--------|---------|----------|-------------|----------|------------|
| **Nantar**      | —      | ✅ read     | ✅ read | ✅ proxy | ✅ read   | ✅ bootstrap | ✅ async  | ✅          |
| **AethyrRank**  | ❌      | —          | ❌      | ❌       | ❌        | ❌           | ❌        | ✅          |
| **Zior**        | ❌      | ❌          | —      | ❌       | ❌        | ❌           | ❌        | ✅          |
| **Vovin**       | ❌      | ❌          | ❌      | —       | ❌        | ❌           | ❌        | ✅          |
| **Ain Soph**    | ❌      | ❌          | ❌      | ❌       | —        | ❌           | ❌        | ✅          |
| **Elohim Veni** | ❌      | ❌          | ❌      | ❌       | ❌        | —           | ❌        | ✅          |
| **Zodacare**    | ❌      | ❌          | ❌      | ❌       | ❌        | ✅ actions   | —        | ✅          |

All brains may consume events from Kafka. This matrix covers **direct HTTP calls only**.

---

## Adding a New Brain

1. Create `brains/<name>/` directory following the template structure
2. Add entry to `BRAIN_MAP.md` and `enforcement/dependency-graph/rules.yaml`
3. Create database and add to `postgres-init/01-create-databases.sql`
4. Add Dockerfile using `rust:1.95.0-slim-bookworm` or `golang:1.26.2-alpine`
5. Add service to `docker-compose.local.yml` and `infra/k3s/manifests/<name>.yaml`
6. Run `bash scripts/new_brain.sh <name>` to scaffold structure
7. Register event schemas with Schema Registry on startup
8. Update `README.md` and `README-LOCAL.md` brain tables

See `docs/brain-registry.md` for full walkthrough.
