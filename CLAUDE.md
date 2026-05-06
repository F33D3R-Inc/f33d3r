# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

---

## ⛔ MUST-HAVE RULE — VERSIONS

**NEVER hand-pick versions. ALWAYS use the latest stable software as of the current date.**

- Docker images: use `:latest` (or unversioned). NEVER pin to a specific tag like `:1.23.1`, `:RELEASE.2026-04-15`, `:v2.55.0`.
- Language base images: `rust:latest`, `golang:latest`, `node:latest`, etc. NEVER `rust:1.95.0`, `golang:1.26.2`.
- Cargo crates: use the major version (e.g. `tracing = "0.1"` — semver caret means latest compatible 0.1.x). NEVER add patch-level pins.
- Go modules: use major version where possible. `go.mod` `require` lines are picked by `go mod tidy` — let it resolve to latest.
- Anything Anthropic / OS-level: stable channel, no version locks.

This rule was added after a build failure where pinned tags didn't exist on the registry. The build only worked once the user set every image to `:latest`. Reproducible builds at scale come from a lock file (`Cargo.lock`, `go.sum`, container digest), not from human-typed version strings.

Floor versions in the table below are HISTORICAL and superseded by this rule. Do not introduce new version pins. If you see one in old code, replace it with `:latest` when you touch the file.

---

## Build & Run

```bash
# Start everything (first run ~5–10 min; subsequent ~15 sec)
bash bootstrap-local.sh
# or:
docker compose -f docker-compose.local.yml up --build

# Rebuild and restart a single brain
docker compose -f docker-compose.local.yml build feed-engine
docker compose -f docker-compose.local.yml up -d feed-engine

# Check all services
bash health-check-local.sh

# Logs
docker logs -f f33d3r-local-feed-engine-1

# Postgres shell
docker exec -it f33d3r-local-postgres-1 psql -U f33d3r -d f33d3r_feed

# feed-engine: run outside Docker (for fast iteration)
cd feed-engine && make run          # normal
cd feed-engine && make run-dev      # DEV_MODE=true SHOW_SCORES=true
cd feed-engine && go build -o feed-engine ./cmd/server   # binary only
```

No local Rust or Go toolchain required — Docker handles all compilation.

---

## Secret Sauce — Mandatory Rule

**The ranking algorithm is Jungian-psychology-based. This is confidential IP.**

- `JungArchetype`, `aesq_alignment`, Jung axis labels, `shadowscoring`, `jungfeed` must **never** appear in any user-facing template, API response, JS variable, or error message.
- `AethyrRank` internally uses these fields; they stay inside the ranking engine only.
- Public-facing vibe names: **Deep Space, Flow State, Soft Power, Sharp Edge, Root System, Golden Hour**.
- Before editing any template or API response, scan for `archetype`, `jung`, `aesq`, `shadow` used as labels and strip them.

---

## Architecture: How the Brains Connect

All brains are documented in `BRAIN_MAP.md` and `ARCHITECTURE.md`. The critical runtime topology:

```
Browser (HTMX)
    │
    ▼
Nantar :8081 (feed-engine, Go)   ← ONLY brain that serves HTML to browsers
    ├─ /vovin/*   → Vovin    :8092  (aethyr-msg, Rust)
    ├─ /ainsoph/* → Ain Soph :8089  (ain-soph, Rust)
    ├─ /verity/*  → Verity   :8095  (verity, Rust)
    ├─ /ledger/*  → Ledger   :8096  (aethyr-ledger, Rust)
    ├─ POST /rank → AethyrRank :8080 (aethyrrank-engine, Rust)
    └─ POST /v1/media/upload → Caeor :8086 (caeor, Rust)

Elohim Veni :8093 ← Nantar bootstraps PIAL here on signup (synchronous)
                  ← Zodacare sends moderation actions here
Zodacare    :8090 ← Nantar reports content here (async, fire-and-forget)
Caeor       :8086 ← Nantar forwards uploads here; Caeor writes to shared f33d3r_media
                     volume, Nantar serves from /static/media/* (1-year cache headers)
```

**Brain call rules** (enforced by `enforcement/`):
- Nantar is the only edge brain. No other brain may call Nantar.
- No brain reads another brain's database.
- Zodacare is advisory; Elohim Veni enforces. Never call them in reverse.
- AethyrRank, Vovin, Ain Soph never call each other.
- Full dependency matrix: `BRAIN_MAP.md` § Dependency Matrix.

---

## feed-engine (Nantar) — Internal Layout

```
feed-engine/
├── cmd/server/main.go          ← entry point, CSP headers, TLS config
├── internal/
│   ├── config/config.go        ← env var parsing (PORT, AETHYRRANK_URL, etc.)
│   ├── model/types.go          ← all structs: User, Post, Track, PIAL*, FeedPage, ...
│   ├── db/
│   │   ├── queries.go          ← all SQL: posts, follows, likes, notifications, tracks
│   │   ├── migrate.go          ← schema migration (run on startup)
│   │   ├── pial.go             ← PIAL root + capability queries
│   │   └── security.go         ← session token, user_roles, user_credentials
│   ├── handler/
│   │   ├── handlers.go         ← ALL HTTP handlers + route registration (Routes())
│   │   ├── helpers.go          ← htmxError(), TimeAgo(), AvatarColors()
│   │   └── auth.go             ← userFromRequest(), requireHandle()
│   ├── middleware/
│   │   ├── ratelimit.go        ← in-memory rate limiter (rlRead/rlWrite/rlAuth)
│   │   └── csrf.go             ← CSRF token middleware
│   ├── aethyr/                 ← AethyrRank HTTP client
│   └── realm/                  ← XP thresholds, realm name lookups
└── web/
    ├── templates/
    │   ├── base.html           ← shell: sidebar, nav, compose modal, SSE setup
    │   ├── feed_items.html     ← HTMX partial for post cards (shared across surfaces)
    │   └── *.html              ← one file per page
    └── static/
        ├── css/styles.css
        └── js/
            ├── f33d3r.js       ← shared JS: SSE, compose, toast, mobile nav, timestamps
            └── pages/*.js      ← page-specific JS (messages.js, profile.js, etc.)
```

