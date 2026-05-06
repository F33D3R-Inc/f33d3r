# F33D3R — Project State

**Live snapshot of the platform. Update whenever a brain's state changes.**

Last updated: 3 May 2026 (Session 002)

---

## Production target

| Field           | Value                                          |
|-----------------|------------------------------------------------|
| Domain          | `f33d3r.com`                                   |
| Production IP   | 37.27.246.218 (Hetzner Falkenstein)            |
| Server          | Hetzner CCX23 — 4 vCPU, 16 GB RAM, 160 GB disk |
| Egress quota    | 20 TB/month                                    |
| Region          | EU (Falkenstein, Germany) — single            |
| Tier            | **T0** (see SPRINT_LOG.md staircase)            |

Local dev: `bash bootstrap-local.sh` → all brains via Docker Compose on the developer box. Production currently runs the same Compose stack (per Q-007 — to confirm).

---

## Brain status

Legend: 🟢 production-ready · 🟡 needs work · 🔴 ship-blocker · ⚪ planned, not built

| Brain         | Port | DB             | Status | Notes |
|---------------|------|----------------|--------|-------|
| Nantar        | 8081 | f33d3r_feed    | 🟡 | Edge brain. NSFW gating cosmetic. Dwell-time signals not captured. |
| AethyrRank    | 8080 | f33d3r_feed    | 🟡 | 7-stage pipeline real. Neural stage is heuristic stub. Per-surface (not per-user) LinUCB. |
| Caeor         | 8086 | —              | 🔴 | Images good (WebP derivatives). Video pass-through only. No upload hash. No PhotoDNA. |
| Vovin         | 8092 | f33d3r_msg     | 🔴 | V1 ECDH live path; X3DH + Double Ratchet are dead code. Per-device keys, no master-PIAL key. |
| Zior          | 8082 | —              | 🟡 | Audio analysis excellent. Playback UX missing (no queue, no Media Session API). |
| Ain Soph      | 8089 | f33d3r_wallet  | 🟡 | Double-entry correct. READ COMMITTED isolation. No AML/KYT call. |
| Aethyr Ledger | 8096 | f33d3r_ledger  | 🔴 | 2-second sealer present but not synced with Ain Soph. `events_root` is a hash-fold, not a Merkle tree. |
| Thessalon     | 8084 | f33d3r_commerce| 🔴 | No own-path idempotency. No Stripe. No CCBill / Segpay (adult rail). |
| Verity        | 8095 | f33d3r_verity  | 🟢 | Decision-hash design is exemplary. CSAM endpoint is a stub but architecturally correct. |
| Elohim Veni   | 8093 | f33d3r_security| 🟢 | Cleanest code in the repo. Deterministic 10-rule ladder. |
| Zodacare      | 8090 | f33d3r_safety  | 🟢 | Advisory, small, correct. |
| Schema Reg    | 8079 | f33d3r_registry| 🟢 | Versioning works. |
| Registrar     | 8094 | f33d3r_handles | 🟢 | Handle ownership. |
| eKYC          | 8099 | —              | 🟡 | Python service, scaffolded. Decision: orchestrate a commercial KYC vendor under it. |
| Postal (mail) | 8098 | f33d3r_mail    | ⚪ | Specced. Not yet deployed. Sprint 2 candidate. |
| PRAXIS        | TBD  | f33d3r_praxis  | ⚪ | Specced. P1. Sprint 4+ candidate. Will write into Aethyr Ledger as event type `media.*`. |
| Enochain      | 8097 | TBD            | ⚪ | Specced (per user diagram). LLM/reasoning brain. Out of Sprint 1–3 scope. |
| Loxion        | TBD  | —              | ⚪ | Voice rooms / WebRTC. Out of Sprint 1–3 scope. |
| Astraon       | 8088 | f33d3r_analytics| ⚪ | **Dedicated creator analytics brain.** Serves *every* monetizing creator (SFW + adult). Sprint 4 candidate — see Astraon scope below. |
| transcoder    | 8098 (claimed) | — | 🔴 | Service slot exists but never invoked from upload path. Conflicts with Postal's claimed port — to deconflict in Sprint 2. |

