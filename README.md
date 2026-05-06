# F33D3R

**Distributed social operating system for creators, communities, and the creator economy.**

F33D3R is a multi-brain platform built on isolated service architecture, a cryptographic persistent identity layer (PIAL), Signal-grade end-to-end encrypted messaging, a closed-loop economic fabric (ETHRA/AET), and a native commerce engine. It is engineered from first principles for creator monetization at scale — with a platform fee of 2.5% versus the industry standard 20%.

---

## Platform Overview

| Capability | Implementation |
|---|---|
| Social feed + ranking | Nantar (Go) + AethyrRank (Rust) — Jungian behavioral ranking, LinUCB exploration |
| End-to-end messaging | Vovin (Rust) — ECDH-P256, Double Ratchet, AES-256-GCM, keys never leave client |
| Creator commerce | Thessalon (Rust) — subscriptions, pay-per-view, tips; 2.5% fee |
| Media processing | Caeor (Rust) — WebP conversion, multi-size derivatives, shared volume serving |
| Wallet + settlement | Ain Soph (Rust) + Aethyr Ledger (Rust) — double-entry AET ledger |
| Identity + capabilities | PIAL UUID spine + Elohim Veni enforcement |
| KYC + compliance | Verity (Rust) — 4-tier attestation, privacy-preserving decision hashes |
| Content safety | Zodacare (Rust) — risk scoring, moderation queue, Elohim Veni triggers |
| Music intelligence | Zior (Rust) — audio analysis, 8-axis vectorization, cluster ranking |
| Handle registry | Registrar (Rust) — handle ownership, transfers |
| Schema contracts | Schema Registry (Rust) — versioned event schemas, CI contract enforcement |

---

## Service Registry

| Brain | Service Dir | Port | Database | Language | Status |
|---|---|---|---|---|---|
| Nantar | `feed-engine` | 8081 | f33d3r_feed | Go 1.26.2 | Production |
| AethyrRank | `aethyrrank-engine` | 8080 | f33d3r_feed | Rust 1.95.0 | Production |
| Zior | `zior-engine` | 8082 | — | Rust 1.95.0 | Production |
| Vovin | `aethyr-msg` | 8092 | f33d3r_msg | Rust 1.95.0 | Production |
| Ain Soph | `ain-soph` | 8089 | f33d3r_wallet | Rust 1.95.0 | Production |
| Elohim Veni | `elohim-veni` | 8093 | f33d3r_security | Rust 1.95.0 | Production |
| Zodacare | `zodacare` | 8090 | f33d3r_safety | Rust 1.95.0 | Production |
| Schema Registry | `aethyr-schema-registry` | 8079 | f33d3r_registry | Rust 1.95.0 | Production |
| Registrar | `registry-brain` | 8094 | f33d3r_handles | Rust 1.95.0 | Production |
| Verity | `verity` | 8095 | f33d3r_verity | Rust 1.95.0 | Production |
| Aethyr Ledger | `aethyr-ledger` | 8096 | f33d3r_ledger | Rust 1.95.0 | Production |
| Thessalon | `thessalon` | 8084 | f33d3r_commerce | Rust 1.95.0 | Production |
| Caeor | `caeor` | 8086 | — | Rust 1.95.0 | Production |
| Loxion | — | 8087 | — | TBD | Planned |
| Astraon | — | — | f33d3r_analytics | TBD | Planned |

Supporting infrastructure: PostgreSQL 16 (port 5432) · Redis 7 (port 6379)

---

## Architecture

### Topology

