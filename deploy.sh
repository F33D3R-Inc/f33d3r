#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# F33D3R Deploy — Hetzner CCX33 (8 vCPU / 32 GB RAM / 150 GB NVMe)
#
# Usage:
#   bash deploy.sh --reload     — feed-engine RESTART only (~8s): template (.html) changes
#   bash deploy.sh --ui         — REBUILD feed-engine image (~90s): Go code changes
#   bash deploy.sh --service X  — rebuild + restart one service by compose name
#   bash deploy.sh --check      — health check only, no restart
#   bash deploy.sh --pull-base  — pull fresh base images only (run occasionally)
#   bash deploy.sh --all        — full rebuild + restart of ALL services (slow)
#   bash deploy.sh              — same as --all, but prompts before the full rebuild
#
# After a `git pull`, pick the lightest option:
#   • CSS / JS only        → nothing needed (web/static is a live bind mount; bump ?v=)
#   • templates (.html)    → bash deploy.sh --reload   (bind-mounted :ro, reparsed on restart)
#   • Go code changed      → bash deploy.sh --ui       (image rebuild required)
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

cd "$(dirname "$(realpath "$0")")"

COMPOSE="docker compose -f docker-compose.prod.yml --env-file .env.prod"

# Services are built ONE AT A TIME (sequential) to prevent multiple Rust linkers
# from exhausting the 150 GB disk with temp artifacts.
# Ordered: fast Go services first, heavy Rust/Python last.
BUILD_ORDER=(
  feed-engine
  help-center
  tor
  mediamtx
  aethyrrank-engine
  manhattan
  elohim-veni
  ain-soph
  verity
  aethyr-ledger
  aethyr-schema-registry
  caeor
  zodacare
  zior-engine
  registry-brain
  astraon
  alexandria
  herald
  lore
  auralis
  themis
  abraxas-shield
  transcoding
  ekyc
  content-scan
)
# NOTE: image-based services (sitra-achra/Redpanda, postgres, redis, caddy,
# live-edge, libretranslate) are intentionally NOT in BUILD_ORDER — they have
# no build: config and `compose build` on them warns "No services to build".
# They are started by `compose up -d` like everything else.

# Refuse to start a build if free disk is below this threshold.
# A single Rust build can use up to 5 GB of temp space.
MIN_DISK_GB=20

GREEN="\033[0;32m"; YELLOW="\033[1;33m"; RED="\033[0;31m"; BOLD="\033[1m"; NC="\033[0m"
info()    { echo -e "${GREEN}[deploy]${NC} $*"; }
warn()    { echo -e "${YELLOW}[deploy]${NC} $*"; }
err()     { echo -e "${RED}[deploy]${NC} $*" >&2; }
section() { echo -e "\n${BOLD}── $* ──${NC}"; }

# ── Disk guard ────────────────────────────────────────────────────────────────
check_disk() {
  local free_gb
  free_gb=$(df / --output=avail -BG | tail -1 | tr -d 'G ')
  if [ "$free_gb" -lt "$MIN_DISK_GB" ]; then
    err "Only ${free_gb}GB free on /. Minimum ${MIN_DISK_GB}GB required."
    err "Free space first:"
    err "  docker image prune -f"
    err "  docker builder prune -f"
    exit 1
  fi
  info "Disk OK — ${free_gb}GB free"
}

# ── Post-build cleanup ────────────────────────────────────────────────────────
# Clears both dangling image layers AND the BuildKit cache.
# BuildKit cache is the main cause of disk exhaustion — Rust services each
# leave 2–5 GB of incremental compile artifacts that image prune never touches.
prune_dangling() {
  info "Pruning dangling layers + BuildKit cache..."
  docker image prune -f 2>/dev/null || true
  docker builder prune -f 2>/dev/null || true
}

# ── Stale lock cleanup ────────────────────────────────────────────────────────
# Redpanda writes a pid.lock into its data volume. If the container was killed
# hard (e.g. containerd wipe, power loss) the lock is never released and the
# next start fails with "failed to lock pidfile. already locked".
# This runs before every `docker compose up` to clear it.
clear_stale_locks() {
  info "Clearing stale pid locks..."
  docker run --rm -v f33d3r_sitra_achra_data:/data alpine \
    sh -c "rm -f /data/pid.lock /data/.lock" 2>/dev/null || true
}

# ── Gnosis messaging E2EE core ───────────────────────────────────────────────
# The messaging crypto core is the `sealcore` crate, pre-compiled and committed
# as static assets at feed-engine/web/static/js/sealcore/ (sealcore.js +
# sealcore_bg.wasm). gnosis-seal.js imports /static/js/sealcore/sealcore.js and
# the assets are served live through the ./feed-engine/web/static bind mount —
# no build-time copy step is required. (The retired gnosis-malkuth image-sync
# step lived here; it was removed when the core moved to committed assets.)

