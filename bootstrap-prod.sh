#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# F33D3R Production Bootstrap — first-time start on a provisioned server
# Run from the repo root on the Debian server, after provision-server.sh (which
# installs Docker, the firewall, the backup directory and .env.prod) — or on any
# host where those already exist.
#
# Usage: bash bootstrap-prod.sh
#
# The same flow as bootstrap-local.sh: check the tree, check Docker, check the
# environment, build and start, health check. The build, start and health
# check are deploy.sh's (--bootstrap), so first install and every later deploy
# run the same code. It refuses to run where a database already exists — that
# is a deploy, and deploy.sh --all backs up first.
#
# There is no founder account to seed. The first person to sign up, through
# elohim-veni, is the founder.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

cd "$(dirname "$(realpath "$0")")"

GREEN="\033[0;32m"; YELLOW="\033[1;33m"; RED="\033[0;31m"; BOLD="\033[1m"; NC="\033[0m"
info()    { echo -e "${GREEN}[f33d3r]${NC} $*"; }
warn()    { echo -e "${YELLOW}[f33d3r]${NC} $*"; }
err()     { echo -e "${RED}[f33d3r]${NC} $*" >&2; }
section() { echo -e "\n${BOLD}── $* ──${NC}"; }

COMPOSE_FILE=docker-compose.prod.yml
ENV_FILE=.env.prod

# ── 1. Docker and tools ───────────────────────────────────────────────────────
section "Checking Docker"
command -v docker >/dev/null || { err "Docker not found. Install with: curl -fsSL https://get.docker.com | sh"; exit 1; }
docker info >/dev/null || { err "Docker daemon not reachable: sudo systemctl start docker"; exit 1; }
docker compose version >/dev/null || { err "docker compose v2 not found: https://docs.docker.com/compose/install/"; exit 1; }
for tool in jq curl openssl; do
  command -v "$tool" >/dev/null || { err "$tool not found: sudo apt-get install -y $tool"; exit 1; }
done
info "Docker $(docker --version | awk '{print $3}' | tr -d ',') · Compose $(docker compose version --short)"

# ── 2. Environment file ───────────────────────────────────────────────────────
section "Environment file"
if [ ! -f "$ENV_FILE" ]; then
  cp .env.prod.example "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  err "$ENV_FILE was not found — a template has been created."
  err "provision-server.sh generates every secret in it for a fresh server."
  err "To fill it by hand, replace every CHANGE_ME:"
  err "  POSTGRES_PASSWORD       — openssl rand -hex 24"
  err "  SESSION_SECRET          — openssl rand -base64 48"
  err "  INTERNAL_API_KEY        — openssl rand -hex 32"
  err "  MEDIA_TOKEN_SECRET      — openssl rand -base64 48"
  err "  THEMIS_ECDH_PRIVKEY_B64 — openssl ecparam -name prime256v1 -genkey -noout | openssl pkcs8 -topk8 -nocrypt -outform DER | base64 -w0"
  err "  LIVEKIT_API_KEY         — frequencies_\$(openssl rand -hex 6)"
  err "  LIVEKIT_API_SECRET      — openssl rand -hex 32"
  err "  LIVE_PUBLIC_HOST        — this server's public IP"
  err "Then re-run this script."
  exit 1
fi
if grep -q "CHANGE_ME" "$ENV_FILE"; then
  err "$ENV_FILE still contains placeholder values:"
  grep -n "CHANGE_ME" "$ENV_FILE" | sed 's/=.*//' | while read -r line; do err "  $line"; done
  exit 1
fi
INTERNAL_KEY_LEN=$(grep -E '^INTERNAL_API_KEY=' "$ENV_FILE" | head -n1 | cut -d= -f2- | tr -d '[:space:]' | wc -c)
if [ "$INTERNAL_KEY_LEN" -lt 32 ]; then
  err "INTERNAL_API_KEY in $ENV_FILE is missing or shorter than 32 characters (openssl rand -hex 32)."
  exit 1
fi
info "$ENV_FILE OK"

COMPOSE="docker compose -f $COMPOSE_FILE --env-file $ENV_FILE"

# ── 3. The tree: every build context the compose file names ───────────────────
section "Checking the repository against $COMPOSE_FILE"
$COMPOSE config -q
MISSING=0
while IFS=$'\t' read -r svc ctx dockerfile; do
  if [ ! -d "$ctx" ]; then err "  $svc: build context $ctx is missing"; MISSING=1; continue; fi
  if [ ! -f "$ctx/$dockerfile" ]; then err "  $svc: $ctx/$dockerfile is missing"; MISSING=1; fi
done < <($COMPOSE config --format json \
  | jq -r '.services | to_entries[] | select(.value.build) | [.key, .value.build.context, (.value.build.dockerfile // "Dockerfile")] | @tsv')
[ "$MISSING" -eq 0 ] || { err "The checkout is incomplete. Clone the full repository."; exit 1; }
info "Every build context and Dockerfile is present"

# ── 4. Backup directory ───────────────────────────────────────────────────────
section "Backup directory"
BACKUP_HOST_DIR="$($COMPOSE config --format json | jq -er '.services.backup.volumes[] | select(.target == "/backups") | .source')"
if [ ! -d "$BACKUP_HOST_DIR" ]; then
  err "$BACKUP_HOST_DIR does not exist. Create it (provision-server.sh does):"
  err "  sudo install -d -m 700 -o \"\$(id -un)\" \"$BACKUP_HOST_DIR\""
  exit 1
fi
info "Backups go to $BACKUP_HOST_DIR (copy it off this host as well)"

# ── 5. DNS / ports ────────────────────────────────────────────────────────────
section "Pre-flight"
SERVER_IP=$(curl -fsS --max-time 5 https://api.ipify.org) || { err "could not read this server's public IP"; exit 1; }
info "Server public IP: ${SERVER_IP}"
warn "DNS: f33d3r.com must resolve to ${SERVER_IP} — Caddy cannot obtain a certificate"
warn "until it does, and the health check through https://f33d3r.com fails until then."
warn "Public ports: 80, 443/tcp · 1935/tcp RTMP · 8890/udp SRT · 8189/udp WebRTC · 7881/tcp 7882/udp LiveKit"
if [ -t 0 ]; then
  read -r -p "Continue? [y/N] " confirm
  [[ "$confirm" =~ ^[Yy]$ ]] || { info "Aborted."; exit 0; }
fi

# ── 6. Build, start, health check ─────────────────────────────────────────────
section "Building and starting F33D3R (production)"
info "First build takes 10–20 min (Rust compile time)"
export DOCKER_BUILDKIT=1 COMPOSE_DOCKER_CLI_BUILD=1
bash ./deploy.sh --bootstrap

echo ""
echo -e "${BOLD}F33D3R is live.${NC}"
echo ""
echo "  Site:     https://f33d3r.com — the first account to sign up is the founder"
echo "  Health:   bash deploy.sh --check"
echo "  Deploy:   bash deploy.sh --all   (backs up first)"
echo "  Logs:     $COMPOSE logs -f"
echo ""
