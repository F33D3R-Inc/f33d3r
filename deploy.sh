#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# F33D3R Deploy — Hetzner CCX33 (8 vCPU / 32 GB RAM / 150 GB NVMe)
#
# Usage:
#   bash deploy.sh --reload     — feed-engine RESTART only (~8s): template (.html) changes
#   bash deploy.sh --ui         — REBUILD feed-engine image (~90s): Go code changes
#   bash deploy.sh --service X  — rebuild + restart one service by compose name
#   bash deploy.sh --check      — health check only, no restart
#   bash deploy.sh --pull-base  — rebuild images against freshly pulled base images
#   bash deploy.sh --all        — backup, then full rebuild + restart of ALL services
#   bash deploy.sh              — same as --all, but prompts before the full rebuild
#   bash deploy.sh --bootstrap  — first install only: build + start on an empty host
#
# After a `git pull`, pick the lightest option:
#   • CSS / JS only        → nothing needed (web/static is a live bind mount; bump ?v=)
#   • templates (.html)    → bash deploy.sh --reload   (bind-mounted :ro, reparsed on restart)
#   • Go code changed      → bash deploy.sh --ui       (image rebuild required)
#
# Every mode that starts something ends in the health check, and the health
# check is HTTP, not "the container exists":
#   • every service with a compose healthcheck must report healthy — those
#     healthchecks are each service's own /health (or /ready, /api/health,
#     /healthz) probed over HTTP inside its container, so the per-service probe
#     is defined once, next to the service, and not restated here;
#   • services with no healthcheck must be running; one-shot init services must
#     have exited 0;
#   • and https://f33d3r.com/api/health must answer 200 through Caddy on this
#     host, which proves the edge, TLS and feed-engine together.
#
# --all takes a backup first and fails if it cannot: every database
# (pg_dump -Fc each, plus roles) and the image every service is running now,
# tagged f33d3r-rollback/<service>:<stamp>. It prints the exact rollback
# command. scripts/rollback.sh restores both.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

cd "$(dirname "$(realpath "$0")")"

[ -f .env.prod ] || { echo "[deploy] .env.prod not found in $(pwd)" >&2; exit 1; }
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
  caeor
  zior-engine
  herald
  auralis
  themis
  abraxas-shield
  transcoding
  ekyc
  content-scan
)
# NOTE: image-based services (sitra-achra/Redpanda, postgres, redis, caddy,
# live-edge, libretranslate, backup) are intentionally NOT in BUILD_ORDER —
# they have no build: config. They are started by `compose up -d` like
# everything else.

# Refuse to start a build if free disk is below this threshold.
# A single Rust build can use up to 5 GB of temp space.
MIN_DISK_GB=20

# How long services get to become healthy. content-scan's model load and
# libretranslate's language packs are the slow ones (start_period 180s/120s).
HEALTH_TIMEOUT_SECONDS="${HEALTH_TIMEOUT_SECONDS:-600}"

# Pre-deploy backup sets (and their rollback image tags) kept.
PREDEPLOY_KEEP="${PREDEPLOY_KEEP:-3}"

SITE="f33d3r.com"

GREEN="\033[0;32m"; YELLOW="\033[1;33m"; RED="\033[0;31m"; BOLD="\033[1m"; NC="\033[0m"
info()    { echo -e "${GREEN}[deploy]${NC} $*"; }
warn()    { echo -e "${YELLOW}[deploy]${NC} $*"; }
err()     { echo -e "${RED}[deploy]${NC} $*" >&2; }
section() { echo -e "\n${BOLD}── $* ──${NC}"; }

command -v jq >/dev/null || { err "jq is required (apt-get install jq)"; exit 1; }
command -v curl >/dev/null || { err "curl is required (apt-get install curl)"; exit 1; }

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
# The images a rollback needs are tagged f33d3r-rollback/*, so they are not
# dangling and this never removes them.
prune_dangling() {
  info "Pruning dangling layers + BuildKit cache..."
  docker image prune -f
  docker builder prune -f
}

# ── Stale Redpanda locks ──────────────────────────────────────────────────────
# Redpanda writes a pid.lock into its data volume. If the container was killed
# hard (containerd wipe, power loss) the lock is never released and the next
# start fails with "failed to lock pidfile. already locked". A lock is only
# stale when its broker is not running, so only a stopped broker's lock is
# removed — never a running one's. The broker's own image does the removal.
clear_stale_locks() {
  local svc cid state
  for svc in sitra-achra sitra-achra-2 sitra-achra-3; do
    cid="$($COMPOSE ps -a -q "$svc" | head -n1)"
    if [ -n "$cid" ]; then
      state="$(docker inspect -f '{{.State.Status}}' "$cid")"
      [ "$state" = "running" ] && continue
    fi
    info "Clearing a stale pid lock for $svc (not running)"
    $COMPOSE run --rm --no-deps -T --entrypoint /bin/sh "$svc" \
      -c 'rm -f /var/lib/redpanda/data/pid.lock /var/lib/redpanda/data/.lock'
  done
}

