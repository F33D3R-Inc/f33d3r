#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════════════════════
# F33D3R — Debian Server Provisioning Script
#
# Run this ONCE on a fresh Debian 12 server as root or a sudo user. It installs
# Docker and the firewall, clones the repository, generates .env.prod, creates
# the backup directory, installs the boot unit and starts F33D3R through
# bootstrap-prod.sh. The first account to sign up is the founder.
#
# Usage:
#   bash provision-server.sh
#
# Configuration (environment, all optional):
#   F33D3R_REPO_URL   git remote to clone  (default git@github.com:F33D3R-Inc/f33d3r.git)
#   F33D3R_BRANCH     branch to deploy     (default main)
#   F33D3R_APP_DIR    checkout location    (default /opt/f33d3r)
#
# Idempotent: safe to re-run if something fails partway through. A re-run on a
# host that already has a database does not start anything; later updates are
# `git pull` and `bash deploy.sh --all`, which backs up first.
# ═══════════════════════════════════════════════════════════════════════════════
set -euo pipefail

# ── Colours ───────────────────────────────────────────────────────────────────
G="\033[0;32m"; Y="\033[1;33m"; R="\033[0;31m"; B="\033[1m"; NC="\033[0m"
ok()      { echo -e "${G}[✓]${NC} $*"; }
warn()    { echo -e "${Y}[!]${NC} $*"; }
err()     { echo -e "${R}[✗]${NC} $*" >&2; }
section() { echo -e "\n${B}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n    $*\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }

REPO_URL="${F33D3R_REPO_URL:-git@github.com:F33D3R-Inc/f33d3r.git}"
BRANCH="${F33D3R_BRANCH:-main}"
APP_DIR="${F33D3R_APP_DIR:-/opt/f33d3r}"
COMPOSE_FILE="docker-compose.prod.yml"
ENV_FILE=".env.prod"

# ── 0. Who runs what ──────────────────────────────────────────────────────────
if [ "$EUID" -ne 0 ]; then
  warn "Not running as root. Will use sudo for system commands."
  SUDO="sudo"
else
  SUDO=""
fi
# The account that owns the checkout, the SSH key and the backups, and runs
# deploy.sh. Under sudo that is the invoking user, not root.
REAL_USER="${SUDO_USER:-$(id -un)}"
REAL_HOME="$(getent passwd "$REAL_USER" | cut -d: -f6)"
as_user() {
  if [ "$(id -un)" = "$REAL_USER" ]; then "$@"; else $SUDO -u "$REAL_USER" -H "$@"; fi
}

# ── 1. System packages ────────────────────────────────────────────────────────
section "1 / 9 — System packages"
$SUDO apt-get update -qq
$SUDO apt-get install -y -qq \
  git curl ca-certificates gnupg lsb-release \
  ufw openssl jq dnsutils
ok "Core packages installed"

# Remove host-level nginx/apache — Caddy (in Docker) owns ports 80 and 443
for svc in nginx apache2 lighttpd; do
  if systemctl is-active --quiet "$svc"; then
    warn "Stopping $svc (conflicts with Caddy on ports 80/443)"
    $SUDO systemctl stop "$svc"
    $SUDO systemctl disable "$svc"
    ok "$svc stopped and disabled"
  fi
done

# ── 2. Docker ─────────────────────────────────────────────────────────────────
section "2 / 9 — Docker Engine"
if command -v docker &>/dev/null; then
  ok "Docker already installed: $(docker --version | awk '{print $3}' | tr -d ',')"
else
  ok "Installing Docker (official script)…"
  curl -fsSL https://get.docker.com | $SUDO sh
  ok "Docker installed: $(docker --version | awk '{print $3}' | tr -d ',')"
fi
$SUDO docker compose version >/dev/null || { err "docker compose v2 not found: https://docs.docker.com/compose/install/"; exit 1; }
ok "Compose $($SUDO docker compose version --short)"
if [ "$REAL_USER" != "root" ]; then
  $SUDO usermod -aG docker "$REAL_USER"
  warn "Added $REAL_USER to the docker group. Log out and back in before running deploy.sh yourself."
fi

# ── 3. Firewall ───────────────────────────────────────────────────────────────
# The same list docker-compose.prod.yml publishes, and nothing else. Docker
# publishes container ports through its own iptables chain, ahead of ufw, so a
# published port is reachable whatever ufw says; keeping the two lists equal is
# what makes `ufw status` the truth about this host.
section "3 / 9 — Firewall (ufw)"
$SUDO ufw --force enable
$SUDO ufw allow 22/tcp   comment "SSH"
$SUDO ufw allow 80/tcp   comment "HTTP — Caddy Let's Encrypt challenge + redirect"
$SUDO ufw allow 443/tcp  comment "HTTPS — F33D3R"
$SUDO ufw allow 1935/tcp comment "RTMP live ingest — OBS / desktop encoders"
$SUDO ufw allow 8890/udp comment "SRT live ingest — hardware / mobile encoders"
$SUDO ufw allow 8189/udp comment "WebRTC media — browser live broadcast"
$SUDO ufw allow 7881/tcp comment "LiveKit — Frequency audio over TCP"
$SUDO ufw allow 7882/udp comment "LiveKit — Frequency audio"
$SUDO ufw reload
ok "Firewall: 22, 80, 443, 1935/tcp, 8890/udp, 8189/udp, 7881/tcp, 7882/udp"

# ── 4. SSH key for the git remote ─────────────────────────────────────────────
section "4 / 9 — SSH key for ${REPO_URL}"
GIT_HOST=""
case "$REPO_URL" in
  git@*:*) GIT_HOST="${REPO_URL#git@}"; GIT_HOST="${GIT_HOST%%:*}" ;;
  ssh://*) GIT_HOST="${REPO_URL#ssh://}"; GIT_HOST="${GIT_HOST#*@}"; GIT_HOST="${GIT_HOST%%[:/]*}" ;;
