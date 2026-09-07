# F33D3R Brain Registry

Reference for adding new brains to the F33D3R platform.

---

## Brain naming conventions

| Thing              | Convention             | Example            |
|--------------------|------------------------|--------------------|
| Directory          | kebab-case             | `thessalon-engine` |
| Binary             | snake_case             | `thessalon_engine` |
| Docker image       | kebab-case             | `thessalon`        |
| k8s deployment     | kebab-case             | `thessalon`        |
| Kafka topic prefix | snake_case             | `commerce.events`  |
| Database name      | f33d3r\_ + snake\_case | `f33d3r_commerce`  |
| Port               | sequential from 8094   | `8094`             |
| Brain display name | Title Case             | `Thessalon`        |

---

## Adding a new brain — checklist

### Step 1: Scaffold

```bash
bash scripts/new_brain.sh <brain-name> <port>
```

This creates:

- `<brain-name>/Cargo.toml`
- `<brain-name>/src/main.rs` (health endpoint only)
- `<brain-name>/docker/Dockerfile`
- `infra/k3s/manifests/<brain-name>.yaml`
- `<brain-name>/README.md`

### Step 2: Database

Add to `postgres-init/01-create-databases.sql`:

```sql
SELECT 'CREATE DATABASE f33d3r_<name>' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_<name>')\gexec
```

Add secret key to `infra/k3s/manifests/ingress.yaml` secrets template:

```yaml
<name>-db-url: "postgres://f33d3r:CHANGEME@postgres:5432/f33d3r_<name>"
```

### Step 3: Register in enforcement layer

Add to `enforcement/dependency-graph/rules.yaml`:

```yaml
<brain-name>:
  description: "..."
  service_dir: <brain-name>
  port: <port>
  can_call:
    - schema-registry
  kafka_produce:
    - <topic>.events
  kafka_consume:
    - identity.events
  forbidden_call:
    - nantar  # list all brains this cannot call
```

Add any forbidden edges to `enforcement/dependency-graph/forbidden_edges.json`.

### Step 4: Document

- Add row to `BRAIN_MAP.md` brain registry table
- Add responsibility section to `BRAIN_MAP.md`
- Update `README.md` brain table (mark as 🔲 Planned → ✅ Active when deployed)
- Update `README-LOCAL.md` service map
- Add `<brain-name>/README.md` with full API docs

### Step 5: Docker Compose (local)

Add to `docker-compose.local.yml`:

```yaml
<brain-name>:
  build:
    context: ./<brain-name>
    dockerfile: docker/Dockerfile
  ports:
    - "<port>:<port>"
  environment:
    DATABASE_URL: "postgres://f33d3r:f33d3r@postgres:5432/f33d3r_<name>"
    RUST_LOG: "${RUST_LOG:-info}"
  depends_on:
    postgres:
      condition: service_healthy
  healthcheck:
    test: ["CMD", "curl", "-f", "http://localhost:<port>/health"]
    interval: 10s
    timeout: 3s
    retries: 3
    start_period: 30s
  networks:
    - f33d3r
```

### Step 6: GitLab CI

Add a build job to `.gitlab-ci.yml`:

```yaml
build:<brain-name>:
  <<: *build_image
  variables:
    SERVICE: <brain-name>
    IMAGE: <brain-name>
```

### Step 7: Event schemas

If the brain produces events:

1. Create `events/schemas/<topic>.<action>.json`
2. Register schema with `aethyr-schema-registry` on startup
3. Add to `events/taxonomy.md`

### Step 8: Run enforcement validator

```bash
python3 enforcement/ci-gates/graph_validation.py
python3 enforcement/contract-checker/event_schema_validator.py
python3 enforcement/contract-checker/version_policy.py
```

All must pass before opening MR.

---

## Planned brains

| Brain     | Port | DB               | Owner | Priority | Prerequisite    |
|-----------|------|------------------|-------|----------|-----------------|
| Thessalon | 8094 | f33d3r_commerce  | TBD   | High     | Ain Soph stable |
| Caeor     | 8095 | —                | TBD   | Medium   | k3s production  |
| Loxion    | 8096 | —                | TBD   | Medium   | WebRTC infra    |
| Astraon   | 8097 | f33d3r_analytics | TBD   | Low      | Kafka pipeline  |
| Auralis (Frequencies) | 8108 | f33d3r_frequencies | freq team | High | Phase 1 control plane built (`frequencies/`); Phase 3 SFU on 40000/udp |