# ── Health check ──────────────────────────────────────────────────────────────
# One line per service in the compose file, read from it — there is no second
# list of services here to drift from it.
service_state() {
  # prints: status|health|restart-policy|exit-code, or "absent"
  local cid
  cid="$($COMPOSE ps -a -q "$1" | head -n1)"
  if [ -z "$cid" ]; then echo "absent"; return; fi
  docker inspect -f '{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}|{{.HostConfig.RestartPolicy.Name}}|{{.State.ExitCode}}' "$cid"
}

# service_ok SVC — 0 healthy, 1 not yet, 2 failed for good.
service_ok() {
  local svc="$1" st status health policy code
  st="$(service_state "$svc")"
  [ "$st" = "absent" ] && return 1
  IFS='|' read -r status health policy code <<< "$st"
  # One-shot init services (restart "no") are done when they exited 0.
  if [ "$policy" = "no" ] || [ -z "$policy" ]; then
    if [ "$status" = "exited" ]; then
      [ "$code" = "0" ] && return 0 || return 2
    fi
  fi
  [ "$status" = "running" ] || return 1
  # The backup service's health is the age of its last success, which is a
  # schedule, not readiness: while its first run is in its start period it is
  # "starting" and that is fine; "unhealthy" means the nightly backup is
  # failing, and that fails the deploy.
  if [ "$svc" = "backup" ]; then
    [ "$health" = "unhealthy" ] && return 2
    return 0
  fi
  case "$health" in
    none|healthy) return 0 ;;
    unhealthy)    return 2 ;;
    *)            return 1 ;;
  esac
}

edge_ok() {
  curl -fsS -o /dev/null --max-time 10 --resolve "${SITE}:443:127.0.0.1" "https://${SITE}/api/health"
}

# wait_healthy [SVC...] — every named service (default: all) healthy and the
# edge answering, within HEALTH_TIMEOUT_SECONDS.
wait_healthy() {
  local services=("$@") deadline svc rc pending failed
  if [ "${#services[@]}" -eq 0 ]; then
    mapfile -t services < <($COMPOSE config --services)
  fi
  deadline=$(( $(date +%s) + HEALTH_TIMEOUT_SECONDS ))
  while :; do
    pending=(); failed=()
    for svc in "${services[@]}"; do
      rc=0; service_ok "$svc" || rc=$?
      case "$rc" in 0) ;; 1) pending+=("$svc") ;; *) failed+=("$svc") ;; esac
    done
    if [ "${#failed[@]}" -eq 0 ] && [ "${#pending[@]}" -eq 0 ] && edge_ok; then
      break
    fi
    if [ "${#failed[@]}" -gt 0 ] || [ "$(date +%s)" -ge "$deadline" ]; then
      break
    fi
    sleep 5
  done
  print_health "${services[@]}"
}

print_health() {
  local svc rc st bad=0
  for svc in "$@"; do
    rc=0; service_ok "$svc" || rc=$?
    st="$(service_state "$svc")"
    if [ "$rc" -eq 0 ]; then
      printf "  ${GREEN}●${NC} %-26s %s\n" "$svc" "$st"
    else
      printf "  ${RED}○${NC} %-26s %s\n" "$svc" "$st"
      bad=$((bad+1))
    fi
  done
  if edge_ok; then
    printf "  ${GREEN}●${NC} %-26s %s\n" "edge" "https://${SITE}/api/health 200 via Caddy"
  else
    printf "  ${RED}○${NC} %-26s %s\n" "edge" "https://${SITE}/api/health did not answer 200 via Caddy"
    bad=$((bad+1))
  fi
  echo ""
  if [ "$bad" -gt 0 ]; then
    err "$bad check(s) failed."
    err "  $COMPOSE logs --tail=80 <service>"
    return 1
  fi
  info "All checks passed."
}

# ── Backups ───────────────────────────────────────────────────────────────────
# The host directory the backup service writes to, read from the compose file
# so this script and the service can never disagree about it.
backup_host_dir() {
  $COMPOSE config --format json \
    | jq -er '.services.backup.volumes[] | select(.target == "/backups") | .source'
}