# ── Service registry ─────────────────────────────────────────────────────────
# Format: "display-name|compose-service-name|critical(1/0)"
# critical=1 → deploy exits non-zero if service is down after startup.
# One entry per service in docker-compose.prod.yml — keep the two in step.
SERVICES=(
  "Postgres              |postgres               |1"
  "Redis                 |redis                  |1"
  "Caddy (edge)          |caddy                  |1"
  "Feed Engine (Nantar)  |feed-engine            |1"
  "AethyrRank            |aethyrrank-engine      |1"
  "Caeor (media)         |caeor                  |1"
  "Manhattan (naming)    |manhattan              |1"
  "Elohim Veni (PIAL)    |elohim-veni            |1"
  "Ain Soph (wallet)     |ain-soph               |1"
  "Verity                |verity                 |0"
  "Aethyr Ledger         |aethyr-ledger          |0"
  "Abraxas Shield (scan) |abraxas-shield         |0"
  "Content Scan          |content-scan           |0"
  "Schema Registry       |aethyr-schema-registry |0"
  "Zodacare (safety)     |zodacare               |0"
  "Zior Engine           |zior-engine            |0"
  "Registry Brain        |registry-brain         |1"
  "Astraon (discovery)   |astraon                |0"
  "Alexandria (library)  |alexandria             |0"
  "Herald (push)         |herald                 |0"
  "Lore (notifications)  |lore                   |0"
  "Auralis (frequencies) |auralis                |0"
  "Themis (commerce)     |themis                 |0"
  "eKYC                  |ekyc                   |0"
  "Transcoding           |transcoding            |1"
  "MediaMTX (live ingest)|mediamtx               |0"
  "Live Edge (HLS)       |live-edge              |0"
  "LibreTranslate        |libretranslate         |0"
  "Sitra Achra           |sitra-achra            |0"
  "Help Center           |help-center            |0"
  "Tor                   |tor                    |0"
)

# ── Health check ──────────────────────────────────────────────────────────────
PASS=0; FAIL=0; WARN=0
check_service() {
  local label="$1" svc="$2" critical="$3"
  if $COMPOSE ps --status running "$svc" 2>/dev/null | grep -q "$svc"; then
    printf "  ${GREEN}●${NC} %-30s running\n" "$label"
    PASS=$((PASS+1))
  else
    if [ "$critical" = "1" ]; then
      printf "  ${RED}○${NC} %-30s NOT RUNNING  ← CRITICAL\n" "$label"
      FAIL=$((FAIL+1))
    else
      printf "  ${YELLOW}○${NC} %-30s not running\n" "$label"
      WARN=$((WARN+1))
    fi
  fi
}

run_health_check() {
  PASS=0; FAIL=0; WARN=0
  for entry in "${SERVICES[@]}"; do
    IFS='|' read -r label svc critical <<< "$entry"
    check_service "${label// /}" "${svc// /}" "${critical// /}"
  done
  echo ""
  if [ "$FAIL" -gt 0 ]; then
    err "$FAIL critical service(s) not running."
    err "  $COMPOSE logs --tail=80 <service>"
    return 1
  else
    info "${PASS} services running${WARN:+, ${WARN} optional offline}"
    return 0
  fi
}

# ── --check mode ──────────────────────────────────────────────────────────────
if [[ "${1:-}" == "--check" ]]; then
  section "F33D3R health check"
  run_health_check
  exit $?
fi

# ── --pull-base mode: update base images only ─────────────────────────────────
# Run this manually when you want fresh base images (new Go/Rust/Python releases).
# Do NOT run on every deploy — it invalidates the build cache and wastes disk.
if [[ "${1:-}" == "--pull-base" ]]; then
  section "Pulling fresh base images (sequential)"
  for svc in "${BUILD_ORDER[@]}"; do
    check_disk
    info "Pulling base for $svc..."
    $COMPOSE build --pull --no-cache "$svc" || warn "Skipped $svc (no build config or failed)"
    docker image prune -f 2>/dev/null || true
  done
  info "Base images updated. Run bash deploy.sh to bring services up."
  exit 0
fi

# ── --reload mode: restart feed-engine, NO rebuild ────────────────────────────
# For template (.html) changes after a `git pull`. web/templates is bind-mounted
# read-only and parsed at boot, so a restart is enough — no image rebuild.
# (CSS/JS live in the web/static bind mount and need nothing but a browser cache
# bust; this restart picks them up too, harmlessly.)
if [[ "${1:-}" == "--reload" ]]; then
  section "Reload — restarting feed-engine (no rebuild)"
  $COMPOSE restart feed-engine
  sleep 8

  if $COMPOSE ps --status running feed-engine 2>/dev/null | grep -q "feed-engine"; then
    info "feed-engine restarted — templates reparsed, static assets live."
    info "https://f33d3r.com"
  else
    err "feed-engine failed to start. Logs:"
    $COMPOSE logs --tail=60 feed-engine
    exit 1
  fi
  exit 0