esac
if [ -n "$GIT_HOST" ]; then
  SSH_KEY="${REAL_HOME}/.ssh/id_ed25519"
  if [ ! -f "$SSH_KEY" ]; then
    as_user mkdir -p "${REAL_HOME}/.ssh"
    as_user ssh-keygen -t ed25519 -C "f33d3r-server" -f "$SSH_KEY" -N ""
    ok "Generated new SSH key: $SSH_KEY"
    echo ""
    echo -e "${B}Add this public key as a read-only deploy key on the repository${NC}"
    echo -e "${B}(GitHub: repository → Settings → Deploy keys):${NC}"
    cat "${SSH_KEY}.pub"
    echo ""
    read -r -p "Press ENTER after the key is added → "
  else
    ok "SSH key already exists: $SSH_KEY"
  fi
  # GitHub answers a successful key check with exit 1 and a greeting, so the
  # greeting is what is checked, not the exit status.
  SSH_ANSWER="$(as_user ssh -o StrictHostKeyChecking=accept-new -o BatchMode=yes -T "git@${GIT_HOST}" 2>&1 || true)"
  case "$SSH_ANSWER" in
    *"successfully authenticated"*|*"Welcome to"*) ok "SSH access to ${GIT_HOST} confirmed" ;;
    *) err "SSH access to ${GIT_HOST} failed: ${SSH_ANSWER}"; err "Add ${SSH_KEY}.pub to the repository and re-run."; exit 1 ;;
  esac
else
  ok "Remote is not SSH; no key needed"
fi

# ── 5. Clone / update repository ─────────────────────────────────────────────
section "5 / 9 — Repository (${BRANCH})"
if [ -d "$APP_DIR/.git" ]; then
  ORIGIN="$(as_user git -C "$APP_DIR" remote get-url origin)"
  if [ "$ORIGIN" != "$REPO_URL" ]; then
    err "$APP_DIR tracks $ORIGIN, not $REPO_URL."
    err "Set F33D3R_REPO_URL to the remote you mean, or: git -C $APP_DIR remote set-url origin $REPO_URL"
    exit 1
  fi
  ok "Repo already cloned at $APP_DIR — updating ${BRANCH}"
  as_user git -C "$APP_DIR" fetch origin "$BRANCH"
  as_user git -C "$APP_DIR" checkout "$BRANCH"
  as_user git -C "$APP_DIR" pull --ff-only origin "$BRANCH"