```
Browser (HTMX 2.0.10)
         │
         ▼
  ┌─────────────────────────────────────────────────────┐
  │            Nantar  :8081  (Go 1.26.2)               │
  │   The only brain that serves HTML to browsers.       │
  │   All other brains are internal.                     │
  └─────┬──────────┬──────┬───────┬───────┬─────────────┘
        │          │      │       │       │
     /vovin/  /ainsoph/ /verity/ /ledger/ /thessalon/
        │          │      │       │       │
     Vovin    Ain Soph  Verity  Ledger  Thessalon
     :8092    :8089     :8095   :8096   :8084
                                        │
                             POST /rank │  POST /v1/media/upload
                                  ▼               ▼
                            AethyrRank        Caeor
                              :8080            :8086

Elohim Veni :8093  ← Nantar bootstraps PIAL on signup (sync, once)
                   ← Zodacare sends moderation actions
Zodacare    :8090  ← Nantar reports content (async, fire-and-forget)
```

### Brain Isolation Rules

These rules are enforced by the CI gate at `enforcement/`. Violations block merge to `main`.

- Each brain owns exactly one database. No brain reads another brain's database.
- The PIAL UUID is the only cross-brain identity. Never use account UUIDs or handles cross-brain.
- Nantar is the sole edge brain. No other brain may call Nantar.
- Zodacare is advisory. Elohim Veni enforces. Never reverse this.
- AethyrRank, Vovin, Ain Soph, and Caeor cannot call each other.
- Thessalon may call Ain Soph (AET transfers) and Verity (KYC tier checks) only.
- All async data flow uses the Kafka event bus. Direct HTTP calls are permitted only where documented in `enforcement/dependency-graph/rules.yaml`.

Full dependency matrix: `BRAIN_MAP.md`

---

## Identity — PIAL

**PIAL (Persistent Identity + Access Layer)** is the cryptographic root of every user account.

```
Device keypair (ECDH-P256 + ECDSA-P256 Glyph)
    └── PIAL UUID  (permanent, immutable, never reused)
            ├── Handle(s)            ← transferable pointer (Registrar)
            ├── Ain Soph wallet      ← AET economic layer
            ├── Vovin vault          ← message encryption keys
            ├── Capabilities         ← posting, monetization, adult content, live
            └── Verity attestations  ← KYC tier 0–3
```

A PIAL UUID is generated by Nantar on first signup and bootstrapped into Elohim Veni synchronously. It cannot be transferred, renamed, or deleted. Every brain that needs to identify a user receives the PIAL UUID via the `X-Pial-Identity` header injected by Nantar's proxy layer.

The PIAL UUID survives account bans, handle changes, platform policy shifts, and payment processor pressure. Creators own their identity permanently.

---

## Economic Fabric — ETHRA / AET

AET (Aethyr Exchange Token) is the platform's internal settlement currency. It does not exist outside F33D3R.

```
Fiat deposit (payment rails)
    └── Aethyr Credit (1:1 with cents)
            └── AET (stored in µAET, 1 AET = 1,000,000 µAET)
                    ├── Tips             (1% platform fee)
                    ├── Subscriptions    (3% platform fee via Ain Soph transfer)
                    ├── Pay-per-view     (3% platform fee via Ain Soph transfer)
                    ├── Wallet transfers (3% fee)
                    ├── Boosts           (full burn to platform reserve)
                    └── Relay rewards    (minted from platform reserve)
```

**Platform fee: 2.5% blended** (1% on tips, 3% on transfers — versus OnlyFans at 20%)

All AET movements are recorded in Ain Soph (double-entry wallet) and settled through Aethyr Ledger (hash-chained immutable blocks every 2 seconds). Ain Soph enforces idempotency — duplicate requests return the original result.

### Commerce (Thessalon)

Thessalon handles all creator monetization:

| Endpoint | Description |
|---|---|
| `POST /thessalon/v1/creator/enable` | Enable monetization for a creator (checks Verity KYC tier) |
| `POST /thessalon/v1/plans` | Create subscription tier (price in AET) |
| `POST /thessalon/v1/subscribe` | Subscribe — deducts AET, creates 30-day period |
| `GET /thessalon/v1/access/subscription` | Gate check — `{ has_access: bool }` |
| `POST /thessalon/v1/ppv` | Register a PPV content item |
| `POST /thessalon/v1/ppv/:id/purchase` | Purchase PPV — deducts AET |
| `GET /thessalon/v1/access/ppv` | PPV access check by content_id |
| `POST /thessalon/v1/tips` | Send tip — calls Ain Soph `/v1/tip` |
| `GET /thessalon/v1/creator/:pial_id/earnings` | Earnings summary by stream |

---

## Identity Compliance — Verity

Verity stores decision hashes only. No ID images, no biometrics, no date of birth — only an `age_band` and a signed decision hash anchored to a PIAL UUID.

| Tier | Requirements | Unlocks |
|---|---|---|
| 0 | Account created | Basic browsing, posting |
| 1 | Email + phone verified | Full social features, DMs |
| 2 | Government ID + liveness check | Adult content access, creator signup |
| 3 | Tier 2 + fraud clearance | Payouts, full monetization |

Creator monetization requires Tier 1 minimum (Thessalon checks at enable time and stores the tier). Payment rails require Tier 3.

---

## Messaging Security — Vovin / AMP

Signal-grade end-to-end encryption. Keys are generated client-side in the browser's WebCrypto API, stored in IndexedDB, and never transmitted to any server.

| Layer | Algorithm |
|---|---|
| Long-term identity | ECDH-P256 |
| Signing | ECDSA-P256 (Glyph) |
| Session key agreement | X3DH (V1) / Double Ratchet (V2) |
| Per-message encryption | AES-256-GCM |
| Key derivation | HKDF-SHA256 |

Vovin stores ciphertext and IVs only. It cannot read message content under any circumstances. The browser requires HTTPS to access WebCrypto — see `infra/README-CERTS.md` for local TLS setup.

---

## Media Pipeline — Caeor

All uploads route through Caeor before being stored. Raw originals are never served in the feed.

| Media Type | Derivatives Generated | Format |
|---|---|---|
| Avatar | 64px · 128px · 256px · 512px (square center-crop) | WebP |
| Header/banner | 600×200 · 1200×400 · 2400×800 (3:1 center-crop) | WebP |
| Post image | 300×300 thumb · 800w feed · 1200w full | WebP |
| Video | Stored as-is | mp4/mov/webm |

Files are written to a shared Docker volume (`f33d3r_media`) and served by Nantar at `/static/media/*` with `Cache-Control: public, max-age=31536000, immutable`. Maximum upload size: 10 MB. Nantar falls back to direct storage if Caeor is unavailable.

---

## Local Development

### Prerequisites

Docker and Docker Compose. No local Go or Rust toolchain required.

### Start

```bash
git clone <repo>
cd f33d3r-local
bash bootstrap-local.sh
```

First run compiles all Rust brains — approximately 10 minutes. Subsequent starts: approximately 15 seconds.

The app runs at `http://localhost:8081`. For WebCrypto (E2E messaging) to work in Safari and on mobile devices, HTTPS via Caddy is required — see `infra/README-CERTS.md`.

### Dev accounts (password: `f33d3rdev`)

| Handle | Role |
|---|---|
| @edd | Founder / Admin |
| @admin | Admin |
| @creator | Creator |
| @dev | Engineer |
| @guest | User |

Grant admin to any account: `UPDATE users SET role = 'admin' WHERE handle = 'x';`

### Common commands

```bash
# Rebuild and restart a single brain
docker compose -f docker-compose.local.yml build feed-engine
docker compose -f docker-compose.local.yml up -d feed-engine

# Tail logs
docker logs -f f33d3r-local-feed-engine-1
docker logs -f f33d3r-local-thessalon-1

# Health check all brains
bash health-check-local.sh

# Postgres shell
docker exec -it f33d3r-local-postgres-1 psql -U f33d3r -d f33d3r_feed

# Commerce database
docker exec -it f33d3r-local-postgres-1 psql -U f33d3r -d f33d3r_commerce

# feed-engine: run outside Docker for fast iteration
cd feed-engine && make run
cd feed-engine && make run-dev   # DEV_MODE=true SHOW_SCORES=true
```

