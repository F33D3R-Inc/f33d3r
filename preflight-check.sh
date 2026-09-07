#!/usr/bin/env bash
# F33D3R Pre-flight check — run this if bootstrap fails
# Diagnoses the most common issues before you try to start the stack

GREEN="\033[0;32m"; RED="\033[0;31m"; YELLOW="\033[1;33m"; BOLD="\033[1m"; NC="\033[0m"
ok()   { echo -e "  ${GREEN}✓${NC} $*"; }
fail() { echo -e "  ${RED}✗${NC} $*"; ISSUES=$((ISSUES+1)); }
warn() { echo -e "  ${YELLOW}!${NC} $*"; }
ISSUES=0

echo -e "\n${BOLD}F33D3R Pre-flight Check${NC}"
echo "──────────────────────────────────────"

# 1. Service directories
for d in feed-engine aethyrrank-engine zior-engine aethyr-schema-registry; do
  if [ -d "$d" ]; then ok "$d/ found"
  else fail "$d/ NOT FOUND — unzip all 5 zips into this directory"; fi
done

# 2. Docker
if command -v docker &>/dev/null; then
  ok "Docker $(docker --version | awk '{print $3}' | tr -d ',')"
else
  fail "Docker not installed — https://docs.docker.com/get-docker/"
fi

if docker compose version &>/dev/null 2>&1; then
  ok "Docker Compose $(docker compose version --short 2>/dev/null || echo 'ok')"
else
  fail "docker compose v2 not found"
fi

# 3. Docker daemon
if docker info &>/dev/null 2>&1; then
  ok "Docker daemon running"
else
  fail "Docker daemon not running — run: sudo systemctl start docker"
fi

# 4. Kernel modules (Arch Linux / Linux specific)
if [ -f /etc/arch-release ] || uname -r | grep -q arch; then
  if [ -d "/usr/lib/modules/$(uname -r)" ]; then
    ok "Kernel modules match running kernel ($(uname -r))"
  else
    fail "KERNEL MISMATCH — reboot required after kernel update"
    echo -e "     ${YELLOW}Fix: sudo reboot${NC}"
  fi
  # Test veth specifically
  if modprobe --dry-run veth &>/dev/null 2>&1; then
    ok "veth module available"
  else
    fail "veth module missing — Docker networking will fail"
    echo -e "     ${YELLOW}Fix: sudo modprobe veth (or reboot)${NC}"
  fi
fi

# 5. Test Docker networking (the actual veth check)
echo ""
echo "Testing Docker networking..."
if docker run --rm --network bridge alpine echo "network ok" &>/dev/null 2>&1; then
  ok "Docker bridge networking works"
else
  fail "Docker bridge networking FAILED"
  echo -e "     ${YELLOW}This is the 'operation not supported' error.${NC}"
  echo -e "     ${YELLOW}Fix: reboot your machine.${NC}"
  echo -e "     ${YELLOW}     sudo reboot${NC}"
fi

# 6. Ports free?
for port in 5432 6379 8079 8080 8081 8082 8108; do
  if ss -tlnp 2>/dev/null | grep -q ":${port} " || \
     netstat -tlnp 2>/dev/null | grep -q ":${port} "; then
    warn "Port $port is already in use — may conflict"
  else
    ok "Port $port free"
  fi
done

# 7. .env.local
if [ -f .env.local ]; then ok ".env.local found"
else fail ".env.local not found — it should be in the same directory"; fi

# 8. INTERNAL_API_KEY — the brain-to-brain shared secret. Every brain, Verity,
# Ain Soph, Themis, Manhattan and feed-engine carry the one value; a missing,
# placeholder or short key shows up later as 401s between services, so it is
# checked here in every env file present.
check_internal_key() {
  local envfile="$1" key
  [ -f "$envfile" ] || return 0
  key=$(grep -E '^INTERNAL_API_KEY=' "$envfile" | head -n1 | cut -d= -f2- | tr -d '[:space:]"'"'")
  if [ -z "$key" ]; then
    fail "$envfile: INTERNAL_API_KEY is not set — run: openssl rand -hex 32"
  elif printf '%s' "$key" | grep -qiE 'CHANGE_ME|change_in_production|placeholder'; then
    fail "$envfile: INTERNAL_API_KEY is a placeholder — replace it with: openssl rand -hex 32"
  elif [ "${#key}" -lt 32 ]; then
    fail "$envfile: INTERNAL_API_KEY is ${#key} characters — needs at least 32 (openssl rand -hex 32)"
  else
    ok "$envfile: INTERNAL_API_KEY set (${#key} characters)"
  fi
}
check_internal_key .env.local
check_internal_key .env.prod

echo ""
echo "──────────────────────────────────────"
if [ $ISSUES -eq 0 ]; then
  echo -e "${GREEN}${BOLD}All checks passed. Run: bash bootstrap-local.sh${NC}"
else
  echo -e "${RED}${BOLD}$ISSUES issue(s) found. Fix them before running bootstrap.${NC}"
fi
echo ""