else
  ok "Cloning ${REPO_URL} (${BRANCH}) into $APP_DIR…"
  $SUDO install -d -o "$REAL_USER" -g "$(id -gn "$REAL_USER")" "$APP_DIR"
  as_user git clone --branch "$BRANCH" "$REPO_URL" "$APP_DIR"
fi
cd "$APP_DIR"
ok "On commit: $(as_user git rev-parse --short HEAD) — $(as_user git log -1 --format='%s')"

# ── 6. Public address + DNS ───────────────────────────────────────────────────
section "6 / 9 — Public address and DNS"
SERVER_IP="$(curl -fsS --max-time 5 https://api.ipify.org)" || { err "could not read this server's public IP"; exit 1; }
RESOLVED_IP="$(dig +short f33d3r.com @1.1.1.1 | tail -n1)"
ok "Server public IP:  $SERVER_IP"
ok "f33d3r.com → ${RESOLVED_IP:-unresolved}"
if [ "$SERVER_IP" = "$RESOLVED_IP" ]; then
  ok "DNS is correctly pointed at this server"
else
  warn "DNS mismatch — f33d3r.com does not resolve to this server yet."
  warn "Add an A record: f33d3r.com → $SERVER_IP"
  warn "Caddy cannot obtain a certificate, and the health check through"
  warn "https://f33d3r.com fails, until it does. Re-run bootstrap-prod.sh then."
fi

# ── 7. Environment file ───────────────────────────────────────────────────────
section "7 / 9 — Environment"
if [ ! -f "$ENV_FILE" ]; then
  as_user cp .env.prod.example "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  # Every secret the template ships as a CHANGE_ME placeholder is generated
  # here, on this host, by openssl — none of them is ever a value in git.
  PG_PASS=$(openssl rand -hex 24)
  SESSION_SECRET=$(openssl rand -base64 48 | tr -d '\n')
  # Brain-to-brain shared secret: every service carries this one value.
  INTERNAL_API_KEY=$(openssl rand -hex 32)
  MEDIA_TOKEN_SECRET=$(openssl rand -base64 48 | tr -d '\n')
  # LiveKit: signs Frequency audio join tokens and Auralis's calls to its server API
  LIVEKIT_API_KEY="frequencies_$(openssl rand -hex 6)"
  LIVEKIT_API_SECRET=$(openssl rand -hex 32)
  # Themis P-256 ECDH key (PKCS#8 DER, base64 single line). Losing this key
  # loses every previously sold CEK, which is why it is generated once here
  # and never regenerated on re-run.
  THEMIS_ECDH_PRIVKEY_B64=$(openssl ecparam -name prime256v1 -genkey -noout \
    | openssl pkcs8 -topk8 -nocrypt -outform DER | base64 -w0)

  sed -i "s|CHANGE_ME_use_a_strong_random_password|${PG_PASS}|g" "$ENV_FILE"
  sed -i "s|CHANGE_ME_generate_with_openssl_rand_-base64_48|${SESSION_SECRET}|g" "$ENV_FILE"
  sed -i "s|^INTERNAL_API_KEY=CHANGE_ME$|INTERNAL_API_KEY=${INTERNAL_API_KEY}|" "$ENV_FILE"
  sed -i "s|CHANGE_ME_media_token_generate_with_openssl_rand_-base64_48|${MEDIA_TOKEN_SECRET}|g" "$ENV_FILE"
  sed -i "s|CHANGE_ME_generate_with_openssl_ecparam|${THEMIS_ECDH_PRIVKEY_B64}|g" "$ENV_FILE"
  sed -i "s|^LIVEKIT_API_KEY=CHANGE_ME_frequencies$|LIVEKIT_API_KEY=${LIVEKIT_API_KEY}|" "$ENV_FILE"
  sed -i "s|CHANGE_ME_livekit_generate_with_openssl_rand_-hex_32|${LIVEKIT_API_SECRET}|g" "$ENV_FILE"
  sed -i "s|^LIVE_PUBLIC_HOST=CHANGE_ME_server_public_ip$|LIVE_PUBLIC_HOST=${SERVER_IP}|" "$ENV_FILE"

  ok "Secrets generated in $APP_DIR/$ENV_FILE (mode 600)"
  warn "Copy $APP_DIR/$ENV_FILE off this host now. THEMIS_ECDH_PRIVKEY_B64 in it"
  warn "unlocks every piece of sold content, and the nightly backup does not copy it."
  warn "Set SMTP_* and SECURITY_ALERT_EMAIL in it for password-reset mail and alerts."