### Pinned versions — do not change without explicit approval

| Component | Version |
|---|---|
| feed-engine (Go) | 1.26.2 |
| All Rust brains | 1.95.0 |
| HTMX | 2.0.10 |
| PostgreSQL | 16 |

---

## CI / Branch Strategy

| Branch | Pipeline behavior |
|---|---|
| `feature/*` | Validate + test only |
| `develop` | Validate + test + build + enforce + auto-deploy staging |
| `main` | All stages + manual production deploy gate |

CI enforcement runs four validators on every branch:

- `enforcement/ci-gates/graph_validation.py` — brain dependency rules + version pins
- `enforcement/ci-gates/version_policy.py` — Dockerfile FROM pin compliance
- `enforcement/contract-checker/event_schema_validator.py` — PIAL field presence in all schemas
- `enforcement/contract-checker/api_schema_validator.py` — Kafka topic ↔ schema file coverage

All four must pass. Violations block merge.

---

## Repository Structure

```
f33d3r-local/
├── feed-engine/           ← Nantar (Go) — UI, social layer, proxy hub
├── aethyrrank-engine/     ← AethyrRank (Rust) — ranking
├── zior-engine/           ← Zior (Rust) — music intelligence
├── aethyr-msg/            ← Vovin (Rust) — messaging relay
├── ain-soph/              ← Ain Soph (Rust) — AET wallet
├── elohim-veni/           ← Elohim Veni (Rust) — PIAL enforcement
├── zodacare/              ← Zodacare (Rust) — content safety
├── aethyr-schema-registry/← Schema Registry (Rust) — contract versioning
├── registry-brain/        ← Registrar (Rust) — handle ownership
├── verity/                ← Verity (Rust) — KYC/compliance
├── aethyr-ledger/         ← Aethyr Ledger (Rust) — AET settlement
├── thessalon/             ← Thessalon (Rust) — commerce engine
├── caeor/                 ← Caeor (Rust) — media processing
├── infra/                 ← Caddyfile, certs, k3s manifests
├── enforcement/           ← CI validators, dependency rules, forbidden edges
├── events/schemas/        ← Canonical JSON schemas for all Kafka events
├── postgres-init/         ← Database creation scripts
├── scripts/               ← Ops tooling, daily audit
├── BRAIN_MAP.md           ← Canonical brain responsibility reference
├── ARCHITECTURE.md        ← System design detail
└── docker-compose.local.yml
```

---

## Security Notes

**The ranking algorithm uses Jungian psychology-based behavioral modeling. This is confidential IP.**

- `JungArchetype`, `aesq_alignment`, Jung axis labels, `shadowscoring` must never appear in any user-facing surface, API response, JS variable, or log message.
- Public-facing signal names are: Deep Space, Flow State, Soft Power, Sharp Edge, Root System, Golden Hour.
- AethyrRank uses these fields internally. They do not leave the ranking engine.

PIAL UUIDs must never be exposed to users. Verify before shipping any template or API response that contains identity fields.

---

## Further Reading

- `BRAIN_MAP.md` — brain responsibilities, dependency matrix, forbidden actions per brain
- `ARCHITECTURE.md` — system design, data flows, scaling model
- `CLAUDE.md` — engineering guidance for AI-assisted development in this repo
- `infra/README-CERTS.md` — local HTTPS setup for Safari / mobile device testing
- `enforcement/dependency-graph/rules.yaml` — machine-readable brain call rules
- `events/schemas/` — all canonical event schemas
# f33d3r
# f33d3r
# f33d3r
# f33d3r
# f33d3r
# f33d3r
# f33d3r
# f33d3r
# f33d3r