Port collision flagged: transcoder and Postal both want 8098 in different specs. Resolve before deploying either. Recommendation: transcoder moves to 8085, Postal stays 8098.

---

## Sprint 0 in flight (resume here)

### S0.1 — Compose foundation services (in progress)

**Files in scope:**
- `docker-compose.local.yml` — append new services: `prometheus`, `loki`, `grafana`, `alertmanager`, `minio`, `pgbouncer`, `transcoder` (slot). New named volumes for each.
- `infra/observability/prometheus.yml` (new) — scrape targets for all 14 brains on `/metrics`.
- `infra/observability/loki-config.yml` (new) — local filesystem chunks; promtail not needed (each brain writes JSON directly via Loki HTTP push).
- `infra/observability/grafana/datasources.yml` + `infra/observability/grafana/dashboards/*.json` (new) — provisioned at boot.
- `infra/observability/alertmanager.yml` (new) — Discord webhook receiver, email fallback.
- `infra/pgbouncer/pgbouncer.ini` + `infra/pgbouncer/userlist.txt` (new) — transaction pooling, 70-conn limit.
- `infra/minio/init-buckets.sh` (new) — creates `media-raw`, `media-derived`, `backups` on first boot.
- `transcoder/Cargo.toml` + `transcoder/src/main.rs` + `transcoder/docker/Dockerfile` (new scaffold) — Rust + ffmpeg, listens on 8085, consumes `media.uploaded` Kafka topic. Implementation lands in S0.5; this PR scaffolds the service so Compose can boot it as a stub.

**Acceptance test:**
```bash
bash bootstrap-local.sh
# expected: all existing 14 brains + 7 new foundation services boot healthy

curl http://localhost:9090/api/v1/targets | jq '.data.activeTargets | length'
# expected: 14+ (every brain scraped)

open http://localhost:3000  # Grafana, default admin/admin
# expected: F33D3R Overview dashboard already provisioned

curl http://localhost:9001  # MinIO console
# expected: 3 buckets pre-created
```

### S1.1 — Server-side NSFW URL gating (deferred to Sprint 1)

**Files in scope:**
- `feed-engine/internal/handler/media.go` — new `serveMedia` handler with PIAL capability check.
- `feed-engine/internal/handler/handlers.go` — register `/media/*` route, deprecate `/static/media/*` direct serving.
- `feed-engine/internal/db/queries.go` — `GetPostByMediaURL()` for capability lookup, `IsMediaNSFW()`.
- `feed-engine/internal/url/signed.go` (new) — HMAC-SHA256 token mint + verify, 5-min TTL.
- `feed-engine/web/templates/feed_items.html` — change `<img src=…>` from raw URL to signed-URL helper.
- `feed-engine/internal/handler/helpers.go` — template func `mediaURL(rawURL, user)` that mints a token.

**Design (decided this session, D-005):**
1. Media URLs in DB stay as `/static/media/posts/{uuid}.{ext}`. No DB migration.
2. Templates render via a new helper `mediaURL` that returns either:
   - the raw URL (if media is not NSFW) — fast path, browser-cacheable
   - a signed URL `/media/{post_id}/{filename}?t={expires}&s={hmac}` (if NSFW) — gated path
