# F33D3R — Local Dev Guide

## Prerequisites

- **Docker Desktop** (or Docker Engine + Compose v2)
- **~6 GB RAM** available to Docker
- **Ports free:** 5432, 6379, 8079, 8080, 8081, 8082, 8089, 8090, 8092, 8093

No Rust, Go, or any other toolchain required locally — Docker handles all builds.

---

## First start

```bash
bash bootstrap-local.sh
```

First run takes **5–10 minutes** — Rust compiles all services from source inside Docker.
Subsequent starts take ~15–20 seconds (Docker layer cache).

---

## Directory layout

```
f33d3r-local/
├── feed-engine/              ← Nantar (Feed + UI) — Go 1.26.2 + HTMX 2.0.10
├── aethyrrank-engine/        ← AethyrRank (Ranking) — Rust 1.95.0
├── zior-engine/              ← Zior (Music cortex) — Rust 1.95.0
├── aethyr-schema-registry/   ← Schema service (supporting) — Rust 1.95.0
├── ain-soph/                 ← Ain Soph (Wallet) — Rust 1.95.0
├── aethyr-msg/               ← Vovin (Messaging) — Rust 1.95.0
├── elohim-veni/              ← Elohim Veni (PIAL enforcement) — Rust 1.95.0
├── zodacare/                 ← Zodacare (Safety + Moderation) — Rust 1.95.0
├── postgres-init/            ← DB init SQL (creates all 6 databases)
├── docker-compose.local.yml  ← Compose file for local dev
├── bootstrap-local.sh        ← One-command startup
├── health-check-local.sh     ← Health check all services
└── README-LOCAL.md           ← This file
```

---

## Service map

| Brain              | Service dir            | Port | DB              | Status                     |
|--------------------|------------------------|------|-----------------|----------------------------|
| Nantar (Feed + UI) | feed-engine            | 8081 | f33d3r_feed     | ✅ Active — open in browser |
| AethyrRank         | aethyrrank-engine      | 8080 | f33d3r_feed     | ✅ Active                   |
| Zior (Music)       | zior-engine            | 8082 | —               | ✅ Active                   |
| Schema Registry    | aethyr-schema-registry | 8079 | f33d3r_registry | ✅ Active                   |
| Ain Soph (Wallet)  | ain-soph               | 8089 | f33d3r_wallet   | ✅ Active                   |
| Vovin (Messaging)  | aethyr-msg             | 8092 | f33d3r_msg      | ✅ Active                   |
| Elohim Veni (PIAL) | elohim-veni            | 8093 | f33d3r_security | ✅ Active                   |
| Zodacare (Safety)  | zodacare               | 8090 | f33d3r_safety   | ✅ Active                   |
| Postgres           | —                      | 5432 | —               | ✅ Active                   |
| Redis              | —                      | 6379 | —               | ✅ Active                   |

---

## Health check

```bash
bash health-check-local.sh
```

Or check manually:

```bash
curl http://localhost:8081/api/health   # Nantar (feed-engine)
curl http://localhost:8080/health       # AethyrRank
curl http://localhost:8082/health       # Zior
curl http://localhost:8079/health       # Schema Registry
curl http://localhost:8089/health       # Ain Soph
curl http://localhost:8092/health       # Vovin (aethyr-msg)
curl http://localhost:8093/health       # Elohim Veni
curl http://localhost:8090/health       # Zodacare
```

---

## Common commands

```bash
# Start all services
docker compose -f docker-compose.local.yml up -d

# Tail all logs
docker compose -f docker-compose.local.yml logs -f

# Tail one service
docker compose -f docker-compose.local.yml logs -f feed-engine

# Rebuild one service after code change
docker compose -f docker-compose.local.yml build feed-engine
docker compose -f docker-compose.local.yml up -d feed-engine

# Restart a service
docker compose -f docker-compose.local.yml restart zodacare

# Stop everything (keep data)
docker compose -f docker-compose.local.yml down

# Stop and wipe all databases
docker compose -f docker-compose.local.yml down -v
```

---

