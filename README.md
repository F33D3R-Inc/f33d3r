# F33D3R

**Distributed social operating system for creators, communities, and the creator economy.**

F33D3R is a multi-brain platform built on isolated service architecture, a cryptographic persistent identity layer (PIAL), Signal-grade end-to-end encrypted real-time messaging (Vovin), a closed-loop economic fabric (AET), a native commerce engine, and an HLS video pipeline. Platform fee: 2.5% versus the industry standard 20%.

---

## Platform Overview

| Capability | Implementation |
|---|---|
| Social feed + ranking | Nantar (Go) + AethyrRank (Rust) — AESQ scoring, LinUCB exploration, author dilution, media/long-form/velocity signals |
| Real-time E2E messaging | Vovin (Rust) — ECDH-P256, Double Ratchet per device, AES-256-GCM, IDB-first persistence, multi-device |
| Creator commerce | Thessalon (Rust) — subscriptions, pay-per-view, tips; 2.5% fee |
| Media processing + HLS | Caeor (Rust) — WebP derivatives, HLS transcoding, MinIO storage |
| Wallet + settlement | Ain Soph (Rust) + Aethyr Ledger (Rust) — double-entry AET ledger |
| Identity + capabilities | PIAL is the system of record + sole authority; brains write to it |
| KYC + compliance | Verity (Rust) — 4-tier attestation, privacy-preserving decision hashes |
| Content safety | Zodacare (Rust) + content-scan (Python) — risk scoring, moderation queue, auto-sweep for stuck posts |
| Music intelligence | Zior (Rust) — audio analysis, 8-axis vectorization, cluster ranking |
| Handle registry | Registrar (Rust) — handle ownership, transfers |
| Push notifications | Herald (Rust) — device push delivery, per-user subscriptions |
| Schema contracts | Schema Registry (Rust) — versioned event schemas, CI contract enforcement |
| Transcoding worker | Transcoding (Rust) — ffmpeg HLS ladder, poster frames |
| Observability | Prometheus + Grafana + Loki + Alertmanager + node-exporter + Promtail |

---

## Service Registry

| Brain | Service Dir | Port | Database | Language | Status |
|---|---|---|---|---|---|
| Nantar | `feed-engine` | 8081 | f33d3r_feed | Go (latest) | ✅ Production |
| AethyrRank | `aethyrrank-engine` | 8080 | f33d3r_feed | Rust (latest) | ✅ Production |
| Zior | `zior-engine` | 8082 | — | Rust (latest) | ✅ Production |
| Vovin | `aethyr-msg` | 8092 | f33d3r_msg | Rust (latest) | ✅ Production |
| Ain Soph | `ain-soph` | 8089 | f33d3r_wallet | Rust (latest) | ✅ Production |
| Elohim Veni | `elohim-veni` | 8093 | f33d3r_security | Rust (latest) | ✅ Production |
| Zodacare | `zodacare` | 8090 | f33d3r_safety | Rust (latest) | ✅ Production |
| Schema Registry | `aethyr-schema-registry` | 8079 | f33d3r_registry | Rust (latest) | ✅ Production |
| Registrar | `registry-brain` | 8094 | f33d3r_handles | Rust (latest) | ✅ Production |
| Verity | `verity` | 8095 | f33d3r_verity | Rust (latest) | ✅ Production |
| Aethyr Ledger | `aethyr-ledger` | 8096 | f33d3r_ledger | Rust (latest) | ✅ Production |
| Thessalon | `thessalon` | 8084 | f33d3r_commerce | Rust (latest) | ✅ Production |
| Caeor | `caeor` | 8086 | — | Rust (latest) | ✅ Production |
| Transcoding | `transcoding` | 8085 | — | Rust (latest) | ✅ Production |
| Content Scan | `content-scan` | 8097 | SQLite (content_scan.db) | Python (latest) | ✅ Production |
| Herald | `herald` | 8105 | f33d3r_herald | Rust (latest) | ✅ Production |
| Astraon | `astraon` | 8088 | f33d3r_feed | Rust (latest) | ✅ Production |
| Auralis (Frequencies) | `frequencies` | 8108 | f33d3r_frequencies | Rust (latest) | 🔧 Phase 1 — control plane |
| eKYC | `ekyc` | 8099 | — | Python (latest) | ✅ Production |
| Loxion | — | 8087 | — | TBD | 🔲 Planned (geolocation brain — no code yet) |