3. The `/media/...` handler:
   - parses post_id, looks up `posts.is_nsfw` and `posts.author_id`
   - if not NSFW → 301 to raw URL (shouldn't happen but harmless)
   - if NSFW → check requester PIAL has `CapNSFWAccess` (Verity tier ≥ 2)
   - if check passes → verify HMAC, verify not expired, serve file via `http.ServeFile`
   - if check fails → 403 with body `{"error":"nsfw_gate_blocked","reason":"verify_age"}` and a link to `/kyc`
4. The signed URL is minted server-side at template-render time, so non-tier-2 users never see a working URL in HTML.
5. CSP unchanged. Cache-Control on `/media/*` paths: `private, max-age=300, must-revalidate`. Cache-Control on `/static/media/*` (legacy non-NSFW): unchanged at `public, max-age=31536000, immutable`.

**Acceptance test:**
```bash
# As @guest (no Verity tier 2):
curl -s -o /dev/null -w "%{http_code}" "http://localhost:8081/media/{nsfw_post_id}/{filename}?t=…&s=…"
# expected: 403

# As @creator (with Verity tier 2 + valid token):
# expected: 200 with image bytes

# Forge a token (wrong HMAC):
# expected: 403
```

### S1.2 — CSAM pipeline correction

**Files in scope:**
- `feed-engine/internal/handler/compliance.go` — split `csamScan` into:
  - `classifyAdultContent(mediaURL)` returns `{is_adult bool, score float}` — used to suggest `is_nsfw=true` only.
  - `scanCSAM(contentHash)` calls Verity `/v1/compliance/csam/scan` only.
  - **Remove the adult-classifier-as-CSAM enforcement** at line 388.
- `feed-engine/internal/handler/post.go` — on upload, compute `sha256(file_bytes)` and call `scanCSAM(hash)` before insert. If flagged → 451 + enforcement cascade. If clean → store `is_nsfw` from creator's flag (or auto-set if classifier says so, but never enforce).
- `feed-engine/internal/handler/media.go` (`uploadPostMedia`) — pass content hash to caller.
- `verity/src/main.rs` — keep `/v1/compliance/csam/scan` stub, but add a `match_count` aware return; later wire NCMEC PhotoDNA when the partnership is signed.
- `caeor/src/handler.rs` — already returns the saved file path; add `content_hash` to the response.

**Acceptance test:**
```bash
# Upload a legitimate adult image (high adult-classifier score, hash never seen):
# expected: post created, is_nsfw=true auto-flagged, NO csamEnforce, NO law_enforcement_queue insert, NO PIAL termination.

# Upload a known-bad hash (manually inserted into csam_scans with result='flagged'):
# expected: 451 Unavailable For Legal Reasons, csamEnforce runs, PIAL terminated, NCMEC queue insert.
```

### S1.3 — Vovin master-PIAL key foundation (phase 1 of P0-3)

**Files in scope:**
- `feed-engine/web/static/js/pages/messages.js` — add `deriveDeviceKeyFromMaster(masterKey, deviceId)` using HKDF-SHA256 over the master ECDH key. Existing `setupVault()` becomes `setupMasterVault()` and produces a 12-word recovery mnemonic (BIP39 wordlist, 128-bit entropy). Existing identity registration becomes per-device-derived-key registration with the master pub key as a parent.
- `aethyr-msg/src/db.rs` — add `master_keys` table: `pial_id PRIMARY KEY, master_pub_b64 TEXT, signed_at TIMESTAMPTZ`. Existing `identities` rows become device leaves with new column `parent_master_pub_b64`.
- `aethyr-msg/src/handlers.rs` — new `POST /v1/identity/master` accepts `{pial_id, master_pub_b64, signature}`. Existing device registration endpoint validates the device key is signed by the master (Ed25519 over `device_id|device_pub`).
- Migration: `aethyr-msg/migrations/20260503_master_keys.sql`.

This is **only the foundation**. Full Double Ratchet wiring (P0-4) and ratchet state sync across devices stay in Sprint 2.

**Acceptance test:**
```js
// In browser console, after Sprint 1.3 ships:
const m = await F33D3R.messaging.exportRecoveryMnemonic();
// expected: 12 space-separated words, BIP39-valid

// On a new browser, paste the mnemonic:
await F33D3R.messaging.importMaster(m);
// expected: master key restored, device-key derived from master, /vovin/v1/identity/master receives matching pub key, no "encrypted for another session" error on subsequent message decrypt for messages encrypted to the master.
```

---

## Astraon — creator analytics brain (scope spec)

Pinned in Session 003 by D-016. Astraon is its own brain, not folded into Nantar or AethyrRank.

**Why a separate brain.** Analytics is a fundamentally different workload than ranking or feed-rendering — long-running aggregation queries, per-creator dashboards, time-series rollups, export jobs. Mixing it into Nantar would slow page renders. Mixing it into AethyrRank would pollute the ranker's hot-path budget. It deserves its own DB, its own scaling profile, its own SLOs.

**Audience:** every PIAL with `MONETIZATION` capability granted. Free creators, SFW creators, adult creators, musicians on Zior, all see Astraon dashboards. Tier 0 viewers see nothing from Astraon (no analytics for non-creators).

**Data flow.** Astraon **never reads another brain's DB**. It consumes Kafka topics and builds its own derived analytics tables in `f33d3r_analytics`:

| Topic                | Astraon consumer purpose                                        |
|----------------------|-----------------------------------------------------------------|
| `content.events`     | Posts published, post performance over time                     |
| `ranking.events`     | Reach, exploration boost, rank distribution                     |
| `feed.events`        | Impressions, dwell-time histograms (after S0.2/Sprint 1)        |
| `economy.events`     | Tip volume, subscription MRR, PPV revenue, withdrawal cadence   |
| `messaging.events`   | DM volume metadata only — never content                         |
| `safety.events`      | Strike count, removal rate (so creators see their own record)   |
| `media.events`       | Upload count, transcode failures, storage usage                 |

**Endpoints (Sprint 4 spec, not yet built):**
- `GET /v1/creator/:pial_id/dashboard` — top-level KPIs (last 7/30/90 days)
- `GET /v1/creator/:pial_id/audience` — follower growth, geo distribution, retention cohorts
- `GET /v1/creator/:pial_id/earnings` — gross / net revenue by stream (tips, subs, PPV), payout cadence
- `GET /v1/creator/:pial_id/content/:content_id/performance` — per-post views, watch-time, conversions
- `GET /v1/creator/:pial_id/funnel` — view → like → follow → tip → subscribe → renew
- `GET /v1/creator/:pial_id/export` — CSV / JSON dump for taxes, off-platform analysis
- `POST /v1/admin/cohort` — admin-only cohort builder

**Forbidden:** reading `f33d3r_feed`, `f33d3r_wallet`, `f33d3r_commerce`, or any other brain's DB directly. Any "join" Astraon needs happens through its own derived tables, populated from Kafka events.

**SLOs:** dashboard p95 < 800 ms. Aggregations are pre-computed (TimescaleDB-style hypertables on Postgres), not computed on every request.

**Comes online:** Sprint 4 (after the Sprint 0 foundation, Sprint 1 security, Sprint 2 transcoder/PRAXIS hardening, Sprint 3 voice/Vovin completion). Until then, Nantar shows minimal earnings data via direct Thessalon HTTP calls — ugly but contained, marked `// TODO_ASTRAON:` in code so we know what to migrate.

---

## Pinned answers / decisions on record

| ID    | Decision / Pinned answer                                                              | Status     |
|-------|---------------------------------------------------------------------------------------|------------|
| D-001 | Brain model: event-driven Kafka default, HTTP only where documented                  | committed  |
| D-002 | Build in-house eKYC orchestration, buy CV/face-match SDK (= Persona — see D-014)     | committed  |
| D-003 | PRAXIS is P1, not P0. Sprint 4+ candidate.                                            | committed  |
| D-004 | Voice messaging is Sprint 3+                                                          | committed  |
| D-005 | NSFW media uses HMAC-SHA256 signed tokens, 5-min TTL (Sprint 1 work, on top of MinIO) | committed  |
| D-006 | PRAXIS writes into Aethyr Ledger; no second hash chain                                | committed  |
| D-007 | Sprint 0 is the foundation sprint. No new features ship until it's done.              | committed  |
| D-008 | Stay on Docker Compose at T0–T1. K3s only at T2 (multi-node). No K8s today.           | committed  |
| D-009 | No Kotlin in the stack. Go + Rust only.                                               | committed  |
| D-010 | Thessalon uses `PaymentRail` adapter pattern. Stripe + CCBill + Segpay + Epoch as adapters. | committed  |
| D-011 | EU-first launch. Block APAC at edge until T3. Build for GDPR; US-compliance follows.  | committed  |
| D-012 | Ripple = XRPL issued-currency settlement rail. Not token listing.                     | committed  |
| D-013 | NSFW payments = CCBill primary, Segpay fallback. Both behind PaymentRail.             | committed  |
| D-014 | KYC vendor = Persona. Orchestrated behind Verity facade.                              | committed  |
| D-015 | T0→T1 trigger: CPU > 60% sustained 4h OR egress > 14 TB/mo OR p95 > 300 ms for 1h.    | committed  |
| D-016 | Astraon is its own brain. Port 8088. DB `f33d3r_analytics`. Serves every monetizing creator. Reads no other brain's DB — consumes Kafka only. | committed  |
| D-017 | Path normalisation in metrics is regex-based at T0–T1; switch to Go 1.22 ServeMux pattern templates at T2 router refactor. | committed  |
| D-018 | Outbound HTTP latency is its own histogram, separate from inbound. Lets dashboards distinguish "I served slowly" from "my dependency served me slowly". | committed  |
| D-019 | `/metrics`, `/api/health`, `/static/*`, `/cdn/*` excluded from per-request log line. Still increment counters. | committed  |
| D-020 | HLS asset stored as 5 columns on `posts` (master/poster/duration/w/h), not a sibling table.   | committed  |
| D-021 | hls.js loaded lazily from jsDelivr at T0–T1; self-host at T2.                                 | committed  |
| D-022 | video-player.js owns hls.js lifecycle exclusively. Disposal via MutationObserver.             | committed  |
| D-023 | Feed videos = muted-autoplay by default. Sound is opt-in via native controls (X/TikTok feel). | committed  |
| D-024 | Caeor synchronous transcode poll at T0 (5-min cap). Move to Kafka `media.transcoded` in Sprint 1. | committed  |
| D-025 | Page-specific JS exposing handlers to inline `onclick=` MUST use `window.X = function...`. Local `function X()` is forbidden. | committed  |
| **D-026** | **MUST-HAVE: NEVER hand-pick versions of languages, runtimes, or Docker images. ALWAYS use `:latest` (or unversioned). Caused a build failure on bootstrap-local.sh — fixed only when user set every image to `:latest`.** | **committed**  |
| D-027 | Every brain Dockerfile sits next to a `.dockerignore` excluding build outputs (`target/`, `feed-engine` binary, `vendor/`). Mandatory — without it BuildKit chokes on multi-GB context.                                          | committed  |
| D-028 | Empty / loading / error UI uses shared `_state.html` partials (`stateEmpty`, `stateLoading`, `stateError`). New pages MUST use them; old pages migrate opportunistically.                                                            | committed  |
| D-029 | Bootstrap script is OS-tolerant. Linux-only checks are warn-only, never block. Docker is the single hard requirement.                                                                                                              | committed  |
| D-030 | Every brain Dockerfile must be self-healing on dep manifests. Go: `go mod tidy && go build` post-COPY. Rust: dummy-build pattern. Pre-fetch caching is a perf optimisation, never the correctness path.                          | committed  |
| Q-007 | What's deployed at f33d3r.com right now? Audit in Sprint 0 W1.                        | open       |

---

## Files I will read at the start of every session

1. `SPRINT_LOG.md` § CURRENT SPRINT + most recent SESSION LOG entry.
2. This file (`PROJECT_STATE.md`) § Sprint in flight.
3. `AUDIT_2026_05.md` if I need strategic baseline.
4. The actual source files for the in-flight work.

If a session ends mid-PR, the SESSION LOG entry's "Stopped at" line names the file + line number to resume from.
