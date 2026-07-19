#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# F33D3R Production Bootstrap — first-time server setup
# Run from the repo root on the Debian server.
#
# Usage: bash bootstrap-prod.sh
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

GREEN="\033[0;32m"; YELLOW="\033[1;33m"; RED="\033[0;31m"; BOLD="\033[1m"; NC="\033[0m"
info()    { echo -e "${GREEN}[f33d3r]${NC} $*"; }
warn()    { echo -e "${YELLOW}[f33d3r]${NC} $*"; }
err()     { echo -e "${RED}[f33d3r]${NC} $*" >&2; }
section() { echo -e "\n${BOLD}── $* ──${NC}"; }

# ── 1. Check Docker ───────────────────────────────────────────────────────────
section "Checking Docker"
if ! command -v docker &>/dev/null; then
  err "Docker not found. Install with: curl -fsSL https://get.docker.com | sh"
  exit 1
fi
if ! docker compose version &>/dev/null; then
  err "docker compose v2 not found. Install: https://docs.docker.com/compose/install/"
  exit 1
fi
info "Docker $(docker --version | awk '{print $3}' | tr -d ',')"
info "Compose $(docker compose version --short 2>/dev/null || echo ok)"

# ── 2. Check service dirs ─────────────────────────────────────────────────────
section "Checking service directories"
MISSING=""
for d in feed-engine aethyrrank-engine zior-engine aethyr-schema-registry ain-soph gnosis-malkuth; do
  [ ! -d "$d" ] && MISSING="$MISSING $d"
done
if [ -n "$MISSING" ]; then
  err "Missing directories:$MISSING"
  err "Make sure you cloned the full repo."
  exit 1
fi
info "All service directories found"

# ── 3. Environment file ───────────────────────────────────────────────────────
section "Environment file"
if [ ! -f .env.prod ]; then
  cp .env.prod.example .env.prod
  err ".env.prod was not found — a template has been created."
  err "Edit it now before continuing:"
  err ""
  err "  nano .env.prod"
  err ""
  err "Required changes:"
  err "  POSTGRES_PASSWORD — set a strong password"
  err "  SESSION_SECRET    — run: openssl rand -base64 48"
  err ""
  err "Then re-run this script."
  exit 1
fi

# Verify required secrets are not placeholders
if grep -q "CHANGE_ME" .env.prod; then
  err ".env.prod still contains placeholder values. Edit it before deploying."
  exit 1
fi
info ".env.prod OK"

# ── 4. Check DNS / ports ──────────────────────────────────────────────────────
section "Pre-flight checks"
SERVER_IP=$(curl -s --max-time 5 https://api.ipify.org 2>/dev/null || echo "unknown")
info "Server public IP: ${SERVER_IP}"
warn "DNS: f33d3r.com must resolve to ${SERVER_IP}"
warn "Firewall: ports 80 and 443 must be open"
echo ""
read -r -p "Continue? [y/N] " confirm
[[ "$confirm" =~ ^[Yy]$ ]] || { info "Aborted."; exit 0; }

# ── 5. Build and start ────────────────────────────────────────────────────────
section "Building and starting F33D3R (production)"
info "First build takes 5–15 min (Rust compile time)"
info "Subsequent deploys are fast (Docker layer cache)"
echo ""

docker compose -f docker-compose.prod.yml --env-file .env.prod up --build -d

# ── 5b. Sync Gnosis Malkuth WASM core onto the host static dir ─────────────────
# feed-engine bind-mounts ./feed-engine/web/static over /app/web/static, masking
# the Malkuth crypto core (Pillar 2 E2EE) the image bakes in. messages.html imports
# /static/js/malkuth/gnosis_malkuth.js, so copy the built core out of the image
# onto the bind-mounted host dir or the E2EE pillar 404s. (deploy.sh does the same.)
section "Syncing Gnosis Malkuth WASM core"
FE_IMG=$(docker compose -f docker-compose.prod.yml --env-file .env.prod images -q feed-engine 2>/dev/null | head -n1)
if [ -n "$FE_IMG" ]; then
  mkdir -p feed-engine/web/static/js/malkuth
  FE_CID=$(docker create "$FE_IMG")
  docker cp "$FE_CID:/app/web/static/js/malkuth/gnosis_malkuth.js"      feed-engine/web/static/js/malkuth/gnosis_malkuth.js
  docker cp "$FE_CID:/app/web/static/js/malkuth/gnosis_malkuth_bg.wasm" feed-engine/web/static/js/malkuth/gnosis_malkuth_bg.wasm
  docker rm "$FE_CID" >/dev/null
  info "Malkuth WASM core synced to feed-engine/web/static/js/malkuth/"
else
  err "feed-engine image not found — Malkuth WASM core not synced; E2EE messaging will 404."
fi

# ── 6. Wait and verify ────────────────────────────────────────────────────────
section "Waiting for services (30s)"
sleep 30

PASS=0; FAIL=0
check() {
  local name="$1" container="$2"
  if docker compose -f docker-compose.prod.yml ps --status running "$container" 2>/dev/null | grep -q "$container"; then
    printf "  ${GREEN}●${NC} %-30s running\n" "$name"
    PASS=$((PASS+1))
  else
    printf "  ${RED}○${NC} %-30s not running\n" "$name"
    FAIL=$((FAIL+1))
  fi
}

check "postgres"               "postgres"
check "aethyr-schema-registry" "aethyr-schema-registry"
check "aethyrrank-engine"      "aethyrrank-engine"
check "feed-engine"            "feed-engine"
check "caeor"                  "caeor"
check "caddy"                  "caddy"

echo ""
if [ "$FAIL" -gt 0 ]; then
  warn "$FAIL service(s) not running — check logs:"
  warn "  docker compose -f docker-compose.prod.yml logs --tail=50"
else
  info "All $PASS services running"
  echo ""
  echo -e "${BOLD}F33D3R is live.${NC}"
  echo ""
  echo "  Site:   https://f33d3r.com"
  echo "  Logs:   docker compose -f docker-compose.prod.yml logs -f"
  echo "  Stop:   docker compose -f docker-compose.prod.yml down"
  echo ""
fi