**Infrastructure:** PostgreSQL · Redis · MinIO · PgBouncer · Caddy  
**Observability:** Prometheus (9090) · Grafana (3000) · Loki (3100) · Alertmanager (9093) · node-exporter (9100) · Promtail

---

## Architecture

### Frontend — Facet Architecture (FA) · v1.1

FA is F33D3R's server-rendered hypermedia UI system. Every visible element is a named **Facet** — a self-contained HTML template with a formal FDL contract declaring its inputs, states, children, and signals.

| Concept | Rule |
|---|---|
| Pages | Assemble Facets. No raw HTML structure. |
| Facets | One file, one `{{define}}`, one CSS block, one FDL contract. |
| Handlers | Fetch data, hydrate structs fully, call `h.render()`. Never write HTML strings. |
| Mutations | `POST /events {event_type}` only. |
| Reads | `GET /facets/{name}` only. |
| Push | SSE on `/api/events`. Pre-rendered HTML fragments — never JSON-then-template-on-client. |

Facets live in `feed-engine/web/templates/partials/_name.html`. Four layers: atomic → composite → overlay → page.

**v1.1 (May 2026) — Mobile-first adaptive UI:**

- **Adaptive icon rail** — sidebar collapses to 52px icon-only rail on mobile (X.com pattern), never disappears. Full pill on desktop, circle on narrow rail for account.
- **WYSIWYG compose** — link preview cards and quoted-post cards render inside the compose modal before posting, served from `/facets/link_preview` and `/facets/quoted_post`.
- **URL shortening** — long URLs in post bodies are display-shortened (`domain.com/path…`) everywhere via `renderMarkdown`. Full URL preserved in `href`.
- **Post detail Facets** — `post_detail_focus`, `reply_compose_row`, `profile_header` extracted from inline page templates. Three pages now fully FA-compliant.
- **Mobile CSS** — `100dvh` everywhere, safe-area-insets on compose FAB and toasts, `@media(hover:hover)` guards on all interactive elements, `aspect-ratio` on media grids, settings horizontal chip nav on mobile.

### Network Topology

```
Browser (HTMX)
         │
         ▼
  ┌──────────────────────────────────────────────┐
  │         Caddy  :443/:80  (TLS, reverse proxy) │
  └──────────────────────────┬───────────────────┘
                             │
  ┌──────────────────────────▼───────────────────┐
  │            Nantar  :8081  (Go)               │
  │  The only brain that serves HTML to browsers  │
  └──┬──────┬──────┬──────┬──────┬───────────────┘
     │      │      │      │      │
  /vovin/ /verity/ /ledger/ /ainsoph/ POST /rank  POST /v1/media/upload
     │      │      │      │      │                │
   Vovin  Verity Ledger AinSoph AethyrRank      Caeor → MinIO
   :8092  :8095  :8096  :8089   :8080           :8086   :9000

Elohim Veni :8093 ← Nantar bootstraps PIAL on signup (sync)
Zodacare    :8090 ← Nantar reports content (async, fire-and-forget)
Transcoding :8085 ← Caeor spawns HLS transcode jobs
```

### Brain Isolation Rules

- Each brain owns exactly one database. No brain reads another brain's database.
- The PIAL UUID is the only cross-brain identity. Never use account UUIDs or handles cross-brain.
- Nantar is the sole edge brain. No other brain may call Nantar.
- Zodacare is advisory. Elohim Veni enforces. Never reverse this.

Full dependency matrix: `BRAIN_MAP.md`

---

## Identity — PIAL

**PIAL (Persistent Identity + Access Layer)** is the cryptographic root of every user account.

```
Device keypair (ECDH-P256 + ECDSA-P256 Glyph)
    └── PIAL UUID  (permanent, immutable, never reused)
            ├── Handle(s)            ← transferable pointer (Registrar)
            ├── Ain Soph wallet      ← AET economic layer
            ├── Vovin vault          ← message encryption keys, per-device
            ├── Capabilities         ← posting, monetization, adult content, live
            └── Verity attestations  ← KYC tier 0–3
```