# backup_run ARGS... — run a command in a one-off backup container.
backup_run() {
  $COMPOSE run --rm --no-deps -T --entrypoint /bin/sh backup "$@"
}

predeploy_backup() {
  local stamp set host svc cid ref id tag rc
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  set="/backups/db/predeploy/${stamp}"
  host="$(backup_host_dir)"

  rc=0; service_ok postgres || rc=$?
  if [ "$rc" -ne 0 ]; then
    err "postgres is not healthy. A full deploy backs up the live database first and"
    err "cannot. Bring postgres up, or for a first install use: bash deploy.sh --bootstrap"
    exit 1
  fi

  section "Backup before deploy — databases"
  backup_run /backup/backup.sh db "$set" \
    || { err "Database backup FAILED. Nothing has been rebuilt."; exit 1; }

  section "Backup before deploy — the images running now"
  local images=""
  while IFS= read -r svc; do
    cid="$($COMPOSE ps -a -q "$svc" | head -n1)"
    if [ -z "$cid" ]; then
      images+="${svc} - - -"$'\n'
      continue
    fi
    ref="$(docker inspect -f '{{.Config.Image}}' "$cid")"
    id="$(docker inspect -f '{{.Image}}' "$cid")"
    tag="f33d3r-rollback/${svc}:${stamp}"
    docker tag "$id" "$tag"
    images+="${svc} ${ref} ${id} ${tag}"$'\n'
    info "  ${svc}: ${ref} → ${tag}"
  done < <($COMPOSE config --services)
  printf '%s' "$images" | backup_run -c 'cat > "$1"' sh "${set}/images.txt" \
    || { err "Recording the running images FAILED. Nothing has been rebuilt."; exit 1; }

  ROLLBACK_SET="${host}/db/predeploy/${stamp}"
  info "Backup complete: ${ROLLBACK_SET}"
  echo ""
  echo -e "${BOLD}  To restore this exact state (databases + images):${NC}"
  echo "    bash scripts/rollback.sh ${ROLLBACK_SET}"
  echo ""
}

# Keep the newest PREDEPLOY_KEEP pre-deploy sets, and the rollback image tags of
# exactly those sets.
prune_predeploy() {
  local kept tag stamp
  backup_run /backup/backup.sh prune predeploy "$PREDEPLOY_KEEP"
  kept="$(backup_run -c 'ls -1 /backups/db/predeploy')"
  while IFS= read -r tag; do
    [ -n "$tag" ] || continue
    stamp="${tag##*:}"
    if ! grep -qxF "$stamp" <<< "$kept"; then
      docker rmi "$tag" >/dev/null
      info "Removed rollback image tag $tag (its backup set aged out)"
    fi
  done < <(docker images --format '{{.Repository}}:{{.Tag}}' | grep '^f33d3r-rollback/' || true)
}

build_all() {
  section "Building all images (sequential — one at a time)"
  local svc
  for svc in "${BUILD_ORDER[@]}"; do
    check_disk
    info "Building $svc..."
    $COMPOSE build "$svc" || { err "Failed to build $svc"; exit 1; }
    docker image prune -f
  done
}

# ── --check mode ──────────────────────────────────────────────────────────────
if [[ "${1:-}" == "--check" ]]; then
  section "F33D3R health check"
  # One pass by default. A caller that has just started services (rollback.sh)
  # sets CHECK_TIMEOUT_SECONDS to wait for them.
  HEALTH_TIMEOUT_SECONDS="${CHECK_TIMEOUT_SECONDS:-0}"
  wait_healthy
  exit $?
fi

# ── --pull-base mode: rebuild against fresh base images ───────────────────────
# Run this manually when you want fresh base images (new Go/Rust/Python releases).
# Do NOT run on every deploy — it invalidates the build cache and wastes disk.
# It builds images only; bring them up with --all (which backs up first).
if [[ "${1:-}" == "--pull-base" ]]; then
  section "Pulling fresh base images (sequential)"
  for svc in "${BUILD_ORDER[@]}"; do
    check_disk
    info "Pulling base for $svc..."
    $COMPOSE build --pull --no-cache "$svc" || { err "Failed to rebuild $svc against a fresh base"; exit 1; }
    docker image prune -f
  done
  info "Base images updated. Run bash deploy.sh --all to bring services up."
  exit 0
fi

