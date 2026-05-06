# F33D3R Deployment Guide

## Environments

| Environment | URL                        | Branch        | Deploy method  |
|-------------|----------------------------|---------------|----------------|
| Local dev   | http://localhost:8081      | any           | docker-compose |
| Staging     | https://staging.f33d3r.app | main (auto)   | GitLab CI      |
| Production  | https://f33d3r.app         | main (manual) | GitLab CI      |

---

## Local development

```bash
# First time
bash bootstrap-local.sh

# After code changes to a service
docker compose -f docker-compose.local.yml build <service>
docker compose -f docker-compose.local.yml up -d <service>

# Health check
bash scripts/health_check.sh
```

---

## Provisioning a new production server

### 1. Run Terraform

```bash
cd infra/terraform
terraform init
terraform plan -var="hcloud_token=$HCLOUD_TOKEN" \
               -var="cloudflare_api_token=$CF_TOKEN" \
               -var="cloudflare_zone_id=$CF_ZONE_ID" \
               -var="ssh_public_key=$(cat ~/.ssh/id_ed25519.pub)"
terraform apply
```

### 2. Install k3s on the master node

```bash
# SSH to master
ssh root@$(terraform output -raw master_ip)

# Run install script
bash infra/k3s/install.sh
```

### 3. Configure secrets

```bash
# On master node — fill in real values
kubectl apply -f infra/k3s/manifests/ingress.yaml
kubectl edit secret f33d3r-secrets -n f33d3r
# Update all CHANGEME values with real Postgres URLs and session secret
```

### 4. Deploy all brains

```bash
bash scripts/deploy_k3s.sh production latest
```

---

## Deploying code changes (CI/CD)

The GitLab CI pipeline handles this automatically on merge to `main`:

1. **validate** → architecture graph, version pins, Go build, Rust cargo check
2. **test** → Go tests, Rust tests (with Postgres service)
3. **build** → Docker images built and pushed to GitLab registry
4. **enforce** → API contracts, event schema coverage, dependency graph
5. **security** → Trivy image scan, secrets scan
6. **infra** → Terraform validate + plan
7. **deploy:staging** → auto-deploy to staging
8. **deploy:production** → manual approval required

---

## Manual deployment

If CI is unavailable:

```bash
# Build images locally
docker build -f feed-engine/Dockerfile -t $REGISTRY/nantar:$SHA feed-engine/
docker push $REGISTRY/nantar:$SHA

# Deploy to k3s
export KUBECONFIG=/path/to/kubeconfig
bash scripts/deploy_k3s.sh production $SHA
```

---

## Rollback

```bash
# Roll back all brains to previous revision
bash scripts/rollback.sh production

# Roll back a specific brain
bash scripts/rollback.sh production nantar
```

---

## Database migrations

Migrations are embedded in each service and run automatically on startup.

- **feed-engine** (Go): migrations in `internal/db/migrate.go` — run via `MigrateUp()` on startup
- **Rust services**: migrations in `src/db.rs` — run via `sqlx::migrate!()` macro

**Never run migrations manually in production.** Deploy the new service version and it migrates itself.

---

## Secrets management

In production, secrets are stored in Kubernetes Secrets in the `f33d3r` namespace.

```bash
# View current secrets (base64-encoded values)
kubectl get secret f33d3r-secrets -n f33d3r -o yaml

# Update a secret
kubectl patch secret f33d3r-secrets -n f33d3r \
  --type='json' \
  -p='[{"op":"replace","path":"/data/session-secret","value":"'$(echo -n "newvalue" | base64)'"}]'

# Restart services to pick up new secrets
kubectl rollout restart deployment/nantar -n f33d3r
```

In GitLab CI, secrets are stored as CI/CD Variables (masked + protected):

- `KUBECONFIG_PROD` — base64-encoded kubeconfig for production cluster
- `KUBECONFIG_STAGING` — base64-encoded kubeconfig for staging cluster
- `HCLOUD_TOKEN` — Hetzner Cloud API token
- `CLOUDFLARE_API_TOKEN` — Cloudflare DNS management token
- `GITLAB_TOKEN` — Registry deploy token for image pulls

---

## Monitoring

Check service health:

```bash
# All services in k3s
kubectl get pods -n f33d3r

# Logs for a service
kubectl logs -n f33d3r deploy/nantar --tail=100 -f

# Resource usage
kubectl top pods -n f33d3r
```

Local health check:

```bash
bash scripts/health_check.sh
```

---

## May 1 Launch Checklist

- [ ] DNS pointed at production server (f33d3r.app → Cloudflare → master node IP)
- [ ] TLS certificate issued by cert-manager (Let's Encrypt)
- [ ] All 8 brains healthy (bash scripts/health_check.sh production)
- [ ] Postgres backups configured (Hetzner managed DB or pg_dump cron)
- [ ] GITLAB_TOKEN set for image pulls in k3s cluster
- [ ] f33d3r-secrets applied with real values (not CHANGEME)
- [ ] Admin account created: `UPDATE users SET role = 'admin' WHERE handle = 'your_handle';`
- [ ] SHOW_SCORES=false in production
- [ ] Rate limits tuned for expected traffic
- [ ] Monitoring / alerting set up (optional at launch)
