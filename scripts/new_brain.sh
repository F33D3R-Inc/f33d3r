#!/usr/bin/env bash
# F33D3R New Brain Scaffolder
# Creates the full directory structure for a new brain service.
# Usage: bash scripts/new_brain.sh <brain-name> <port>
#   brain-name: kebab-case service name (e.g. thessalon-engine)
#   port:       service port (e.g. 8094)

set -euo pipefail

BRAIN="${1:?Usage: new_brain.sh <brain-name> <port>}"
PORT="${2:?Usage: new_brain.sh <brain-name> <port>}"
DB_NAME="f33d3r_${BRAIN//-/_}"

log() { echo "[new_brain] $*"; }

log "Scaffolding brain: ${BRAIN} (port ${PORT})"

# ── Create directory structure ────────────────────────────────────────────────
mkdir -p "${BRAIN}/src" "${BRAIN}/docker"

# ── Cargo.toml ────────────────────────────────────────────────────────────────
cat > "${BRAIN}/Cargo.toml" <<EOF
[package]
name    = "${BRAIN}"
version = "1.0.0"
edition = "2021"

[[bin]]
name = "${BRAIN//-/_}"
path = "src/main.rs"

[dependencies]
axum               = { version = "0.7", features = ["json"] }
tokio              = { version = "1",   features = ["full"] }
tower-http         = { version = "0.5", features = ["cors", "trace"] }
serde              = { version = "1",   features = ["derive"] }
serde_json         = "1"
sqlx               = { version = "0.8", features = ["runtime-tokio-rustls", "postgres", "uuid", "chrono"] }
uuid               = { version = "1",   features = ["v4", "serde"] }
chrono             = { version = "0.4", features = ["serde"] }
tracing            = "0.1"
tracing-subscriber = { version = "0.3", features = ["env-filter", "fmt"] }
anyhow             = "1"

[profile.release]
opt-level = 3
lto       = true
codegen-units = 1
EOF

# ── main.rs ───────────────────────────────────────────────────────────────────
cat > "${BRAIN}/src/main.rs" <<EOF
use axum::{routing::get, Json, Router};
use serde_json::{json, Value};
use std::net::SocketAddr;
use tracing::info;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(std::env::var("RUST_LOG").unwrap_or_else(|_| "info".to_string()))
        .init();

    let app = Router::new().route("/health", get(health));

    let addr = SocketAddr::from(([0, 0, 0, 0], ${PORT}));
    info!("{} listening on {}", env!("CARGO_PKG_NAME"), addr);

    let listener = tokio::net::TcpListener::bind(addr).await?;
    axum::serve(listener, app).await?;
    Ok(())
}

async fn health() -> Json<Value> {
    Json(json!({
        "status":  "ok",
        "service": env!("CARGO_PKG_NAME"),
    }))
}
EOF

# ── Dockerfile ────────────────────────────────────────────────────────────────
cat > "${BRAIN}/docker/Dockerfile" <<EOF
# ── Stage 1: build ─────────────────────────────────────────────────────────────
FROM rust:1.95.0-slim-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends pkg-config libssl-dev && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY Cargo.toml Cargo.lock* ./
RUN mkdir src && echo "fn main(){}" > src/main.rs
RUN cargo build --release 2>&1 | tail -5

COPY src ./src
RUN touch src/main.rs && cargo build --release 2>&1 | tail -5

# ── Stage 2: minimal runtime ───────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update \\
    && apt-get install -y --no-install-recommends ca-certificates curl \\
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/target/release/${BRAIN//-/_} .

EXPOSE ${PORT}
ENV RUST_LOG=info

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \\
    CMD curl -f http://localhost:${PORT}/health || exit 1

CMD ["./${BRAIN//-/_}"]
EOF

# ── k3s manifest ──────────────────────────────────────────────────────────────
cat > "infra/k3s/manifests/${BRAIN}.yaml" <<EOF
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${BRAIN}
  namespace: f33d3r
  labels:
    brain: ${BRAIN}
spec:
  replicas: 1
  selector:
    matchLabels:
      brain: ${BRAIN}
  template:
    metadata:
      labels:
        brain: ${BRAIN}
    spec:
      containers:
        - name: ${BRAIN}
          image: registry.gitlab.com/f33d3r-core/platform/${BRAIN}:latest
          imagePullPolicy: Always
          ports:
            - containerPort: ${PORT}
          env:
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: f33d3r-secrets
                  key: ${BRAIN//-/_}-db-url
            - name: RUST_LOG
              value: "info"
          resources:
            requests:
              cpu: "100m"
              memory: "128Mi"
            limits:
              cpu: "500m"
              memory: "256Mi"
          livenessProbe:
            httpGet:
              path: /health
              port: ${PORT}
            initialDelaySeconds: 10
            periodSeconds: 10
      imagePullSecrets:
        - name: gitlab-registry-secret

---
apiVersion: v1
kind: Service
metadata:
  name: ${BRAIN}
  namespace: f33d3r
  labels:
    brain: ${BRAIN}
spec:
  selector:
    brain: ${BRAIN}
  ports:
    - port: ${PORT}
      targetPort: ${PORT}
EOF

# ── README ────────────────────────────────────────────────────────────────────
cat > "${BRAIN}/README.md" <<EOF
# ${BRAIN^} Brain

**Port:** ${PORT}
**Database:** ${DB_NAME} (PostgreSQL 16)
**Language:** Rust 1.95.0

TODO: describe this brain's purpose and API.

## Running locally

\`\`\`bash
docker compose -f ../docker-compose.local.yml up -d ${BRAIN}
curl http://localhost:${PORT}/health
\`\`\`
EOF

log ""
log "Scaffolded ${BRAIN} successfully."
log ""
log "Next steps:"
log "  1. Add to docker-compose.local.yml"
log "  2. Add database '${DB_NAME}' to postgres-init/01-create-databases.sql"
log "  3. Add to enforcement/dependency-graph/rules.yaml"
log "  4. Add to BRAIN_MAP.md and README.md brain tables"
log "  5. Implement src/main.rs"
