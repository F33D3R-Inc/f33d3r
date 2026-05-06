#!/usr/bin/env bash
# F33D3R Health Check Script
# Usage: bash scripts/health_check.sh [environment]
#   No args = local dev (localhost)
#   environment = production | staging

set -euo pipefail

ENVIRONMENT="${1:-local}"
NAMESPACE="f33d3r"

# ── Service map ────────────────────────────────────────────────────────────────
if [[ "${ENVIRONMENT}" == "local" ]]; then
  declare -A SERVICES=(
    ["Nantar (feed-engine)"]="http://localhost:8081/api/health"
    ["AethyrRank"]="http://localhost:8080/health"
    ["Zior (music)"]="http://localhost:8082/health"
    ["Schema Registry"]="http://localhost:8079/health"
    ["Ain Soph (wallet)"]="http://localhost:8089/health"
    ["Vovin (messaging)"]="http://localhost:8092/health"
    ["Elohim Veni (PIAL)"]="http://localhost:8093/health"
    ["Zodacare (safety)"]="http://localhost:8090/health"
  )
else
  # In k3s: port-forward each service to check health
  # Or check via kubectl rollout status
  echo "Checking k3s pod readiness in namespace ${NAMESPACE}..."
  kubectl get pods -n "${NAMESPACE}" -o wide
  kubectl rollout status deployment/nantar -n "${NAMESPACE}" --timeout=60s
  kubectl rollout status deployment/aethyrrank -n "${NAMESPACE}" --timeout=60s
  kubectl rollout status deployment/elohim-veni -n "${NAMESPACE}" --timeout=60s
  kubectl rollout status deployment/zodacare -n "${NAMESPACE}" --timeout=60s
  echo ""
  echo "All deployments healthy in ${ENVIRONMENT}"
  exit 0
fi

# ── Local health checks ────────────────────────────────────────────────────────
PASS=0
FAIL=0

echo ""
echo "F33D3R Health Check — $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "─────────────────────────────────────────────────────────"

for name in "${!SERVICES[@]}"; do
  url="${SERVICES[$name]}"
  if curl -sf --max-time 3 "${url}" > /dev/null 2>&1; then
    printf "  %-30s  \033[32m✓ online\033[0m\n" "${name}"
    PASS=$((PASS + 1))
  else
    printf "  %-30s  \033[31m✗ offline\033[0m  (${url})\n" "${name}"
    FAIL=$((FAIL + 1))
  fi
done

echo "─────────────────────────────────────────────────────────"
echo "  ${PASS} online  ${FAIL} offline"
echo ""

if [[ ${FAIL} -gt 0 ]]; then
  echo "Some services are offline. Run:"
  echo "  docker compose -f docker-compose.local.yml logs <service-name>"
  exit 1
fi

echo "All services healthy."
echo "Open: http://localhost:8081"