## Making code changes visible

This repo builds Docker images from source — there is no hot reload.
After editing code in any service:

```bash
# Example: after editing feed-engine Go code
docker compose -f docker-compose.local.yml build feed-engine
docker compose -f docker-compose.local.yml up -d feed-engine

# Example: after editing zodacare Rust code
docker compose -f docker-compose.local.yml build zodacare
docker compose -f docker-compose.local.yml up -d zodacare
```

Service names match directory names:
`feed-engine` · `aethyrrank-engine` · `zior-engine` · `aethyr-schema-registry` · `ain-soph` · `aethyr-msg` ·
`elohim-veni` · `zodacare`

---

## Making your account admin

```bash
docker exec -it f33d3r-local-postgres-1 psql -U f33d3r -d f33d3r_feed
```

```sql
UPDATE users SET role = 'admin' WHERE handle = 'your_handle';
```

Admin accounts see an **Admin** link in the sidebar with platform stats, report queue, and capability management.

---

## Creating missing databases manually

If a brain fails to start with `database "X" does not exist`, the postgres init script only runs on the very first
container start. Create the DB manually:

```bash
# Example: f33d3r_safety was missing
docker exec f33d3r-local-postgres-1 psql -U f33d3r -c "CREATE DATABASE f33d3r_safety;"

# Then restart the affected service
docker compose -f docker-compose.local.yml restart zodacare
```

All databases that should exist:

```
f33d3r_feed · f33d3r_msg · f33d3r_wallet · f33d3r_security · f33d3r_safety · f33d3r_registry
```

---

## AethyrRank fallback mode

If `aethyrrank-engine` is not reachable, the feed uses a fallback ranker (engagement × freshness). You will see this in
logs:

```
[main] AethyrRank UNREACHABLE — fallback ranker active
```

Normal during cold startup. Live ranking resumes automatically once AethyrRank is healthy.

---

## PIAL identity system

Every account gets a PIAL (Persistent Identity + Access Layer) root UUID on signup. This is the cross-brain identity
anchor — think of it as the platform's Apple ID.

- Created in feed-engine (`pial_roots` table in `f33d3r_feed`)
- Enforced by Elohim Veni (`f33d3r_security` DB)
- Used by Vovin as the messaging identity (`X-Vovin-Identity` header)
- Never changes, even if the user's handle or account changes

To check PIAL status for an account:

```bash
curl http://localhost:8093/v1/stats  # Elohim Veni stats
curl "http://localhost:8093/v1/pial/{pial_id}/capabilities"
```

---

## Vovin messaging setup

Vovin uses client-side ECDH-P256 keypairs stored in IndexedDB. Each browser instance maintains its own vault.

- New users are prompted to set up their vault during onboarding (`/onboard/setup`)
- Keys never leave the browser — only the public key is registered with Vovin
- Setting up on a new browser/device generates a new keypair and replaces the old one
- The vault can be protected by device biometric, passphrase, or left unlocked

---

## Troubleshooting

### Docker networking on Arch Linux

```
failed to create endpoint... operation not supported
```

Your kernel was updated since last reboot. Fix: reboot, then bootstrap again.

```bash
[ -d "/usr/lib/modules/$(uname -r)" ] && echo "kernel OK" || echo "REBOOT NEEDED"
sudo reboot
bash bootstrap-local.sh
```

### Service stuck in restart loop

Check logs for the specific error:

```bash
docker compose -f docker-compose.local.yml logs --tail=30 <service-name>
```

Common causes:

- Missing database (see "Creating missing databases" above)
- Port conflict (check `lsof -i :PORT`)
- Environment variable missing in `docker-compose.local.yml`

---

## Planned brains (not yet implemented)

| Brain     | Purpose                                                |
|-----------|--------------------------------------------------------|
| Thessalon | Commerce — creator stores, AET payments, subscriptions |
| Caeor     | Streaming — live video, CDN, broadcast                 |
| Loxion    | Voice Rooms — geo-aware audio spaces                   |
| Astraon   | Creator Dashboard — analytics, revenue insights        |
