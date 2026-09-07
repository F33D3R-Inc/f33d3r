#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# F33D3R Local Bootstrap — cross-platform (Linux, macOS, Windows/WSL)
#
# Run from the directory that contains the brain folders + docker-compose.local.yml
# Usage:  bash bootstrap-local.sh
#
# Requirements:
#   - Docker (any OS, any distro). Docker Desktop on macOS / Windows is fine.
#   - Docker Compose v2 (bundled with modern Docker installs).
#   - .env.local file present (template in README.md).
#
# This script intentionally avoids OS-specific assumptions. It detects the
# host OS and skips Linux-only checks (kernel modules, modprobe) on macOS / WSL.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

GREEN="\033[0;32m"; YELLOW="\033[1;33m"; RED="\033[0;31m"; BOLD="\033[1m"; NC="\033[0m"
info()    { echo -e "${GREEN}[f33d3r]${NC} $*"; }
warn()    { echo -e "${YELLOW}[f33d3r]${NC} $*"; }
err()     { echo -e "${RED}[f33d3r]${NC} $*" >&2; }
section() { echo -e "\n${BOLD}── $* ──${NC}"; }

# ── 0. Detect host OS so we can skip platform-specific checks ────────────────
OS_KIND="unknown"
case "$(uname -s 2>/dev/null || echo unknown)" in
  Linux*)
    if grep -qi microsoft /proc/version 2>/dev/null; then
      OS_KIND="wsl"
    else
      OS_KIND="linux"
    fi
    ;;
  Darwin*)  OS_KIND="macos" ;;
  MINGW*|MSYS*|CYGWIN*) OS_KIND="windows" ;;
esac

# ── 1. Directory layout ───────────────────────────────────────────────────────
section "Checking directory layout"
MISSING=""
for d in feed-engine aethyrrank-engine zior-engine aethyr-schema-registry ain-soph; do
  if [ ! -d "$d" ]; then MISSING="$MISSING $d"; fi
done
if [ -n "$MISSING" ]; then
  err "Missing directories:$MISSING"
  err "Run this script from the folder that contains all unzipped services."
  exit 1
fi
info "Core service directories found"

# ── 2. Docker ─────────────────────────────────────────────────────────────────
section "Checking Docker"
if ! command -v docker >/dev/null 2>&1; then
  err "Docker not found. Install: https://docs.docker.com/get-docker/"
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  err "Docker daemon not reachable. On macOS/Windows: start Docker Desktop."
  err "On Linux: 'sudo systemctl start docker' (or your equivalent)."
  exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
  err "docker compose v2 not found. Install: https://docs.docker.com/compose/install/"
  exit 1
fi
DOCKER_VER=$(docker --version 2>/dev/null | awk '{print $3}' | tr -d ',' || echo "?")
COMPOSE_VER=$(docker compose version --short 2>/dev/null || echo "?")
info "Docker $DOCKER_VER · Compose $COMPOSE_VER · Host: $OS_KIND"

# ── 3. Local toolchains (optional; everything builds in Docker either way) ────
section "Checking local toolchains (optional)"
command -v rustc >/dev/null 2>&1 && info "Rust $(rustc --version 2>/dev/null | awk '{print $2}')" || warn "Rust not installed — Docker builds will still work"
command -v go    >/dev/null 2>&1 && info "Go $(go version | awk '{print $3}')"                    || warn "Go not installed — Docker builds will still work"

# ── 4. Linux-only kernel sanity (warn-only; never fatal) ─────────────────────
# Docker Desktop on macOS / Windows manages its own VM kernel, so this check
# does not apply. On Linux, we warn the user but never block — Docker will
# fail loudly enough on its own if networking is broken, and many distros
# (NixOS, container hosts, etc.) lay out modules differently.
if [ "$OS_KIND" = "linux" ]; then
  section "Checking Linux kernel modules"
  KREL="$(uname -r 2>/dev/null || echo unknown)"
  if [ -d "/usr/lib/modules/$KREL" ] || [ -d "/lib/modules/$KREL" ]; then
    info "Kernel modules present ($KREL)"
  else
    warn "No kernel-module directory for $KREL."
    warn "If you just updated your kernel, reboot before running Docker workloads."
    warn "If your distro stores modules elsewhere (NixOS / minimal containers / cloud images),"
    warn "this is harmless — proceeding."
  fi
  if command -v modprobe >/dev/null 2>&1; then
    if ! modprobe --dry-run veth >/dev/null 2>&1; then
      warn "veth module not detected via modprobe — Docker bridge networking may need 'sudo modprobe veth'."
    fi
  fi
fi

# ── 5. Environment file ───────────────────────────────────────────────────────
section "Setting up environment"
if [ ! -f .env.local ]; then
  warn ".env.local not found — creating a minimal one with sane defaults."
  # The brain-to-brain secret is random per checkout, never a constant that
  # every clone of this repo shares. Generated once here; the file is not
  # rewritten on later runs, so the value is stable for this machine.
  if ! command -v openssl >/dev/null 2>&1; then
    err "openssl not found — it is needed to generate INTERNAL_API_KEY for .env.local."
    err "Install it (apt: openssl · brew: openssl · Windows: ships with Git for Windows) and re-run."
    exit 1
  fi
  LOCAL_INTERNAL_API_KEY=$(openssl rand -hex 32)
  cat > .env.local <<EOF
