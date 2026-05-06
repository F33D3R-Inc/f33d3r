# Observability stack — Sprint 0 / S0.1

Prometheus + Loki + Grafana + Alertmanager, wired via `docker-compose.local.yml`.

## What lives where

| Component    | Local URL                  | Purpose                                   |
|--------------|----------------------------|-------------------------------------------|
| Prometheus   | http://localhost:9090      | Metrics scraping (15 s, 14 d retention)   |
| Grafana      | http://localhost:3000      | Dashboards. Default `admin` / `admin`     |
| Loki         | http://localhost:3100      | Log aggregation (30 d retention at T0)    |
| Alertmanager | http://localhost:9093      | Alert routing (Discord + email)           |

## Access for first-time setup

```bash
# Boot the foundation services alongside the existing stack
bash bootstrap-local.sh

# Verify Prometheus is scraping every brain
curl -s http://localhost:9090/api/v1/targets | jq '.data.activeTargets[].labels.brain' | sort -u

# Open Grafana — F33D3R Overview dashboard auto-provisioned
open http://localhost:3000

# Tail logs across all brains in real time (after S0.2 lands)
curl -s 'http://localhost:3100/loki/api/v1/tail?query={cluster="f33d3r-local"}'
```

## Brain self-instrumentation expectations (S0.2 work)

Every brain must expose:

- `GET /metrics` — Prometheus exposition format. Includes at minimum:
  - `http_requests_total{method,path,status}` — counter
  - `http_request_duration_seconds` — histogram
  - `process_cpu_seconds_total`, `process_resident_memory_bytes` — built into client libs

Every brain must emit JSON logs to stdout with at minimum:

- `time` (RFC3339), `level`, `msg`, `brain`, `request_id`, `pial_id` (when present)

Reference snippets in `docs/observability.md` (lands with S0.2).

## Capacity-trigger alerts (D-015)

These fire when the T0 → T1 migration thresholds are crossed:

| Alert                    | Threshold                               |
|--------------------------|-----------------------------------------|
| `HostCpuSustainedHigh`   | avg CPU > 60% for 4 h                   |
| `HighLatencyP95`         | p95 latency > 300 ms for 1 h            |
| `DiskUsageHigh`          | disk > 85% (CCX23 = 160 GB total)       |
| egress (manual review)   | check Hetzner console monthly until 14 TB |

When any of these fire repeatedly, open `SPRINT_LOG.md` and start the T0 → T1 migration plan.

## Loki retention tuning

Default 30 days. If disk pressure builds (CCX23 has 160 GB), reduce to 7 days by editing `loki-config.yml` `retention_period: 168h`.

## Discord webhook setup

Set in `.env.local`:

```env
DISCORD_WEBHOOK_WARNING=https://discord.com/api/webhooks/...
DISCORD_WEBHOOK_CRITICAL=https://discord.com/api/webhooks/...
DISCORD_WEBHOOK_CAPACITY=https://discord.com/api/webhooks/...
ONCALL_EMAIL=oncall@f33d3r.com
```

Defaults route to `http://localhost/disabled` (alerts get logged but not sent) so the stack boots in dev without webhooks configured.