fi

# ── --ui mode: rebuild feed-engine image ──────────────────────────────────────
# Use ONLY when Go code changed. For template/CSS/JS-only changes use --reload
# (or nothing for CSS/JS) — those are bind-mounted live and do not need a rebuild.
# Does not pull base images — uses the cached golang:alpine layer.
if [[ "${1:-}" == "--ui" ]]; then
  check_disk
  section "UI deploy — rebuilding feed-engine image (Go code)"
  $COMPOSE build feed-engine

  section "Restarting feed-engine"
  $COMPOSE up -d --no-deps feed-engine
  sleep 8

  if $COMPOSE ps --status running feed-engine 2>/dev/null | grep -q "feed-engine"; then
    info "feed-engine live — new binary, templates, CSS, JS reloaded."
    info "https://f33d3r.com"
  else
    err "feed-engine failed to start. Logs:"
    $COMPOSE logs --tail=60 feed-engine
    exit 1
  fi
  exit 0
fi

# ── --service mode: rebuild a single service ──────────────────────────────────
# Example: bash deploy.sh --service caeor
if [[ "${1:-}" == "--service" ]]; then
  SVC="${2:-}"
  if [ -z "$SVC" ]; then
    err "Usage: bash deploy.sh --service <compose-service-name>"
    exit 1
  fi
  check_disk
  section "Rebuilding service: $SVC"
  $COMPOSE build "$SVC"
  clear_stale_locks
  $COMPOSE up -d --no-deps "$SVC"
  sleep 5
  info "$SVC restarted."
  exit 0
fi

# ── Unknown flag guard ────────────────────────────────────────────────────────
# Anything other than the handled flags (or no arg / --all) is almost certainly a
# typo — fail loudly instead of silently doing a full every-service rebuild.
if [[ -n "${1:-}" && "${1}" != "--all" ]]; then
  err "Unknown option: ${1}"
  err "Run with no flag (or --all) for a full rebuild, or one of:"
  err "  --reload  --ui  --service <name>  --check  --pull-base"
  exit 1
fi

# ── Full deploy ───────────────────────────────────────────────────────────────
# Builds each service ONE AT A TIME to prevent disk exhaustion from parallel
# Rust linker temp files. Prunes dangling layers between each build.
# This rebuilds ALL services and is SLOW — confirm it was intentional.
warn "Full rebuild of ALL services requested (this is slow)."
warn "For a UI/code change you usually want:  --reload (templates)  or  --ui (Go)."
if [[ -t 0 ]]; then
  read -r -p "$(echo -e "${YELLOW}[deploy]${NC} Proceed with FULL rebuild? [y/N] ")" _ans
  [[ "${_ans}" =~ ^[Yy]$ ]] || { info "Aborted. Use --reload / --ui for UI changes."; exit 0; }
fi
check_disk

section "Building all images (sequential — one at a time)"
for svc in "${BUILD_ORDER[@]}"; do
  check_disk
  info "Building $svc..."
  $COMPOSE build "$svc" || { err "Failed to build $svc"; exit 1; }
  docker image prune -f 2>/dev/null || true
done

section "Bringing all services up"
clear_stale_locks
$COMPOSE up -d

section "Pruning dangling layers"
prune_dangling


section "Waiting for services to stabilise (20s)"
sleep 20

# Second up-pass: picks up any containers left in Created state because their
# dependency (e.g. sitra-achra) was still starting during the first up-pass.
section "Second pass — starting any dependency-blocked containers"
$COMPOSE up -d

sleep 10

section "Health check"
# `run_health_check` returns non-zero when a critical service is down. Under
# `set -e` a bare call would exit before we capture $?, skipping the summary
# below — so guard it with `|| STATUS=$?`.
STATUS=0
run_health_check || STATUS=$?

echo ""
if [ "$STATUS" -eq 0 ]; then
  info "Deploy complete. https://f33d3r.com"
  info "Template change:     bash deploy.sh --reload"
  info "Go code change:      bash deploy.sh --ui"
  info "Single service:      bash deploy.sh --service <name>"
  info "Health check only:   bash deploy.sh --check"
  info "Update base images:  bash deploy.sh --pull-base"
else
  err "Deploy finished but $FAIL critical service(s) are down."
  err "Run: $COMPOSE logs --tail=80 <service>"
  exit 1
fi
