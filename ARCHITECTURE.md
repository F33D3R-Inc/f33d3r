# F33D3R — System Architecture

**Version:** 1.0  
**Date:** 2026-04-23  
**Classification:** Platform Engineering — Internal

---

## 1. System Overview

F33D3R is a distributed social operating system built as a set of independently deployable **brains**. No brain is a
monolith. No brain shares a database. No brain calls another brain synchronously in the hot path.

```
┌─────────────────────────────────────────────────────────────┐
│                        USERS                                 │
│              (web browser, mobile browser)                   │
└───────────────────────────┬─────────────────────────────────┘
                            │ HTTPS
┌───────────────────────────▼─────────────────────────────────┐
│                     API GATEWAY                              │
│              (Nginx + Envoy — global edge)                   │
│  Rate limit · TLS termination · Header injection · Routing   │
└───────────┬───────────────────────────┬─────────────────────┘
            │                           │
┌───────────▼───────┐       ┌───────────▼───────────────────┐
│   NANTAR (Feed)   │       │    VOVIN (Messaging)           │
│   Go 1.26.2       │       │    Rust 1.95.0                 │
│   Port 8081       │       │    Port 8092                   │
│   HTMX 2.0.10     │       │    ECDH-P256 + AES-256-GCM     │
└───────────────────┘       └────────────────────────────────┘
            │
    ┌───────┴───────────────────────────────┐
    │            EVENT BUS                  │
    │    (Kafka — all async data flow)      │
    └───────┬───────────┬───────────────────┘
            │           │
 ┌──────────▼──┐  ┌─────▼──────────┐  ┌──────────────────┐
 │ AETHYRRANK  │  │  ZODACARE      │  │  ELOHIM VENI     │
 │ Rust :8080  │  │  Rust :8090    │  │  Rust :8093      │
 │ Ranking     │  │  Safety        │  │  PIAL + Caps     │
 └─────────────┘  └────────────────┘  └──────────────────┘
 ┌─────────────┐  ┌────────────────┐
 │   ZIOR      │  │   AIN SOPH     │
 │ Rust :8082  │  │  Rust :8089    │
 │ Music       │  │  Wallet/AET    │
 └─────────────┘  └────────────────┘
            │
┌───────────▼───────────────────────────────────────────────┐
│              POSTGRESQL 16 (per-brain databases)           │
│  f33d3r_feed · f33d3r_msg · f33d3r_wallet                 │
│  f33d3r_security · f33d3r_safety · f33d3r_registry        │
└───────────────────────────────────────────────────────────┘
```

---

## 2. Core Architectural Rules

These rules are enforced at CI time by the enforcement layer. Violations block merge.

### 2.1 Brain independence

- Each brain owns exactly one database. No brain reads another brain's DB.
- All cross-brain communication flows through the event bus or approved API calls via the gateway.
- No brain may call another brain synchronously in the request hot path (except Nantar → approved reads with timeout).

### 2.2 PIAL is the identity spine

- No brain uses account UUIDs or handles as cross-brain identity.
- The PIAL UUID is the only cross-brain identity anchor.
- PIAL UUIDs are created by Nantar and are immutable.
- Elohim Veni is the authoritative source for capability state.

### 2.3 Safety is a hard constraint

- AethyrRank's safety barrier removes items — it never softly penalises them.
- Elohim Veni's capability revocations are synchronous and authoritative.
- Zodacare is advisory only; Elohim Veni enforces.
- No brain bypasses the safety layer by calling AethyrRank directly with inflated epsilon.

### 2.4 Psychology is inference, not enforcement

- Jung/archetype data (AESQ axes, vibe scores) lives only in AethyrRank's shadow model.
- This data never flows to Elohim Veni, Zodacare, or Ain Soph.
- No enforcement decision may reference psychological scores.

### 2.5 Events are append-only

- The PIAL event ledger is never updated or deleted.
- Kafka events are produced idempotently and consumed at-least-once.
- Consumer state is recoverable from the event log.

---

## 3. Data Flow

### 3.1 Post creation (full async path)

```
User submits post
    │
Nantar: validate PIAL capability (POSTING)
    │
Nantar: INSERT into posts table (f33d3r_feed)
    │
Nantar: emit event → Kafka topic: content.events
    │
    ├─► AethyrRank consumer: index content vector, update LinUCB
    ├─► Zodacare consumer: risk assessment on new post
    └─► Zior consumer: (if audio attached) begin audio analysis
    │
Nantar: HTTP 201 response to user (does not wait for consumers)
```

### 3.2 Feed assembly

```
User requests feed
    │
Nantar: build candidate pool from f33d3r_feed (latest posts)
    │
Nantar: POST /rank → AethyrRank (200ms timeout, fallback on error)
    │
AethyrRank: 7-stage pipeline → ranked list with scores
    │
Nantar: render HTMX partial with ranked posts
    │
Nantar: emit impression events → Kafka (async, non-blocking)
```

### 3.3 Messaging

```
User sends message
    │
Nantar: validate PIAL capability (MESSAGING)
    │
Nantar: proxy to Vovin with X-Vovin-Identity header
    │
Vovin: store ciphertext (never sees plaintext)
    │
Vovin: push notification via WebSocket to recipient
    │
Nantar: emit event → Kafka topic: messaging.events (metadata only)
```

### 3.4 Moderation action

```
User reports content
    │
Nantar: async POST → Zodacare
    │
Zodacare: score report, update risk profile
    │
IF risk > threshold:
    Zodacare: POST → Elohim Veni /v1/pial/:id/action
    │
Elohim Veni: update capability state (restrict/revoke)
    │
Elohim Veni: append to immutable event ledger
    │
Elohim Veni: emit event → Kafka topic: safety.events
    │
Nantar: (on next request) reads updated capability → blocks action
```

