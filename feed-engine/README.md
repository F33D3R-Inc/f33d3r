# Nantar — Feed Engine

The frontend server and social layer. The only brain that serves HTML to browsers.

**Port:** 8081 · **DB:** f33d3r_feed · **Language:** Go (latest)

---

## What Nantar owns

- All HTMX server-side rendering
- User sessions + PIAL bootstrapping on signup
- Social graph: posts, follows, likes, bookmarks, notifications
- Media upload proxy → Caeor
- AethyrRank client (`POST /rank`)
- Prometheus metrics at `/metrics` + structured JSON logs via `internal/observ/`

## Key internals

```
cmd/server/main.go          entry point, CSP headers, TLS
internal/
├── config/config.go        env var parsing
├── model/types.go          User, Post, Track, FeedPage structs
├── db/
│   ├── queries.go          all SQL
│   ├── migrate.go          schema migration (runs on startup)
│   └── pial.go             PIAL capability queries
├── handler/
│   ├── handlers.go         HTTP route registration
│   ├── onboard.go          signup flow, auto-follow @tehanibentley
│   └── media.go            upload proxy → Caeor, HLS polling
├── observ/                 JSON logging, /metrics, request-ID middleware
└── realm/                  XP thresholds, realm names
web/
├── templates/              HTMX templates (base.html, feed_items.html, etc.)
└── static/
    ├── css/styles.css
    └── js/
        ├── f33d3r.js       SSE, compose modal, video player init
        └── video-player.js HLS player with IntersectionObserver memory lifecycle
```

## Role badges

| `users.role` | Badge shown on profile | Admin panel access |
|---|---|---|
| `user` | Realm badge only | ✗ |
| `creator` | ✦ Creator (purple) | ✗ |
| `admin` | ⚙ Admin (red) | ✓ |
| `founder` | ★ Founder (gold) | ✓ |

## Dev commands

```bash
# Run outside Docker (fastest iteration)
cd feed-engine && make run
cd feed-engine && make run-dev   # DEV_MODE=true SHOW_SCORES=true

# Rebuild container
docker compose -f ../docker-compose.local.yml build feed-engine
```
