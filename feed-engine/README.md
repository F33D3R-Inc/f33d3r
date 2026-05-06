# Nantar — Feed Engine

The frontend server and social layer for the F33D3R platform.

**Brain name:** Nantar  
**Port:** 8081  
**Database:** f33d3r_feed (PostgreSQL 18)

Built with:

- **Go 1.26.2** — zero-dependency HTTP server, `html/template` SSR
- **HTMX 2.0.10** — hypermedia-driven interactions, no JS framework
- **Tailwind CSS** — via CDN, no build step required
- **DM Serif Display + DM Sans** — typography system
- **No React. No npm. No build pipeline.**

---

## What Nantar does

Nantar is the entry point for all users. It owns:

- Social feed (posts, replies, likes, reposts, bookmarks)
- User profiles, follows, and notifications
- Music listener UI (Zior integration)
- Wallet display (Ain Soph integration)
- Vovin messaging UI (E2E encrypted, client-side keypairs)
- PIAL identity bootstrap and capability enforcement
- Media uploads (images, video, audio tracks)
- Search (full-text posts + user handle search)
- Admin dashboard (report queue, capability management)
- AethyrRank integration (ranked feed delivery)

---

## Architecture

```
Browser
  │
  ├─ GET  /                    → Full page SSR (home feed)
  ├─ GET  /feed                → HTMX partial (ranked post cards)
  ├─ GET  /explore             → HTMX partial (explore surface)
  ├─ GET  /music/tracks        → HTMX partial (track feed)
  ├─ POST /api/post            → Create post (with media_urls, comment_gating)
  ├─ POST /api/reply           → Create reply (gating enforced server-side)
  ├─ POST /feed/item/like      → HTMX swap (like button state)
  ├─ POST /feed/item/save      → HTMX swap (bookmark button state)
  ├─ POST /feed/item/repost    → HTMX swap (repost button state)
  ├─ POST /feed/item/dislike   → HTMX swap (not-for-me signal)
  ├─ GET  /api/users/search    → JSON — mention autocomplete
  ├─ POST /api/feedback        → Forward to AethyrRank (engagement signals)
  ├─ GET  /vovin/*             → Proxy to Vovin (messaging relay)
  └─ POST /api/report          → Forward to Zodacare (content reports)
  ↓
Feed Engine (Go, port 8081)
  ├─ POST /rank                → AethyrRank :8080
  ├─ POST /feedback            → AethyrRank :8080
  ├─ GET  /v1/tracks           → Zior :8082
  ├─ GET  /balance/:user_id    → Ain Soph :8089
  ├─ POST /v1/reports          → Zodacare :8090
  ├─ POST /v1/pial/bootstrap   → Elohim Veni :8093
  └─ /v1/*                     → Vovin :8092 (via vovinProxy)
```

---

## PIAL integration

Every account is anchored to a PIAL root UUID on first login. Nantar:

1. Creates the PIAL root in `pial_roots` table (f33d3r_feed DB)
2. Calls Elohim Veni `/v1/pial/bootstrap` to mirror it into the security DB
3. Grants default capabilities: `POSTING`, `MESSAGING`, `MUSIC_UPLOAD`, `REALM_PROGRESSION`, `NEW_ACCOUNT_TRUST`
4. Auto-bootstraps existing accounts missing PIAL on their next request (`userFromRequest`)
5. Injects the PIAL UUID into every Vovin proxy call as `X-Vovin-Identity` header

Capability checks run on every write action via `dbpkg.HasCapability()`.

---

## Comment audience gating

Posts support a `comment_gating` field controlling who can reply:

| Value | Who can reply |
|-------|--------------|
| `open` | Everyone (default) |
| `followers` | Only users following the author |
| `verified` | Only verified accounts |
| `none` | No replies allowed |

Gating is enforced server-side in `replyAPI`. The UI shows a gating badge on restricted posts and hides the reply composer when `none`.

---

## Vovin proxy

All `/vovin/*` requests are proxied to `aethyr-msg` (Vovin brain). The proxy:

- Validates that the user has a PIAL ID (fails fast if not ready)
- Injects `X-Vovin-Identity: <pial_id>` and `X-Vovin-Handle: <handle>`
- Forwards request body and Content-Type unchanged
- Returns the Vovin response directly to the browser

This means the browser's Vovin JS calls `/vovin/v1/identity`, `/vovin/v1/dm`, etc. — all proxied securely through the authenticated session.

---

## Content safety

Nantar is a reporter, not an enforcer. On content reports:

1. User clicks "Report" on a post/track
2. Nantar sends `POST /v1/reports` to Zodacare asynchronously
3. Zodacare scores the report and updates the user's risk profile
4. If risk score exceeds threshold, Zodacare calls Elohim Veni to restrict capabilities
5. Elohim Veni revokes `POSTING` or issues a `cooldown` state

Nantar never makes moderation decisions directly.

---

## Configuration