A PIAL UUID is generated by Nantar on first signup and bootstrapped into Elohim Veni synchronously. It cannot be transferred, renamed, or deleted. PIAL UUIDs are never exposed to users or rendered in HTML.

---

## Messaging — Vovin

Signal-grade end-to-end encryption with full multi-device support.

| Layer | Algorithm |
|---|---|
| Long-term identity | ECDH-P256 per device |
| Signing | ECDSA-P256 (Glyph key) |
| Session key agreement | ECDH + Double Ratchet (V2) |
| Per-message encryption | AES-256-GCM |
| Key derivation | HKDF-SHA256 |

**Client architecture:**
- Vault stored in IndexedDB, namespaced per PIAL. Survives logout. Not tied to session cookie.
- Ratchet sessions scoped per `(recipientPIAL, recipientDeviceID)` — concurrent sends to multi-device recipients don't corrupt each other's ratchet state.
- WebSocket connects on page load independent of vault lock state.
- Multi-device: messages delivered to all registered devices. New device receives new messages from registration point.
- Typing indicators, read receipts, presence pings, voice notes, file transfers (AFF).
- DM badge in sidebar nav updates via SSE when new messages arrive.

---

## Content Safety

Posts are inserted with `scan_state = 'pending_scan'` and become visible after the content-scan callback transitions them to `clean`, `age_gated`, `human_review`, or `blocked`.

A cron sweep runs every 10 minutes and rescues posts stuck in `pending_scan` for over 15 minutes by re-triggering the scan. Falls back to `clean` if content-scan is unreachable. The sweep also fires once on startup. Manual trigger available in **Admin → Dev Tools → Pending scan sweep**.

---

## Media Pipeline — Caeor + Transcoding

All uploads route through Caeor. Videos are transcoded to HLS multi-bitrate by the Transcoding brain.

| Media Type | Output | Storage |
|---|---|---|
| Avatar | 64/128/256/512px WebP | MinIO → served via Caddy |
| Post image | 300/800/1200px WebP | MinIO → served via Caddy |
| Video (.mov/.mp4/etc) | HLS ladder (240/360/720/1080/2160p) + poster | MinIO `media-derived/` |
| Original (any) | Stored raw | MinIO `media-raw/` |

---

## Economic Fabric — AET

AET (Aethyr Exchange Token) is F33D3R's internal settlement currency.

```
Fiat deposit → Aethyr Credit (1:1 cents) → AET (1 AET = 1,000,000 µAET)
    ├── Tips             (1% platform fee)
    ├── Subscriptions    (3% platform fee)
    ├── Pay-per-view     (3% platform fee)
    ├── Wallet transfers (3% fee)
    └── Boosts           (full burn to platform reserve)
```

**Platform fee: 2.5% blended** (vs OnlyFans at 20%)

---

## Observability

| Service | URL | Purpose |
|---|---|---|
| Grafana | http://localhost:3000 | Dashboards — F33D3R Overview provisioned |
| Prometheus | http://localhost:9090 | Metrics, 14-day retention, all brains scraped |
| Loki | http://localhost:3100 | Logs, 30-day retention, all containers via Promtail |
| Alertmanager | http://localhost:9093 | Alert routing (BrainDown, HighErrorRate, DiskHigh) |
| MinIO Console | http://localhost:9001 | Object storage (media-raw, media-derived, backups) |

Grafana: `admin` / `f33d3rdev_change_in_prod`

---

## Local Development

### Prerequisites

Docker and Docker Compose. No local Go or Rust toolchain required.

```bash
git clone <repo>
cd f33d3r-local
bash bootstrap-local.sh
```

First run compiles all Rust brains — approximately 10–15 minutes. Subsequent starts: ~15 seconds.

App: `https://localhost:8443` (Caddy, local CA — see `infra/README-CERTS.md`).
Other devices on the LAN: `https://<this machine's IP>:8443` (`LIVE_PUBLIC_HOST` in `.env.local`).
There is deliberately no plain-HTTP port: the app sits behind Caddy locally exactly as in
production, and live video only plays through that edge.

### Dev accounts (password: `f33d3rdev`)

| Handle | Role |
|---|---|
| @tehanibentley | Founder / Admin |
| @admin | Admin |
| @miiyazuko | Creator |
| @dev | Engineer |
| @guest | User |

Grant admin: `UPDATE users SET role = 'admin' WHERE handle = 'x';`

