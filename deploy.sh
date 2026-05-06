#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# F33D3R Deploy
# Run from the repo root on the Debian server.
#
# Usage:
#   bash deploy.sh          — full rebuild (Go/Rust code changes, new services)
#   bash deploy.sh --ui     — UI-only: git pull + restart feed-engine (~5 sec)
#                             Use this for CSS, JS, or template changes only.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

cd "$(dirname "$(realpath "$0")")"

GREEN="\033[0;32m"; YELLOW="\033[1;33m"; RED="\033[0;31m"; BOLD="\033[1m"; NC="\033[0m"
info()    { echo -e "${GREEN}[deploy]${NC} $*"; }
warn()    { echo -e "${YELLOW}[deploy]${NC} $*"; }
err()     { echo -e "${RED}[deploy]${NC} $*" >&2; }
section() { echo -e "\n${BOLD}── $* ──${NC}"; }

# ── UI-only fast path ─────────────────────────────────────────────────────────
if [[ "${1:-}" == "--ui" ]]; then
  section "UI-only deploy — pulling latest"
  git pull origin main
  info "On commit: $(git rev-parse --short HEAD) — $(git log -1 --format='%s')"
  section "Restarting feed-engine (picks up new templates)"
  docker compose -f docker-compose.prod.yml --env-file .env.prod restart feed-engine
  sleep 5
  if docker compose -f docker-compose.prod.yml ps --status running feed-engine 2>/dev/null | grep -q "feed-engine"; then
    info "feed-engine restarted — CSS/JS live immediately, templates live now."
    info "https://f33d3r.com"
  else
    err "feed-engine failed to restart — check logs:"
    err "  docker compose -f docker-compose.prod.yml logs --tail=50 feed-engine"
    exit 1
  fi
  exit 0
fi

# ── Full rebuild ──────────────────────────────────────────────────────────────
section "Pulling latest from GitLab"
git pull origin main
info "On commit: $(git rev-parse --short HEAD) — $(git log -1 --format='%s')"

section "Rebuilding and restarting services"
docker compose -f docker-compose.prod.yml --env-file .env.prod up --build -d

section "Verifying"
sleep 15

PASS=0; FAIL=0
check() {
  local name="$1" container="$2"
  if docker compose -f docker-compose.prod.yml ps --status running "$container" 2>/dev/null | grep -q "$container"; then
    printf "  ${GREEN}●${NC} %-30s running\n" "$name"
    PASS=$((PASS+1))
  else
    printf "  ${RED}○${NC} %-30s NOT running\n" "$name"
    FAIL=$((FAIL+1))
  fi
}

check "postgres"               "postgres"
check "feed-engine"            "feed-engine"
check "aethyrrank-engine"      "aethyrrank-engine"
check "caddy"                  "caddy"

echo ""
if [ "$FAIL" -gt 0 ]; then
  err "$FAIL service(s) not running:"
  err "  docker compose -f docker-compose.prod.yml logs --tail=50 <service>"
  exit 1
else
  info "Deploy complete — $PASS services running"
  info "  Next UI-only change: bash deploy.sh --ui  (no rebuild needed)"
  info "https://f33d3r.com"
fi