---

## 4. PIAL Architecture

PIAL (Persistent Identity + Access Layer) is the platform's Apple ID equivalent.

```
PIAL Root (UUID — immutable, generated by Nantar on signup)
    │
    ├── Account Bindings
    │     └── one active account per PIAL (handle, display_name, device_id)
    │
    ├── Capability Tree (owned by Elohim Veni)
    │     ├── POSTING         (granted | restricted | revoked | cooldown)
    │     ├── MESSAGING       (granted | restricted | revoked | cooldown)
    │     ├── MUSIC_UPLOAD    (granted | restricted | revoked | cooldown)
    │     ├── REALM_PROGRESSION
    │     ├── NSFW_ACCESS
    │     ├── MONETIZATION
    │     └── NEW_ACCOUNT_TRUST
    │
    ├── Trust Score (0–100, updated by Elohim Veni after decisions)
    │
    ├── Enforcement State
    │     └── none | warning | restricted | suspended | terminated
    │
    └── Event Ledger (append-only — decision_log in f33d3r_security)
          └── every capability change, every enforcement decision
```

**Lifecycle:**

1. User registers → Nantar creates PIAL UUID in `pial_roots` (f33d3r_feed)
2. Nantar calls Elohim Veni `/v1/pial/bootstrap` → security DB initialized
3. Default capabilities granted
4. On every write action: Nantar checks capability via `pial_capabilities` table
5. Zodacare reports flow to Elohim Veni → capability state updated
6. PIAL UUID used as Vovin identity, referenced in all audit logs
7. On tombstone: PIAL is permanently deactivated, handle recycled

---

## 5. Deployment Model

### 5.1 Local development

```bash
bash bootstrap-local.sh
```

Uses Docker Compose (`docker-compose.local.yml`). All services on localhost.

### 5.2 Production

- **Orchestration:** k3s (lightweight Kubernetes)
- **Deployment unit:** one Kubernetes Deployment per brain
- **Scaling:** Horizontal Pod Autoscaler per brain, independently scaled
- **Ingress:** Nginx Ingress Controller + Envoy sidecar for service mesh
- **Secrets:** Kubernetes Secrets (sealed in production with SealedSecrets)
- **DNS:** Cloudflare → Nginx ingress

### 5.3 Multi-region (future)

```
US-WEST (primary)   US-EAST (failover)   EU-WEST (GDPR)
      ↑                    ↑                    ↑
   k3s cluster         k3s cluster          k3s cluster
      ↑                    ↑                    ↑
   Cloudflare Global Load Balancer (geo-routing)
```

All Postgres clusters use streaming replication. Kafka uses cross-region replication for event topics. PIAL state is
authoritative in the primary region; read-replicas serve other regions.

---

## 6. Tech Stack — Locked Versions

| Component          | Technology      | Version       | Notes                     |
|--------------------|-----------------|---------------|---------------------------|
| Feed/UI brain      | Go              | 1.26.2        | No CGO                    |
| Feed/UI brain      | HTMX            | 2.0.10        | Via CDN                   |
| All Rust brains    | Rust            | 1.95.0        | Stable channel            |
| All Rust brains    | axum            | 0.7.x         | HTTP framework            |
| All Rust brains    | sqlx            | 0.8.x         | Async SQL                 |
| Database           | PostgreSQL      | 18            | One DB per brain          |
| Cache / queues     | Redis           | 7             | Sessions, rate limits     |
| Event bus          | Apache Kafka    | 3.7.x         | Production event backbone |
| Orchestration      | k3s             | latest stable | Kubernetes distribution   |
| Container registry | GitLab Registry | —             | Per-branch images         |
| CI/CD              | GitLab CI       | —             | See .gitlab-ci.yml        |

---

## 7. Enforcement Layer

See `enforcement/` directory.

The enforcement layer runs at three points:

1. **Pre-commit:** `enforcement/ci-gates/pre_merge_checks.sh` runs locally via git hooks
2. **CI pipeline:** `validate-architecture` stage blocks merge if dependency rules violated
3. **Runtime:** Service mesh policies (Envoy) block disallowed service-to-service calls

---

## 8. Event Bus (Kafka Topics)

| Topic              | Producer              | Consumers                  | Schema                           |
|--------------------|-----------------------|----------------------------|----------------------------------|
| `identity.events`  | Nantar, Elohim Veni   | All brains                 | `events/schemas/identity.*.json` |
| `content.events`   | Nantar                | AethyrRank, Zodacare, Zior | `events/schemas/post.*.json`     |
| `feed.events`      | AethyrRank            | Nantar                     | `events/schemas/feed.*.json`     |
| `ranking.events`   | AethyrRank            | Nantar, Zior               | `events/schemas/ranking.*.json`  |
| `messaging.events` | Vovin                 | Nantar (notifications)     | `events/schemas/message.*.json`  |
| `safety.events`    | Zodacare, Elohim Veni | Nantar, AethyrRank         | `events/schemas/safety.*.json`   |
| `economy.events`   | Ain Soph              | Nantar                     | `events/schemas/economy.*.json`  |

All events follow the canonical schema in `events/schemas/`. The schema registry (`aethyr-schema-registry`) enforces
backward compatibility.

---

## 9. Planned Brains

| Brain     | Purpose                               | Prerequisite          |
|-----------|---------------------------------------|-----------------------|
| Thessalon | Commerce, AET payments, subscriptions | Ain Soph stable       |
| Caeor     | Live streaming, CDN                   | k3s production        |
| Loxion    | Voice rooms, geo audio                | WebRTC infrastructure |
| Astraon   | Creator analytics dashboard           | Kafka event pipeline  |
