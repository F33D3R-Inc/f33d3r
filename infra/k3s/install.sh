#!/usr/bin/env bash
# F33D3R k3s Production Install Script
# Run on a fresh server as root.
# Tested on: Ubuntu 22.04 LTS, Debian 12

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-f33d3r-prod}"
K3S_VERSION="${K3S_VERSION:-v1.31.3+k3s1}"
REGISTRY="registry.gitlab.com/f33d3r-core/platform"

log() { echo "[$(date -u '+%H:%M:%S')] $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "Run as root"

log "Installing k3s ${K3S_VERSION} on ${CLUSTER_NAME}..."

# System prep
apt-get update -qq
apt-get install -y -qq curl wget jq openssl

# Disable swap (k3s requirement)
swapoff -a
sed -i '/swap/d' /etc/fstab

# Set kernel params for container networking
cat >> /etc/sysctl.conf <<EOF
net.ipv4.ip_forward=1
net.bridge.bridge-nf-call-iptables=1
net.bridge.bridge-nf-call-ip6tables=1
EOF
sysctl -p

# Install k3s (single-node server)
curl -sfL https://get.k3s.io | \
  INSTALL_K3S_VERSION="${K3S_VERSION}" \
  INSTALL_K3S_EXEC="server \
    --cluster-name=${CLUSTER_NAME} \
    --disable=traefik \
    --write-kubeconfig-mode=644 \
    --tls-san=$(curl -sf https://ifconfig.me) \
  " sh -

log "Waiting for k3s to be ready..."
timeout 60 bash -c 'until kubectl get nodes 2>/dev/null | grep -q " Ready"; do sleep 2; done'
log "k3s node ready"

# Install Helm
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# Install Nginx Ingress Controller
log "Installing Nginx Ingress..."
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx
helm repo update
helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx \
  --create-namespace \
  --set controller.service.type=LoadBalancer \
  --wait

# Install cert-manager (TLS)
log "Installing cert-manager..."
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --set installCRDs=true \
  --wait

# Install Longhorn (distributed storage for uploads PVC)
log "Installing Longhorn storage..."
helm repo add longhorn https://charts.longhorn.io
helm repo update
helm upgrade --install longhorn longhorn/longhorn \
  --namespace longhorn-system \
  --create-namespace \
  --wait

# Apply namespace
kubectl apply -f infra/k3s/manifests/namespaces.yaml

# Create GitLab registry secret (requires GITLAB_TOKEN env var)
if [[ -n "${GITLAB_TOKEN:-}" ]]; then
  kubectl create secret docker-registry gitlab-registry-secret \
    --docker-server=registry.gitlab.com \
    --docker-username=deploy-token \
    --docker-password="${GITLAB_TOKEN}" \
    --namespace f33d3r \
    --dry-run=client -o yaml | kubectl apply -f -
  log "GitLab registry secret created"
else
  log "WARNING: GITLAB_TOKEN not set — registry secret not created"
  log "Run: kubectl create secret docker-registry gitlab-registry-secret ..."
fi

# Print cluster info
log ""
log "k3s installation complete!"
log ""
log "Cluster: ${CLUSTER_NAME}"
log "kubectl get nodes:"
kubectl get nodes
log ""
log "KUBECONFIG: /etc/rancher/k3s/k3s.yaml"
log ""
log "Next steps:"
log "  1. Set DNS: f33d3r.app → $(kubectl get svc -n ingress-nginx ingress-nginx-controller -o jsonpath='{.status.loadBalancer.ingress[0].ip}')"
log "  2. Create f33d3r-secrets: kubectl apply -f infra/k3s/manifests/ingress.yaml"
log "  3. Deploy brains: bash scripts/deploy_k3s.sh production <commit-sha>"
