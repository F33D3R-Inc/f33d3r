#!/usr/bin/env bash
# F33D3R Rollback Script
# Rolls back all brain deployments to the previous revision.
# Usage: bash scripts/rollback.sh [environment] [brain]
#   brain: optional — rollback only one brain (e.g. nantar)

set -euo pipefail

ENVIRONMENT="${1:-production}"
TARGET_BRAIN="${2:-all}"
NAMESPACE="f33d3r"

log()  { echo "[$(date -u '+%H:%M:%S')] ROLLBACK | $*"; }
ok()   { echo "[$(date -u '+%H:%M:%S')] ✓ $*"; }
fail() { echo "[$(date -u '+%H:%M:%S')] ✗ FAILED: $*" >&2; exit 1; }

BRAINS=(nantar aethyrrank zior ain-soph elohim-veni zodacare schema-registry)

log "Rolling back F33D3R in ${ENVIRONMENT}"
[[ "${TARGET_BRAIN}" != "all" ]] && log "Target brain: ${TARGET_BRAIN}"

if [[ "${TARGET_BRAIN}" == "all" ]]; then
  for brain in "${BRAINS[@]}"; do
    log "Rolling back ${brain}..."
    kubectl rollout undo deployment/"${brain}" -n "${NAMESPACE}" \
      && ok "${brain} rolled back" \
      || log "WARNING: ${brain} rollback failed"
  done
else
  log "Rolling back ${TARGET_BRAIN}..."
  kubectl rollout undo deployment/"${TARGET_BRAIN}" -n "${NAMESPACE}" \
    && ok "${TARGET_BRAIN} rolled back" \
    || fail "${TARGET_BRAIN} rollback failed"
fi

log "Waiting for rollback to complete..."
for brain in "${BRAINS[@]}"; do
  [[ "${TARGET_BRAIN}" != "all" && "${brain}" != "${TARGET_BRAIN}" ]] && continue
  kubectl rollout status deployment/"${brain}" -n "${NAMESPACE}" --timeout=120s \
    && ok "${brain} stable" \
    || log "WARNING: ${brain} may be unstable after rollback"
done

log "Rollback complete. Current state:"
kubectl get pods -n "${NAMESPACE}" -o wide