All config via environment variables (set in `docker-compose.local.yml`):

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8081` | Listen port |
| `DATABASE_URL` | — | PostgreSQL connection string (f33d3r_feed) |
| `AETHYRRANK_URL` | `http://aethyrrank-engine:8080` | AethyrRank base URL |
| `AETHYRRANK_TIMEOUT_MS` | `200` | Timeout for /rank calls (ms) |
| `ZIOR_URL` | `http://zior-engine:8082` | Zior base URL |
| `AIN_SOPH_URL` | `http://ain-soph:8089` | Ain Soph base URL |
| `VOVIN_URL` | `http://aethyr-msg:8092` | Vovin base URL |
| `ZODACARE_URL` | `http://zodacare:8090` | Zodacare base URL |
| `ELOHIM_VENI_URL` | `http://elohim-veni:8093` | Elohim Veni base URL |
| `FEED_PAGE_SIZE` | `20` | Items returned per feed page |
| `FEED_MAX_CANDIDATES` | `200` | Candidates submitted to AethyrRank |
| `DEFAULT_SURFACE` | `feed` | Default ranking surface |
| `SHOW_SCORES` | `false` | Show AethyrRank debug scores on post cards |
| `SESSION_SECRET` | — | CSRF + session signing key (min 32 chars) |

---

## Project structure

```
feed-engine/
├── cmd/server/main.go          ← Entry point, graceful shutdown
├── internal/
│   ├── aethyr/
│   │   ├── client.go           ← AethyrRank HTTP client (rank + feedback)
│   │   └── store.go            ← Content store interface + fallback ranker
│   ├── config/config.go        ← Env-based config loader
│   ├── db/
│   │   ├── db.go               ← DB connection pool
│   │   ├── migrate.go          ← Embedded DDL migrations (runs on startup)
│   │   ├── pial.go             ← PIAL create, bind, bootstrap, capability check
│   │   ├── queries.go          ← All SQL queries (posts, users, tracks, follows)
│   │   └── security.go         ← Crypto utilities
│   ├── handler/
│   │   ├── handlers.go         ← All HTTP handlers + route wiring
│   │   └── helpers.go          ← TimeAgo, AvatarColors, SafetyEpsilon, themes
│   ├── middleware/
│   │   ├── csrf.go             ← Double-origin CSRF check
│   │   └── ratelimit.go        ← Per-IP token bucket (reads/writes/auth)
│   ├── model/types.go          ← All domain types + AethyrRank wire types
│   └── realm/realm.go          ← XP system + realm progression
├── web/
│   ├── templates/
│   │   ├── base.html           ← HTML shell, fonts, Tailwind, HTMX, compose modal
│   │   ├── index.html          ← Home feed
│   │   ├── explore.html        ← Explore (For You / Trending / Latest tabs)
│   │   ├── search.html         ← Search (Posts + People tabs)
│   │   ├── music.html          ← Zior — Listen mode + Artist Studio
│   │   ├── messages.html       ← Vovin — E2E encrypted DMs
│   │   ├── wallet.html         ← Ain Soph — AET balance display
│   │   ├── profile.html        ← User profile + tabs
│   │   ├── settings.html       ← Profile editor + theme/pronouns/location
│   │   ├── post.html           ← Post detail + replies + action bar
│   │   ├── notifications.html  ← Notification feed
│   │   ├── bookmarks.html      ← Saved posts
│   │   ├── onboard.html        ← Step 1 — handle + theme selection
│   │   ├── onboard_setup.html  ← Step 2 — Vovin vault setup (ECDH keygen)
│   │   ├── login.html          ← Login form
│   │   ├── admin.html          ← Admin dashboard (reports, PIAL management)
│   │   ├── feed_items.html     ← Post card component (HTMX partial)
│   │   └── track_items.html    ← Music track card component
│   └── static/
│       ├── css/styles.css      ← Design tokens, themes, components
│       └── uploads/            ← Uploaded media (avatars, headers, posts, audio)
└── Dockerfile                  ← golang:1.26.2-alpine builder
```

---

## Themes

6 built-in themes, selectable in settings:

| ID | Name | Vibe | Accent |
|----|------|------|--------|
| `void` | Void | Deep Space | Indigo `#7B68EE` |
| `aurora` | Aurora | Flow State | Cyan `#3DD4BE` |
| `sakura` | Sakura | Soft Power | Pink `#E8A0B4` |
| `obsidian` | Obsidian | Sharp Edge | Slate Blue `#8B9BE8` |
| `moss` | Moss | Root System | Emerald `#7DC98A` |
| `dusk` | Dusk | Golden Hour | Amber `#D4A96A` |

Users can also set a custom accent hex that overrides the theme default.

---

## Rate limits

Three separate token buckets per IP:

| Bucket | Limit | Applies to |
|--------|-------|-----------|
| Read | 300 req/min | Pages, feeds, search |
| Write | 60 req/min | Posts, likes, follows, uploads |
| Auth | 10 req/min | Login, onboard |

---

## AethyrRank feedback loop

Every user interaction fires back to AethyrRank to update the LinUCB exploration model:

| User action | Event type |
|-------------|-----------|
| Post appears in feed | `impression` |
| User clicks post | `click` |
| User likes | `like` |
| User reposts | `share` |
| User bookmarks | `save` |
| User dislikes | `negative_feedback` |
| User reads > 2s | `view_complete` (dwell tracking via IntersectionObserver) |

All feedback is fire-and-forget (goroutine). It never blocks the feed response.
