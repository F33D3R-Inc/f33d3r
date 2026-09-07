#!/usr/bin/env bash
# F33D3R k3s Deployment Script
# Usage: bash scripts/deploy_k3s.sh <environment> <commit-sha>
#   environment: production | staging
#   commit-sha:  git commit SHA for image tags

set -euo pipefail

ENVIRONMENT="${1:?Usage: deploy_k3s.sh <environment> <commit-sha>}"
COMMIT_SHA="${2:?Usage: deploy_k3s.sh <environment> <commit-sha>}"
REGISTRY="registry.gitlab.com/f33d3r-core/platform"
NAMESPACE="f33d3r"

log()  { echo "[$(date -u '+%H:%M:%S')] DEPLOY | $*"; }
ok()   { echo "[$(date -u '+%H:%M:%S')] ✓ $*"; }
fail() { echo "[$(date -u '+%H:%M:%S')] ✗ FAILED: $*" >&2; exit 1; }

log "Deploying F33D3R to ${ENVIRONMENT} @ ${COMMIT_SHA}"
log "Registry: ${REGISTRY}"

# ── Validate kubeconfig ────────────────────────────────────────────────────────
kubectl cluster-info > /dev/null 2>&1 || fail "kubectl not configured"

# ── Apply namespace and network policies ──────────────────────────────────────
log "Applying namespaces..."
kubectl apply -f infra/k3s/manifests/namespaces.yaml

log "Applying network policies..."
kubectl apply -f enforcement/runtime-guard/service_mesh_policy.yaml

# ── Update image tags and deploy ──────────────────────────────────────────────
BRAINS=(
  "nantar:feed-engine"
  "aethyrrank:aethyrrank-engine"
  "zior:zior-engine"
  "ain-soph:ain-soph"
  "elohim-veni:elohim-veni"
  "zodacare:zodacare"
  "schema-registry:aethyr-schema-registry"
)

log "Updating image tags to ${COMMIT_SHA}..."
for entry in "${BRAINS[@]}"; do
  deployment="${entry%%:*}"
  image_name="${entry##*:}"
  # schema-registry maps to different image name
  if [[ "${deployment}" == "schema-registry" ]]; then
    image_name="schema-registry"
  fi
  kubectl set image \
    deployment/"${deployment}" \
    "${deployment}=${REGISTRY}/${image_name}:${COMMIT_SHA}" \
    -n "${NAMESPACE}" \
    || log "WARNING: ${deployment} deployment not found (may not be deployed yet)"
done

# ── Apply manifests ────────────────────────────────────────────────────────────
log "Applying k3s manifests..."
kubectl apply -f infra/k3s/manifests/brains.yaml
kubectl apply -f infra/k3s/manifests/feed-engine.yaml
kubectl apply -f infra/k3s/manifests/ingress.yaml

# ── Wait for rollout ───────────────────────────────────────────────────────────
log "Waiting for rollouts to complete (timeout: 5m)..."

DEPLOYMENTS=(nantar aethyrrank zior ain-soph elohim-veni zodacare schema-registry)
for dep in "${DEPLOYMENTS[@]}"; do
  kubectl rollout status deployment/"${dep}" -n "${NAMESPACE}" --timeout=300s \
    && ok "${dep} deployed" \
    || fail "${dep} rollout failed — check: kubectl logs -n ${NAMESPACE} deploy/${dep}"
done

# ── Health checks ──────────────────────────────────────────────────────────────
log "Running health checks..."
bash scripts/health_check.sh "${ENVIRONMENT}"

log ""
log "Deployment complete: ${ENVIRONMENT} @ ${COMMIT_SHA}"
log "App: https://$([ "${ENVIRONMENT}" = "production" ] && echo "f33d3r.app" || echo "staging.f33d3r.app")"