### Common commands

```bash
# Rebuild a single brain
docker compose -f docker-compose.local.yml build feed-engine
docker compose -f docker-compose.local.yml up -d feed-engine

# Tail logs
docker logs -f f33d3r-local-feed-engine-1

# Health check
bash health-check-local.sh

# Postgres shell
docker exec -it f33d3r-local-postgres-1 psql -U f33d3r -d f33d3r_feed

# Run feed-engine outside Docker (fast iteration)
cd feed-engine && make run
cd feed-engine && make run-dev   # DEV_MODE=true SHOW_SCORES=true

# Run nightly backup manually
docker compose -f docker-compose.local.yml --profile backup run --rm backup
```

### Version policy

**Never pin versions.** All images use `:latest`. All Cargo crates use caret-major (`"0.1"`). All Go modules resolve via `go mod tidy`. Lock files (`Cargo.lock`, `go.sum`) provide reproducibility — not hand-typed version strings.

---

## Admin Panel

Available at `/admin` (admin/founder role required). Tabs: Overview · Users · Moderation · DMCA · Treasury · Analytics · Dev Tools.

**Dev Tools** provides one-click operations without DB access:
- Inject AET balance to any user
- Reset account password
- Set email address
- Set PIAL capability (grant/revoke/restrict)
- **Resync follower/following counts** — recomputes all cached counts from live `follows` data
- **Pending scan sweep** — rescues posts stuck in "Under review" immediately

---

## CI / Branch Strategy

| Branch | Pipeline |
|---|---|
| `feature/*` | Validate + test |
| `fa` | Active development branch — Facet Architecture |
| `main` | Production |

---

## Repository Structure

```
f33d3r-local/
├── feed-engine/               ← Nantar (Go) — UI, social, proxy hub
├── aethyrrank-engine/         ← AethyrRank (Rust) — ranking engine
├── zior-engine/               ← Zior (Rust) — music intelligence
├── aethyr-msg/                ← Vovin (Rust) — messaging relay
├── ain-soph/                  ← Ain Soph (Rust) — AET wallet
├── elohim-veni/               ← Elohim Veni (Rust) — moderation decisions → writes PIAL
├── zodacare/                  ← Zodacare (Rust) — content safety
├── aethyr-schema-registry/    ← Schema Registry (Rust) — contract versioning
├── registry-brain/            ← Registrar (Rust) — handle ownership
├── verity/                    ← Verity (Rust) — KYC/compliance
├── aethyr-ledger/             ← Aethyr Ledger (Rust) — AET settlement
├── thessalon/                 ← Thessalon (Rust) — commerce engine
├── caeor/                     ← Caeor (Rust) — media processing + HLS
├── transcoding/               ← Transcoding (Rust) — ffmpeg HLS worker
├── infra/
│   ├── observability/         ← Prometheus, Loki, Grafana, Alertmanager configs
│   ├── backup/                ← pg_dump → MinIO backup scripts
│   ├── postgres/              ← postgresql.conf tuning
│   └── caddy/                 ← Caddyfile (local + prod)
├── postgres-init/             ← DB creation + extension init scripts
├── enforcement/               ← CI validators, dependency rules
├── events/schemas/            ← Canonical Kafka event schemas
├── BRAIN_MAP.md               ← Brain responsibility reference
├── ARCHITECTURE.md            ← System design
├── SPRINT_LOG.md              ← Session-by-session engineering log
├── PROJECT_STATE.md           ← Live brain status + decisions
└── docker-compose.local.yml
```

---

## Security Notes

**The ranking algorithm uses behavioral psychology modeling. This is confidential IP.**

Internal scoring terms must never appear in user-facing surfaces, API responses, JS variables, or log messages. See `CLAUDE.md` for the full forbidden-term list.

PIAL UUIDs must never be exposed to users or rendered in any template, data attribute, or JS variable.

---

## Further Reading

- `BRAIN_MAP.md` — brain responsibilities and dependency matrix
- `ARCHITECTURE.md` — system design and data flows
- `SPRINT_LOG.md` — session-by-session engineering history
- `PROJECT_STATE.md` — live status and decisions
- `CLAUDE.md` — engineering rules for AI-assisted development
- `infra/README-CERTS.md` — local HTTPS setup
