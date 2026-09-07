# Observability Stack — S0.1 Complete

Prometheus + Loki + Grafana + Alertmanager + node-exporter + Promtail.  
All services live in `docker-compose.local.yml`. Provisioning is file-based and auto-applied at boot.

## What lives where

| Component | Local URL | Credentials | Purpose |
|---|---|---|---|
| Grafana | http://localhost:3000 | admin / `f33d3rdev_change_in_prod` | Dashboards — F33D3R Overview auto-provisioned |
| Prometheus | http://localhost:9090 | — | Metrics scraping (15 s, 14-day retention) |
| Loki | http://localhost:3100 | — | Log aggregation (30-day retention at T0) |
| Alertmanager | http://localhost:9093 | — | Alert routing (Discord + email) |
| MinIO Console | http://localhost:9001 | `f33d3r_minio` / see `.env.local` | Object storage, includes `backups/` bucket |
| node-exporter | http://localhost:9100 | — | Host CPU/RAM/disk/network metrics |

## Quick checks

```bash
# Verify Prometheus targets
curl -s http://localhost:9090/api/v1/targets \
  | python3 -c "import sys,json; t=json.load(sys.stdin); [print(x['labels'].get('job'), x['health']) for x in t['data']['activeTargets']]" | sort

# Open Grafana dashboard
open http://localhost:3000/d/f33d3r-overview

# Tail all brain logs from Loki
curl -G -s 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={cluster="f33d3r-local"}' \
  --data-urlencode 'limit=50' | python3 -c "import sys,json; r=json.load(sys.stdin); [print(s[0]) for stream in r['data']['result'] for s in stream['values']]"
```

## Brain self-instrumentation (S0.2)

Every brain exposes `GET /metrics` (Prometheus format) via `src/observ.rs` (Rust) or `internal/observ/` (Go):

- `http_requests_total{brain,method,path,status}` — counter
- `http_request_duration_seconds{brain,method,path,status}` — histogram
- `http_requests_in_flight{brain}` — gauge
- `process_cpu_seconds_total`, `process_resident_memory_bytes` — via ProcessCollector

Every brain emits JSON logs to stdout with: `time`, `level`, `brain`, `request_id`, `pial_id`.

Pattern: `infra/observability/rust-pattern.md`

## Provisioning

Grafana datasources and dashboards are provisioned at boot from:
```
infra/observability/grafana/
├── grafana.ini                        # Disables Grafana 13 GitOps provisioning (D-032)
├── provisioning/
│   ├── datasources/datasources.yml   # Prometheus (uid=prometheus) + Loki (uid=loki)
│   └── dashboards/dashboards.yml     # Points to /var/lib/grafana/dashboards
└── dashboards/
    └── f33d3r_overview.json          # Full F33D3R Overview dashboard
```

**Important:** Grafana 13 has a new GitOps provisioning system (`provisioning=true` feature flag) that is incompatible with classic YAML file provisioning. It is disabled in `grafana.ini`. Do not enable it without migrating all provisioning files (D-032).

## Alert rules

Defined in `prometheus-rules.yml`:

| Alert | Threshold | Severity |
|---|---|---|
| BrainDown | brain target `up == 0` for 2 min | critical |
| HighRequestErrorRate | 5xx rate > 1% for 10 min | warning |
| HighLatencyP95 | p95 > 300ms for 1h | warning (T0→T1 trigger) |
| HostCpuSustainedHigh | avg CPU > 60% for 4h | warning (T0→T1 trigger) |
| PgBouncerDown | pgbouncer target `up == 0` for 1 min | critical |
| MinioDown | minio target `up == 0` for 2 min | critical |
| DiskUsageHigh | disk used > 85% | warning |

## Log shipping (Promtail)

Promtail runs alongside Loki and ships all container stdout/stderr via the Docker socket. Config: `promtail-config.yml`.

Labels extracted per container:
- `brain` — from container name pattern `f33d3r-local-<brain>-N`
- `container` — full container name
- `logstream` — stdout/stderr
- `cluster` — always `f33d3r-local`
- `level` — extracted from JSON logs when present

## Capacity-trigger alerts (D-015)

When `HostCpuSustainedHigh` or `HighLatencyP95` fire repeatedly → open `SPRINT_LOG.md` and start the T0→T1 migration.

## Discord webhook setup

In `.env.local`:
```env
DISCORD_WEBHOOK_WARNING=https://discord.com/api/webhooks/...
DISCORD_WEBHOOK_CRITICAL=https://discord.com/api/webhooks/...
ONCALL_EMAIL=oncall@f33d3r.com
```

Defaults to `http://localhost/disabled` so the stack boots without webhooks configured.