# ── --reload mode: restart feed-engine, NO rebuild ────────────────────────────
# For template (.html) changes after a `git pull`. web/templates is bind-mounted
# read-only and parsed at boot, so a restart is enough — no image rebuild.
if [[ "${1:-}" == "--reload" ]]; then
  section "Reload — restarting feed-engine (no rebuild)"
  $COMPOSE restart feed-engine
  wait_healthy feed-engine || { $COMPOSE logs --tail=60 feed-engine; exit 1; }
  info "feed-engine restarted — templates reparsed, static assets live."
  exit 0
fi

# ── --ui mode: rebuild feed-engine image ──────────────────────────────────────
# Use ONLY when Go code changed. For template/CSS/JS-only changes use --reload
# (or nothing for CSS/JS) — those are bind-mounted live and do not need a rebuild.
if [[ "${1:-}" == "--ui" ]]; then
  check_disk
  section "UI deploy — rebuilding feed-engine image (Go code)"
  $COMPOSE build feed-engine
  section "Restarting feed-engine"
  $COMPOSE up -d --no-deps feed-engine
  wait_healthy feed-engine || { $COMPOSE logs --tail=60 feed-engine; exit 1; }
  info "feed-engine live — new binary, templates, CSS, JS reloaded."
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
  $COMPOSE config --services | grep -qxF "$SVC" || { err "No service named $SVC in docker-compose.prod.yml"; exit 1; }
  check_disk
  section "Rebuilding service: $SVC"
  if $COMPOSE config --format json | jq -e --arg s "$SVC" '.services[$s].build' >/dev/null; then
    $COMPOSE build "$SVC"
  fi
  case "$SVC" in sitra-achra*) clear_stale_locks ;; esac
  $COMPOSE up -d --no-deps "$SVC"
  wait_healthy "$SVC" || { $COMPOSE logs --tail=60 "$SVC"; exit 1; }
  info "$SVC restarted."
  exit 0
fi

# ── --bootstrap mode: first install ───────────────────────────────────────────
# Nothing to back up on an empty host, and --all refuses to run without a
# backup. This mode refuses to run anywhere a database already exists, so it
# can never be the way round a backup.
if [[ "${1:-}" == "--bootstrap" ]]; then
  PROJECT="$($COMPOSE config --format json | jq -er '.name')"
  if docker volume inspect "${PROJECT}_postgres_data" >/dev/null 2>&1; then
    err "Volume ${PROJECT}_postgres_data exists: this host already has a database."
    err "Use bash deploy.sh --all, which backs it up first."
    exit 1
  fi
  check_disk
  build_all
  section "Starting F33D3R"
  $COMPOSE up -d
  prune_dangling
  section "Health check"
  wait_healthy
  info "F33D3R is live. https://${SITE}"
  exit 0
fi

# ── Unknown flag guard ────────────────────────────────────────────────────────
if [[ -n "${1:-}" && "${1}" != "--all" ]]; then
  err "Unknown option: ${1}"
  err "Run with no flag (or --all) for a full rebuild, or one of:"
  err "  --reload  --ui  --service <name>  --check  --pull-base  --bootstrap"
  exit 1
fi

# ── Full deploy ───────────────────────────────────────────────────────────────
# Backs up, then builds each service ONE AT A TIME to prevent disk exhaustion
# from parallel Rust linker temp files. Slow — confirm it was intentional.
warn "Full rebuild of ALL services requested (this is slow)."
warn "For a UI/code change you usually want:  --reload (templates)  or  --ui (Go)."
if [[ -t 0 && "${1:-}" != "--all" ]]; then
  read -r -p "$(echo -e "${YELLOW}[deploy]${NC} Proceed with FULL rebuild? [y/N] ")" _ans
  [[ "${_ans}" =~ ^[Yy]$ ]] || { info "Aborted. Use --reload / --ui for UI changes."; exit 0; }
fi
check_disk

ROLLBACK_SET=""
predeploy_backup

build_all

section "Bringing all services up"
clear_stale_locks
$COMPOSE up -d

section "Pruning dangling layers"
prune_dangling
prune_predeploy

section "Health check"
STATUS=0
wait_healthy || STATUS=$?

echo ""
if [ "$STATUS" -eq 0 ]; then
  info "Deploy complete. https://${SITE}"
  info "Backup taken before it:  ${ROLLBACK_SET}"
  info "Template change:     bash deploy.sh --reload"
  info "Go code change:      bash deploy.sh --ui"
  info "Single service:      bash deploy.sh --service <name>"
  info "Health check only:   bash deploy.sh --check"
else
  err "Deploy finished but the health check failed."
  err "To return to the state before this deploy:"
  err "  bash scripts/rollback.sh ${ROLLBACK_SET}"
  exit 1
fi
