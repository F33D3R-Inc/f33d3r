#!/usr/bin/env bash
# F33D3R local health check
GREEN="\033[0;32m"; RED="\033[0;31m"; YELLOW="\033[1;33m"; BOLD="\033[1m"; NC="\033[0m"
echo -e "\n${BOLD}F33D3R Health — $(date '+%H:%M:%S')${NC}"
echo "─────────────────────────────────────"
ONLINE=0; OFFLINE=0
check() {
  local name="$1" port="$2" path="${3:-/health}"
  local code
  code=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 2 --max-time 4 \
    "http://localhost:${port}${path}" 2>/dev/null || echo "000")
  if [ "$code" = "200" ]; then
    printf "  ${GREEN}●${NC} %-32s :%-5s ${GREEN}online${NC}\n" "$name" "$port"
    ONLINE=$((ONLINE+1))
  else
    printf "  ${RED}○${NC} %-32s :%-5s ${RED}offline${NC} (%s)\n" "$name" "$port" "$code"
    OFFLINE=$((OFFLINE+1))
  fi
}
# One line per service docker-compose.local.yml publishes a host port for,
# probing the same path its compose healthcheck does.
check "aethyr-schema-registry" 8079
check "aethyrrank-engine"       8080
check "feed-engine"             8081 "/api/health"
check "zior-engine"             8082
check "transcoding"             8085
check "caeor"                   8086
check "astraon"                 8088
check "ain-soph"                8089
check "zodacare"                8090
check "live-edge"               8092 "/healthz"
check "elohim-veni"             8093
check "registry-brain"          8094
check "verity"                  8095
check "aethyr-ledger"           8096
check "content-scan"            8097 "/ready"
check "alexandria"              8098
check "ekyc"                    8099
check "themis"                  8100
check "herald"                  8105
check "lore"                    8106
check "manhattan"               8107
check "auralis"                 8108
check "minio"                   9000 "/minio/health/live"
check "prometheus"              9090 "/-/healthy"
check "alertmanager"            9093 "/-/healthy"
check "loki"                    3100 "/ready"
check "grafana"                 3000 "/api/health"
echo "─────────────────────────────────────"
echo -e "  ${GREEN}${ONLINE} online${NC}  ${RED}${OFFLINE} offline${NC}\n"
if [ "$OFFLINE" -gt 0 ]; then
  echo -e "${YELLOW}  Tip: docker compose -f docker-compose.local.yml logs <service>${NC}\n"
fi