# F33D3R local defaults — generated by bootstrap-local.sh
POSTGRES_USER=f33d3r
POSTGRES_PASSWORD=f33d3rdev
POSTGRES_DB=f33d3r
SESSION_SECRET=dev_secret_change_in_production_32chars
# Service-to-service secret: Manhattan and every brain that reaches it, Verity,
# Ain Soph, Themis, feed-engine. One value across the stack or calls answer 401.
# Generated by openssl on this machine; docker-compose.local.yml hands the same
# value to every service.
INTERNAL_API_KEY=${LOCAL_INTERNAL_API_KEY}
SHOW_SCORES=false
DEV_MODE=true
RUST_LOG=info
MINIO_ROOT_USER=f33d3r_minio
MINIO_ROOT_PASSWORD=f33d3rdev_change_in_prod_32c
MINIO_PUBLIC_BASE=http://localhost:9000
GRAFANA_ADMIN_USER=admin
GRAFANA_ADMIN_PASSWORD=admin
EOF
  info ".env.local created with development defaults"
else
  info ".env.local found"
fi

# ── 6. Optional Go module integrity check ────────────────────────────────────
section "Checking Go module integrity"
if command -v go >/dev/null 2>&1 && [ -f feed-engine/go.mod ]; then
  pushd feed-engine >/dev/null
  if ! go mod verify >/dev/null 2>&1; then
    warn "go.sum out of sync — running 'go mod tidy'"
    go mod tidy
    info "go.sum regenerated"
  else
    info "Go modules OK"
  fi
  popd >/dev/null
else
  info "Go not installed locally — Docker handles module resolution"
fi

# ── 7. Build + start ─────────────────────────────────────────────────────────
section "Building and starting F33D3R"
info "First run pulls images + compiles Rust crates — expect 10–20 min on cold cache."
info "Subsequent runs are seconds (Docker layer cache + Cargo incremental)."
echo ""

# BuildKit is the modern Docker builder — faster, parallel, and handles
# the 'tar: io: read/write on closed pipe' errors that the legacy builder
# used to throw on large build contexts.
export DOCKER_BUILDKIT=1
export COMPOSE_DOCKER_CLI_BUILD=1

docker compose -f docker-compose.local.yml --env-file .env.local up --build -d

# ── 8. Wait + health check ───────────────────────────────────────────────────
section "Waiting for services to come online"
# First-boot can take a while for Postgres init scripts; give a generous wait.
sleep 12

PASS=0; FAIL=0
check_service() {
  local name="$1" port="$2" path="${3:-/health}" scheme="${4:-http}"
  local code
  # --cacert is only consulted for https probes; the Caddy edge (tls internal)
  # is the one that needs it. See infra/README-CERTS.md.
  code=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 3 --max-time 5 \
    --cacert "$(dirname "${BASH_SOURCE[0]}")/infra/caddy-root.crt" \
    "${scheme}://localhost:${port}${path}" 2>/dev/null || echo "000")
  if [ "$code" = "200" ] || [ "$code" = "204" ]; then
    printf "  ${GREEN}●${NC} %-32s port %-5s ${GREEN}online${NC}\n" "$name" "$port"
    PASS=$((PASS+1))
  else
    printf "  ${RED}○${NC} %-32s port %-5s ${YELLOW}starting…${NC} (http %s)\n" "$name" "$port" "$code"
    FAIL=$((FAIL+1))
  fi
}

# One line per service docker-compose.local.yml publishes a host port for,
# probing the same path its compose healthcheck does.
check_service "aethyr-schema-registry" "8079"
check_service "aethyrrank-engine"      "8080"
# feed-engine publishes no host port (same as prod); it is probed through the
# Caddy edge, which is also the only address a browser or phone should use.
check_service "feed-engine (Nantar) via Caddy" "8443" "/api/health" "https"
check_service "zior-engine"            "8082"
check_service "transcoding"            "8085"
check_service "caeor"                  "8086"
check_service "astraon"                "8088"
check_service "ain-soph"               "8089"
check_service "zodacare"               "8090"
check_service "live-edge"              "8092" "/healthz"
check_service "elohim-veni"            "8093"
check_service "registry-brain"         "8094"
check_service "verity"                 "8095"
check_service "aethyr-ledger"          "8096"
check_service "content-scan"           "8097" "/ready"
check_service "alexandria"             "8098"
check_service "ekyc"                   "8099"
check_service "themis"                 "8100"
check_service "herald"                 "8105"
check_service "lore"                   "8106"
check_service "manhattan"              "8107"
check_service "auralis (Frequencies)"  "8108"
check_service "minio"                  "9000" "/minio/health/live"
check_service "prometheus"             "9090" "/-/healthy"
check_service "alertmanager"           "9093" "/-/healthy"
check_service "loki"                   "3100" "/ready"
check_service "grafana"                "3000" "/api/health"

echo ""
if [ "$FAIL" -gt 0 ]; then
  warn "$FAIL service(s) still starting. Re-check in 30 s:"
  warn "  bash health-check-local.sh"
  warn "Or tail logs:"
  warn "  docker compose -f docker-compose.local.yml logs -f"
else
  info "All $PASS services online"
fi

echo ""
echo -e "${BOLD}F33D3R is running.${NC}"
echo ""
echo "  Site:            https://localhost:8443   (Caddy, local CA — infra/README-CERTS.md; the only way in, as in prod)"
echo "  Site (LAN):      https://$(hostname | tr '[:upper:]' '[:lower:]').local:8443   for phones, simulators, the other machine"
echo "  Grafana:         http://localhost:3000   (admin / admin — change in .env.local)"
echo "  Prometheus:      http://localhost:9090"
echo "  MinIO console:   http://localhost:9001"
echo ""
echo "  Logs:   docker compose -f docker-compose.local.yml logs -f"
echo "  Stop:   docker compose -f docker-compose.local.yml down"
echo ""
