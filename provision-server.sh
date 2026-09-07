#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════════════════════
# F33D3R — Debian Server Provisioning Script
#
# Run this ONCE on a fresh Debian 11/12 server as root or a sudo user.
# After this script completes, the site will be live at https://f33d3r.com
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/... | bash   (if you host it)
#   — OR —
#   bash provision-server.sh
#
# Idempotent: safe to re-run if something fails partway through.
# ═══════════════════════════════════════════════════════════════════════════════
set -euo pipefail

# ── Colours ───────────────────────────────────────────────────────────────────
G="\033[0;32m"; Y="\033[1;33m"; R="\033[0;31m"; B="\033[1m"; NC="\033[0m"
ok()      { echo -e "${G}[✓]${NC} $*"; }
warn()    { echo -e "${Y}[!]${NC} $*"; }
err()     { echo -e "${R}[✗]${NC} $*" >&2; }
section() { echo -e "\n${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n    $*\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }

REPO_URL="git@gitlab.com:f33d3r-core/platform/f33d3r-local.git"
APP_DIR="/opt/f33d3r"
COMPOSE_FILE="docker-compose.prod.yml"
ENV_FILE=".env.prod"

# ── 0. Must be root or sudo ───────────────────────────────────────────────────
if [ "$EUID" -ne 0 ]; then
  warn "Not running as root. Will use sudo for system commands."
  SUDO="sudo"
else
  SUDO=""
fi

# ── 1. System packages ────────────────────────────────────────────────────────
section "1 / 8 — System packages"

$SUDO apt-get update -qq
$SUDO apt-get install -y -qq \
  git curl wget ca-certificates gnupg lsb-release \
  ufw openssl unzip jq

ok "Core packages installed"

# Remove host-level nginx/apache — Caddy (in Docker) owns ports 80 and 443
for svc in nginx apache2 lighttpd; do
  if systemctl is-active --quiet "$svc" 2>/dev/null; then
    warn "Stopping $svc (conflicts with Caddy on ports 80/443)"
    $SUDO systemctl stop "$svc"
    $SUDO systemctl disable "$svc"
    ok "$svc stopped and disabled"
  fi
done

# ── 2. Docker ─────────────────────────────────────────────────────────────────
section "2 / 8 — Docker Engine"

if command -v docker &>/dev/null; then
  ok "Docker already installed: $(docker --version | awk '{print $3}' | tr -d ',')"
else
  ok "Installing Docker (official script)…"
  curl -fsSL https://get.docker.com | $SUDO sh
  ok "Docker installed: $(docker --version | awk '{print $3}' | tr -d ',')"
fi

# Ensure docker compose v2 is available
if ! docker compose version &>/dev/null; then
  err "docker compose v2 not found. Install manually: https://docs.docker.com/compose/install/"
  exit 1
fi
ok "Compose $(docker compose version --short 2>/dev/null || echo ok)"

# Add current user to docker group so we can run without sudo
REAL_USER="${SUDO_USER:-$(whoami)}"
if [ "$REAL_USER" != "root" ]; then
  $SUDO usermod -aG docker "$REAL_USER"
  warn "Added $REAL_USER to docker group. Run 'newgrp docker' or log out and back in to apply."
fi

# ── 3. Firewall ───────────────────────────────────────────────────────────────
section "3 / 8 — Firewall (ufw)"

$SUDO ufw --force enable
$SUDO ufw allow 22/tcp   comment "SSH"
$SUDO ufw allow 80/tcp   comment "HTTP — Caddy Let's Encrypt challenge"
$SUDO ufw allow 443/tcp  comment "HTTPS — F33D3R"
$SUDO ufw reload
ok "Firewall: 22 (SSH), 80 (HTTP), 443 (HTTPS) open"

# ── 4. SSH key for GitLab ─────────────────────────────────────────────────────
section "4 / 8 — GitLab SSH key"

SSH_KEY="/root/.ssh/id_ed25519"
if [ "$REAL_USER" != "root" ]; then
  SSH_KEY="/home/${REAL_USER}/.ssh/id_ed25519"
fi

if [ ! -f "$SSH_KEY" ]; then
  $SUDO -u "$REAL_USER" ssh-keygen -t ed25519 -C "f33d3r-server" -f "$SSH_KEY" -N ""
  ok "Generated new SSH key: $SSH_KEY"
  echo ""
  echo -e "${B}══════════════════════════════════════════════${NC}"
  echo -e "${B}Add this public key to your GitLab account:${NC}"
  echo -e "${B}  GitLab → Settings → SSH Keys${NC}"
  echo -e "${B}══════════════════════════════════════════════${NC}"
  cat "${SSH_KEY}.pub"
  echo ""
  read -r -p "Press ENTER after you've added the key to GitLab → "
else
  ok "SSH key already exists: $SSH_KEY"
fi

# Test GitLab SSH access
if ssh -o StrictHostKeyChecking=no -o BatchMode=yes git@gitlab.com exit 2>/dev/null; then
  ok "GitLab SSH access confirmed"
else
  warn "Could not verify GitLab SSH access. Continuing — clone may fail if key isn't added."
fi

# ── 5. Clone / update repository ─────────────────────────────────────────────
section "5 / 8 — Repository"

if [ -d "$APP_DIR/.git" ]; then
  ok "Repo already cloned at $APP_DIR — pulling latest main"
  git -C "$APP_DIR" fetch origin
  git -C "$APP_DIR" checkout main
  git -C "$APP_DIR" pull origin main
else
  ok "Cloning F33D3R into $APP_DIR…"
  $SUDO mkdir -p "$(dirname "$APP_DIR")"
  git clone "$REPO_URL" "$APP_DIR"
  if [ "$REAL_USER" != "root" ]; then
    $SUDO chown -R "$REAL_USER:$REAL_USER" "$APP_DIR"
  fi
fi

cd "$APP_DIR"
ok "On commit: $(git rev-parse --short HEAD) — $(git log -1 --format='%s')"

# ── 6. Environment file ───────────────────────────────────────────────────────
section "6 / 8 — Environment"

if [ ! -f "$ENV_FILE" ]; then
  cp .env.prod.example "$ENV_FILE"

  echo ""
  echo -e "${B}═══════════════════════════════════════════════════${NC}"
  echo -e "${B}  .env.prod needs its secrets. Generating them now.${NC}"
  echo -e "${B}═══════════════════════════════════════════════════${NC}"

  # Every secret the template ships as a CHANGE_ME placeholder is generated
  # here, on this host, by openssl — none of them is ever a value in git.
  # Auto-generate a strong postgres password
  PG_PASS=$(openssl rand -base64 24 | tr -dc 'a-zA-Z0-9' | head -c 32)
  # Session cookie signing key
  SESSION_SECRET=$(openssl rand -base64 48)
  # Brain-to-brain shared secret: Manhattan, every Rust brain, Verity, Ain
  # Soph, Themis and feed-engine all carry this one value.
  INTERNAL_API_KEY=$(openssl rand -hex 32)
  # NSFW media URL signing key
  MEDIA_TOKEN_SECRET=$(openssl rand -base64 48)
  # Prometheus /metrics bearer token
  METRICS_TOKEN=$(openssl rand -hex 24)
  # Themis P-256 ECDH key (PKCS#8 DER, base64 single line). Losing this key
  # loses every previously sold CEK, which is why it is generated once here
  # and never regenerated on re-run.
  THEMIS_ECDH_PRIVKEY_B64=$(openssl ecparam -name prime256v1 -genkey -noout \
    | openssl pkcs8 -topk8 -nocrypt -outform DER | base64 -w0)

  sed -i "s|CHANGE_ME_use_a_strong_random_password|${PG_PASS}|g" "$ENV_FILE"
  sed -i "s|CHANGE_ME_generate_with_openssl_rand_-base64_48|${SESSION_SECRET}|g" "$ENV_FILE"
  sed -i "s|^INTERNAL_API_KEY=CHANGE_ME$|INTERNAL_API_KEY=${INTERNAL_API_KEY}|" "$ENV_FILE"
  sed -i "s|CHANGE_ME_media_token_generate_with_openssl_rand_-base64_48|${MEDIA_TOKEN_SECRET}|g" "$ENV_FILE"
  sed -i "s|CHANGE_ME_metrics_generate_with_openssl_rand_-hex_24|${METRICS_TOKEN}|g" "$ENV_FILE"
  sed -i "s|CHANGE_ME_generate_with_openssl_ecparam|${THEMIS_ECDH_PRIVKEY_B64}|g" "$ENV_FILE"

  ok "Secrets auto-generated in $ENV_FILE"
  echo ""
  warn "SAVE THESE — you'll need them if you ever need to access postgres directly:"
  echo "  POSTGRES_PASSWORD       = $PG_PASS"
  echo "  SESSION_SECRET          = (saved to .env.prod)"
  echo "  INTERNAL_API_KEY        = (saved to .env.prod)"
  echo "  MEDIA_TOKEN_SECRET      = (saved to .env.prod)"
  echo "  METRICS_TOKEN           = (saved to .env.prod)"
  echo "  THEMIS_ECDH_PRIVKEY_B64 = (saved to .env.prod — back it up; it unlocks sold content)"
  echo ""
fi

# Verify no placeholders remain
if grep -q "CHANGE_ME" "$ENV_FILE"; then
  err ".env.prod still has CHANGE_ME placeholders:"
  grep -n "CHANGE_ME" "$ENV_FILE" | sed 's/=.*//' | while read -r line; do err "  $line"; done
  err "Edit it:  nano $APP_DIR/$ENV_FILE"
  exit 1
fi
# The brain-to-brain secret must be a real key: every service refuses or is
# refused on a short or missing one, and the failure surfaces as 401s at
# runtime rather than here.
INTERNAL_KEY_LEN=$(grep -E '^INTERNAL_API_KEY=' "$ENV_FILE" | head -n1 | cut -d= -f2- | tr -d '[:space:]' | wc -c)
if [ "$INTERNAL_KEY_LEN" -lt 32 ]; then
  err "INTERNAL_API_KEY in $ENV_FILE is missing or shorter than 32 characters."
  err "  Generate one:  openssl rand -hex 32"
  exit 1
fi
ok "$ENV_FILE ready"

# ── 7. DNS check ─────────────────────────────────────────────────────────────
section "7 / 8 — DNS check"

SERVER_IP=$(curl -s --max-time 5 https://api.ipify.org 2>/dev/null || echo "unknown")
RESOLVED_IP=$(dig +short f33d3r.com @1.1.1.1 2>/dev/null | tail -1 || echo "unresolved")

ok "Server public IP:  $SERVER_IP"
ok "f33d3r.com → $RESOLVED_IP"

if [ "$SERVER_IP" = "$RESOLVED_IP" ]; then
  ok "DNS is correctly pointed at this server"
else
  warn "DNS mismatch — f33d3r.com does not resolve to this server yet."
  warn "Add an A record: f33d3r.com → $SERVER_IP"
  warn "Caddy will fail to get a Let's Encrypt cert until DNS propagates."
  warn "You can still start F33D3R now; Caddy will retry cert provisioning."
fi

# ── 8. Build and launch ───────────────────────────────────────────────────────
section "8 / 8 — Build and launch (first build: 10–20 min)"

docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" up --build -d

# ── Systemd service — auto-start on reboot ─────────────────────────────────────
cat > /tmp/f33d3r.service <<SERVICE
[Unit]
Description=F33D3R platform
Requires=docker.service
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=${APP_DIR}
ExecStart=/usr/bin/docker compose -f ${COMPOSE_FILE} --env-file ${ENV_FILE} up -d
ExecStop=/usr/bin/docker compose -f ${COMPOSE_FILE} --env-file ${ENV_FILE} down
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
SERVICE

$SUDO mv /tmp/f33d3r.service /etc/systemd/system/f33d3r.service
$SUDO systemctl daemon-reload
$SUDO systemctl enable f33d3r.service
ok "Systemd service installed — F33D3R will start automatically on reboot"

# ── Wait for services ─────────────────────────────────────────────────────────
echo ""
ok "Waiting 45s for services to come online (Rust compile time)…"
sleep 45

PASS=0; FAIL=0
check() {
  local name="$1" container="$2"
  if docker compose -f "$COMPOSE_FILE" ps --status running "$container" 2>/dev/null | grep -q "$container"; then
    printf "  ${G}●${NC} %-35s running\n" "$name"
    PASS=$((PASS+1))
  else
    printf "  ${R}○${NC} %-35s NOT running\n" "$name"
    FAIL=$((FAIL+1))
  fi
}

check "postgres"               "postgres"
check "redis"                  "redis"
check "aethyr-schema-registry" "aethyr-schema-registry"
check "aethyrrank-engine"      "aethyrrank-engine"
check "feed-engine (Nantar)"   "feed-engine"
check "caeor"                  "caeor"
check "caddy"                  "caddy"

echo ""
if [ "$FAIL" -gt 0 ]; then
  warn "$FAIL service(s) still starting or failed. Check logs:"
  warn "  docker compose -f $COMPOSE_FILE logs --tail=40 <service-name>"
else
  ok "All $PASS services running"
fi

# ── Summary ────────────────────────────────────────────────────────────────────
echo ""
echo -e "${B}═══════════════════════════════════════════════════════${NC}"
echo -e "${B}  F33D3R provisioning complete${NC}"
echo -e "${B}═══════════════════════════════════════════════════════${NC}"
echo ""
echo "  Site:     https://f33d3r.com"
echo "  Logs:     docker compose -f $COMPOSE_FILE logs -f"
echo "  Status:   docker compose -f $COMPOSE_FILE ps"
echo "  Deploy:   bash $APP_DIR/deploy.sh"
echo "  Stop:     docker compose -f $COMPOSE_FILE down"
echo "  Restart:  docker compose -f $COMPOSE_FILE restart <service>"
echo ""
if [ "$SERVER_IP" != "$RESOLVED_IP" ]; then
  echo -e "  ${Y}DNS not yet pointed here. Once propagated, Caddy auto-gets TLS cert.${NC}"
  echo -e "  ${Y}Test HTTP at: http://$SERVER_IP:8081 (bypasses Caddy)${NC}"
  echo ""
fi
echo -e "  ${G}To update after a git push:${NC}"
echo "    bash $APP_DIR/deploy.sh"
echo ""