### Template system

Templates are server-side rendered. `base.html` defines the shell; every page template fills `{{block "body" .}}`. `feed_items.html` is parsed alongside every page template for HTMX partials.

Custom template functions registered in `handlers.go` `loadTemplates()`:
`timeAgo`, `avatarColors`, `themeAccent`, `themeSurface`, `add`, `pct`, `safeHTML`, `firstChar`, `hasPrefix`, `socialLinks`, `div`, `mkRange`, `waveBarH`, `fmtDuration`, `realmName`, `renderMarkdown`, `renderTags`, `xpPercent`, `isAdult`, `isCreator`, `isVerified`.

### HTMX patterns

- Feed infinite scroll: `hx-get="/feed/items" hx-trigger="revealed"` on a sentinel div.
- Actions (like, repost, follow): `hx-post` returns an HTML fragment that replaces the button (`hx-swap="outerHTML"`).
- SSE real-time: `/api/events` EventSource (SSE) in `f33d3r.js`; events `notify`, `balance`, `dm`.
- HTMX errors get styled HTML fragments via `htmxError()` helper (checks `HX-Request` header).

### JavaScript conventions

- `f33d3r.js` loads **before** any page `<script>` block. It exposes globals (`openCompose`, `toast`, `showMobileNotifTicker`, `formatPostTimes`).
- Page-specific files in `web/static/js/pages/` — included at the bottom of their template via `<script src="...">`.
- No inline `onclick` on compose modal buttons — all wired in `f33d3r.js` DOMContentLoaded. Inline `onclick` IS allowed on HTMX action buttons and drawer nav links (CSP has `unsafe-inline`).
- Post timestamps: use `<time class="post-time" data-ts="{{unix epoch seconds}}">` — `formatPostTimes()` in f33d3r.js formats them on load and on every `htmx:afterSwap`.

---

## PIAL — The Identity Spine

PIAL UUID is the **only** cross-brain identity. Never use `user.ID` (account UUID) or `user.Handle` for cross-brain calls.

- Created in `feed-engine` (`pial_roots` table in `f33d3r_feed`).
- Mirrored into Elohim Veni's `pial_states` table via synchronous `/v1/pial/bootstrap` call on signup.
- Auto-bootstrap: `userFromRequest()` in `auth.go` bootstraps PIAL for any pre-existing account that lacks one.
- Vovin uses PIAL UUID as messaging identity — injected via `X-Vovin-Identity` header in `vovinProxy`.
- Capability check: `dbpkg.HasCapability(h.db, user.PIALID, model.CapPosting)` reads cached `pial_capabilities` table in `f33d3r_feed`.

---

## Vovin (Messaging) — Client-Side Crypto

End-to-end encrypted. Nantar proxies `/vovin/*` → aethyr-msg:8092.

- Keys live in browser IndexedDB only. Namespaced per PIAL: `vovin_v1_<PIAL_UUID>`.
- `messages.js` implements the full crypto stack: ECDH-P256 vault (V1) + Double Ratchet (V2).
- Device registration: `POST /vovin/v1/devices` with `{pial_id, device_id, public_key_b64, ...}`.
- Legacy identity: `POST /vovin/v1/identity` (kept for backward compat, non-blocking).
- The `MY_PIAL` value is read from `<meta name="f33d3r:pial">` injected server-side in base.html.
- If `MY_PIAL` is empty, `boot()` shows an error and stops — never proceed with empty PIAL.

---

## Minimum Versions

Always use these versions or newer. Never go below these floors.

| Component | Minimum | Notes |
|-----------|---------|-------|
| feed-engine Go | 1.26.2 | go.mod + Dockerfile `golang:1.26.2-alpine` |
| All Rust brains | 1.95.0 | `rust:1.95.0-slim-bookworm` in every Dockerfile |
| HTMX | 2.0.10 | `unpkg.com/htmx.org@2.0.10` |
| PostgreSQL | 18 | `postgres:18-alpine` |
| Tailwind | 4.2.4 | `@tailwindcss/browser@4.2.4` via jsdelivr CDN |
| Python | 3.14.4 | enforcement scripts + any tooling |

---

## Databases

Each brain has its own database. Never cross-query.

| Brain | Database |
|-------|----------|
| Nantar + AethyrRank | f33d3r_feed |
| Vovin | f33d3r_msg |
| Ain Soph | f33d3r_wallet |
| Elohim Veni | f33d3r_security |
| Zodacare | f33d3r_safety |
| Schema Registry | f33d3r_registry |
| Verity | f33d3r_verity |
| Aethyr Ledger | f33d3r_ledger |

---

## Realm/XP System (public)

5 levels: **Wanderer (R1) → Initiate (R2) → Seeker (R3) → Adept (R4) → Guardian (R5)**  
XP thresholds: 0 / 500 / 2000 / 7500 / 20000

Use `realmName` template func and `realm.XPThreshold()` in Go code. Never expose "Jung" or archetype names publicly.

---

## Dev Accounts

All seeded with password `f33d3rdev`:

| Handle | Role |
|--------|------|
| @edd | Founder / Admin |
| @admin | Admin |
| @creator | Creator |
| @dev | Engineer |
| @guest | User |

To grant admin: `UPDATE users SET role = 'admin' WHERE handle = 'x';`