fi

if grep -q "CHANGE_ME" "$ENV_FILE"; then
  err "$ENV_FILE still has CHANGE_ME placeholders:"
  grep -n "CHANGE_ME" "$ENV_FILE" | sed 's/=.*//' | while read -r line; do err "  $line"; done
  err "Edit it:  nano $APP_DIR/$ENV_FILE"
  exit 1
fi
INTERNAL_KEY_LEN=$(grep -E '^INTERNAL_API_KEY=' "$ENV_FILE" | head -n1 | cut -d= -f2- | tr -d '[:space:]' | wc -c)
if [ "$INTERNAL_KEY_LEN" -lt 32 ]; then
  err "INTERNAL_API_KEY in $ENV_FILE is missing or shorter than 32 characters (openssl rand -hex 32)."
  exit 1
fi
ok "$ENV_FILE ready"

# ── 8. Backup directory ───────────────────────────────────────────────────────
section "8 / 9 — Backup directory"
BACKUP_DIR="$(grep -E '^BACKUP_DIR=' "$ENV_FILE" | tail -n1 | cut -d= -f2-)"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/f33d3r}"
$SUDO install -d -m 700 -o "$REAL_USER" -g "$(id -gn "$REAL_USER")" "$BACKUP_DIR"
ok "Backups: $BACKUP_DIR (mode 700) — copy it off this host on a schedule as well"

# ── 9. Boot unit, then start ──────────────────────────────────────────────────
section "9 / 9 — Boot unit and first start"
UNIT="$(mktemp)"
cat > "$UNIT" <<SERVICE
[Unit]
Description=F33D3R platform
Requires=docker.service
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=${APP_DIR}
ExecStart=/usr/bin/docker compose -f ${COMPOSE_FILE} --env-file ${ENV_FILE} up -d --no-build
ExecStop=/usr/bin/docker compose -f ${COMPOSE_FILE} --env-file ${ENV_FILE} stop
TimeoutStartSec=600

[Install]
WantedBy=multi-user.target
SERVICE
$SUDO install -m 644 "$UNIT" /etc/systemd/system/f33d3r.service
rm -f "$UNIT"
$SUDO systemctl daemon-reload
$SUDO systemctl enable f33d3r.service
ok "Boot unit installed — F33D3R starts with the machine"

PROJECT="$($SUDO docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" config --format json | jq -er '.name')"
if $SUDO docker volume inspect "${PROJECT}_postgres_data" >/dev/null 2>&1; then
  ok "This host already has a database — nothing started."
  ok "Update with:  cd $APP_DIR && git pull --ff-only origin $BRANCH && bash deploy.sh --all"
  exit 0
fi

# bootstrap-prod.sh is the one first-start path; it runs as the checkout's
# owner through sudo, since the docker group above applies only to new logins.
$SUDO bash ./bootstrap-prod.sh

echo ""
echo -e "${B}═══════════════════════════════════════════════════════${NC}"
echo -e "${B}  F33D3R provisioning complete${NC}"
echo -e "${B}═══════════════════════════════════════════════════════${NC}"
echo ""
echo "  Site:     https://f33d3r.com — the first account to sign up is the founder"
echo "  Health:   bash $APP_DIR/deploy.sh --check"
echo "  Update:   git -C $APP_DIR pull --ff-only origin $BRANCH && bash $APP_DIR/deploy.sh --all"
echo "  Backups:  $BACKUP_DIR"
echo ""
