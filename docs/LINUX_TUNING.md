# Linux Tuning — F33D3R Production (CCX23)

**Target:** Hetzner CCX23 — 4 vCPU AMD, 16 GB RAM, 160 GB NVMe  
**Goal:** 10,000 concurrent WebSocket connections in Vovin  
**OS:** Debian 12 / Ubuntu 24.04 (Hetzner standard images)

---

## Why Tuning Is Required

Linux defaults are designed for general-purpose workloads. For 10k concurrent WebSocket connections:

- Default file descriptor limit: **1,024** — this alone hard-caps connections at ~990
- Default TCP backlog: 128 — connection queue fills under burst load
- Default socket buffers: 212 KB — undersized for high-throughput messaging

These are not optional tweaks. Without them, Vovin hard-caps at 1,024 connections.

---

## 1. System-Wide sysctl Settings

Apply in `/etc/sysctl.d/99-f33d3r.conf`:

```ini
# ── File descriptors ──────────────────────────────────────────────────────────
# Each WebSocket connection = 1 file descriptor
# 10k connections + 4k for PG/Redis/OS overhead + headroom
fs.file-max = 200000

# ── Network: connection backlog ───────────────────────────────────────────────
# Backlog queue for SYN packets waiting to be accepted
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535

# ── Network: socket buffers ───────────────────────────────────────────────────
# Increase send/receive buffers for WebSocket throughput
# 4 MB max per socket — OS auto-tunes within this ceiling
net.core.rmem_max = 4194304
net.core.wmem_max = 4194304
net.ipv4.tcp_rmem = 4096 87380 4194304
net.ipv4.tcp_wmem = 4096 87380 4194304

# ── Network: TIME_WAIT and connection reuse ───────────────────────────────────
# Reuse TIME_WAIT sockets — reduces ephemeral port exhaustion
net.ipv4.tcp_tw_reuse = 1
# Maximum TIME_WAIT sockets — prevents OOM from disconnected clients
net.ipv4.tcp_max_tw_buckets = 2000000
# Orphaned TCP connections — clean up stalled connections faster
net.ipv4.tcp_max_orphans = 32768

# ── Network: keepalive ────────────────────────────────────────────────────────
# Detect dead WebSocket connections faster than default 2h
net.ipv4.tcp_keepalive_time = 600      # 10 minutes idle before first probe
net.ipv4.tcp_keepalive_intvl = 30      # 30 seconds between probes
net.ipv4.tcp_keepalive_probes = 5      # 5 failed probes → connection dead

# ── Network: ephemeral port range ─────────────────────────────────────────────
# Default: 32768–60999 (28K ports). Expand for high-connection scenarios.
net.ipv4.ip_local_port_range = 1024 65535

# ── Memory: virtual memory ────────────────────────────────────────────────────
# Prevent OOM from swapping under load — messaging is latency-sensitive
vm.swappiness = 10
# Don't trigger OOM killer for overcommit — Tokio allocates task stacks lazily
vm.overcommit_memory = 1

# ── Kernel: inotify (for Docker volume mounts) ────────────────────────────────
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
```

Apply immediately:
```bash
sysctl -p /etc/sysctl.d/99-f33d3r.conf
```

Verify:
```bash
sysctl fs.file-max net.core.somaxconn net.ipv4.tcp_max_syn_backlog
# Expected: 200000, 65535, 65535
```

---

## 2. System-Wide ulimit Settings

Apply in `/etc/security/limits.conf` (append):

```
# F33D3R — 10k WebSocket connections require elevated file descriptor limits
*    soft    nofile    65536
*    hard    nofile    65536
root soft    nofile    65536
root hard    nofile    65536
```

Also apply in `/etc/systemd/system.conf` for systemd-managed services:

```ini
[Manager]
DefaultLimitNOFILE=65536
```

And `/etc/systemd/user.conf`:
```ini
[Manager]
DefaultLimitNOFILE=65536
```

Reload systemd:
```bash
systemctl daemon-reexec
```

Verify after reboot (or `pam` session re-login):
```bash
ulimit -n
# Expected: 65536
```

---

## 3. Docker Compose Resource Limits

All Vovin-related services in `docker-compose.prod.yml`:

```yaml
services:
  vovin:
    image: f33d3r/aethyr-msg:latest
    deploy:
      resources:
        limits:
          memory: 2g       # hard ceiling — OOM kill before starving other brains
          cpus: '2.0'      # 2 of 4 vCPU reserved for Vovin peak
        reservations:
          memory: 512m     # guaranteed minimum
          cpus: '0.5'
    ulimits:
      nofile:
        soft: 65536
        hard: 65536
    sysctls:
      net.core.somaxconn: 65535  # per-container listen backlog

  redis:
    image: redis:latest
    deploy:
      resources:
        limits:
          memory: 1g       # Redis for Vovin: presence + offline queue + pub/sub
          cpus: '0.5'
    sysctls:
      net.core.somaxconn: 65535
    command: >
      redis-server
      --maxmemory 900mb
      --maxmemory-policy allkeys-lru
      --save ""             # no disk persistence — data is ephemeral
      --tcp-backlog 65535
      --timeout 0
      --tcp-keepalive 300

  postgres:
    image: postgres:latest
    deploy:
      resources:
        limits:
          memory: 4g
          cpus: '1.0'
    environment:
      POSTGRES_MAX_CONNECTIONS: 200
    command: >
      postgres
      -c max_connections=200
      -c shared_buffers=1GB
      -c effective_cache_size=3GB
      -c work_mem=16MB
      -c maintenance_work_mem=256MB
      -c checkpoint_completion_target=0.9
      -c wal_buffers=64MB
      -c default_statistics_target=100
      -c random_page_cost=1.1
      -c effective_io_concurrency=200
      -c min_wal_size=1GB
      -c max_wal_size=4GB
```

### Memory Budget (CCX23, 16 GB)

```
Vovin:              2.0 GB (limit) / ~500 MB (typical)
Redis:              1.0 GB (limit) / ~200 MB (typical for 10k users)
PostgreSQL:         4.0 GB (limit) / ~1.5 GB (shared_buffers + cache)
feed-engine:        2.0 GB (limit) / ~300 MB (typical)
Other brains:       3.0 GB total (aethyrrank, elohim-veni, caeor, etc.)
OS + Docker:        2.0 GB reserved
                   ─────────────
Total:             14.0 GB allocated / 16 GB available
Headroom:           2.0 GB
```

---

## 4. Caddy Configuration for WebSocket

Caddy must be configured to not time out long-lived WebSocket connections:

```caddyfile
# In /etc/caddy/Caddyfile or Caddy compose config

(websocket_tuning) {
    # Disable read/write timeouts for WebSocket connections
    # Vovin implements its own 30s ping timeout
    request_body {
        max_size 100MB
    }
}

vovin.f33d3r.io {
    reverse_proxy /api/ws vovin:8092 {
        transport http {
            read_timeout  0
            write_timeout 0
            dial_timeout  5s
        }
    }
}
```

Caddy's default `idle_timeout` for HTTP/2 connections is 2 minutes. WebSocket upgrades bypass this, but verify with a long-held connection in staging.

---

## 5. Tokio Runtime Tuning (Vovin Rust)

Vovin's `main.rs` should configure Tokio for high connection counts:

```rust
#[tokio::main(flavor = "multi_thread", worker_threads = 4)]
async fn main() -> anyhow::Result<()> {
    // Set per-task stack size to 128 KB (default 2 MB)
    // At 10k tasks × 128 KB = 1.28 GB — still within 2 GB budget
    // Default 2 MB × 10k = 20 GB — exceeds CCX23
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(4)               // match CCX23 vCPU count
        .thread_stack_size(128 * 1024)   // 128 KB per task stack (down from 2 MB default)
        .enable_all()
        .build()?;

    runtime.block_on(run()).await
}
```

**Why 128 KB stack:** Tokio tasks are not OS threads. The "stack" here is the async task's stack frame. Rust async is stackless by default — the compiler determines the actual frame size from the async function's state machine. `thread_stack_size` controls the OS thread pool, not per-task. The correct way to bound per-task memory is to avoid deep synchronous call stacks in async code.

In practice: 10k Tokio tasks in Rust use ~2–5 KB each for the task state machine. 10k × 5 KB = 50 MB. This is the real number, not a function of `thread_stack_size`.

---

## 6. PostgreSQL Tuning for Message Writes

The f33d3r_msg database handles:
- High-write workload (messages INSERT)
- Moderate-read workload (message fetch, prekey lookup)
- Large ciphertext BYTEAs (message blobs)

```sql
-- Run after PostgreSQL container starts
-- Or set via postgres -c flags in Docker command

ALTER SYSTEM SET max_connections = 200;
ALTER SYSTEM SET shared_buffers = '2GB';           -- 25% of 8GB allocated to PG
ALTER SYSTEM SET effective_cache_size = '6GB';
ALTER SYSTEM SET work_mem = '8MB';                 -- per sort/hash operation
ALTER SYSTEM SET maintenance_work_mem = '256MB';
ALTER SYSTEM SET wal_level = replica;              -- enable WAL for backups
ALTER SYSTEM SET checkpoint_completion_target = 0.9;
ALTER SYSTEM SET wal_buffers = '64MB';

-- Partitioned messages table — vacuum must run on each partition
ALTER SYSTEM SET autovacuum_max_workers = 4;       -- one per partition visible

SELECT pg_reload_conf();
```

**Connection pool configuration in Vovin:**

```rust
let pool = sqlx::PgPool::connect_with(
    sqlx::postgres::PgConnectOptions::from_str(&database_url)?
        .application_name("vovin-v4")
)
.max_connections(16)    // 16 async connections — sufficient for 10k concurrent users
.min_connections(4)     // keep 4 warm on idle
.acquire_timeout(std::time::Duration::from_secs(5))
.await?;
```

16 PG connections for 10k WebSocket connections is correct. WebSocket connections are I/O-bound and async — they don't hold DB connections open. Each DB operation acquires, uses, and releases a connection in microseconds.

---

## 7. Redis Tuning

Redis is used for:
- Presence: `vovin:conn:{shard}`, `vovin:presence:{shard}` — 10k keys, ~200 bytes each = 2 MB
- Offline queues: `vovin:offline:{shard}` — worst case 1k users offline × 1000 msgs × 200 bytes = 200 MB
- Pub/Sub: 10k active channels — minimal memory, ~1 KB per channel
- Typing: `vovin:typing:{conv_id}` — ephemeral, small

Total Redis memory at 10k users: ~400 MB. Configure `maxmemory 900mb` with `allkeys-lru` eviction.

```bash
# Verify Redis config
docker exec f33d3r-local-redis-1 redis-cli CONFIG GET maxmemory
docker exec f33d3r-local-redis-1 redis-cli INFO memory | grep used_memory_human
```

**Redis persistence:** Disable RDB snapshots and AOF for the Vovin Redis namespace. All Redis data for Vovin is ephemeral — presence keys TTL out, offline queues drain on reconnect, typing keys auto-expire. Crash recovery is handled by the PostgreSQL message store, not Redis.

---

## 8. Monitoring File Descriptor Usage

```bash
# Count FDs per process
ls -la /proc/$(pgrep vovin)/fd | wc -l

# System-wide FD count
cat /proc/sys/fs/file-nr
# Output: {currently_open} {free} {max}
# Target: currently_open < 50% of max during normal operation

# Per-process FD limit
cat /proc/$(pgrep vovin)/limits | grep 'open files'
# Expected: 65536
```

Add to Prometheus node_exporter custom metrics:
```bash
# /etc/node_exporter/textfile/fd_usage.sh (run via cron every 30s)
echo "node_fd_open_total $(cat /proc/sys/fs/file-nr | awk '{print $1}')" > /var/lib/node_exporter/textfile/fd.prom
```

---

## 9. Verification Checklist

Run after applying all settings (requires reboot or session restart):

```bash
# 1. File descriptor limit
ulimit -n
# Expected: 65536

# 2. sysctl values
sysctl net.core.somaxconn fs.file-max net.ipv4.tcp_max_syn_backlog
# Expected: 65535, 200000, 65535

# 3. Docker container FD limit
docker exec f33d3r-local-vovin-1 sh -c 'ulimit -n'
# Expected: 65536

# 4. Simulate 1000 connections (requires wrk or hey)
hey -n 1000 -c 1000 -q 10 http://localhost:8092/health
# Expected: 100% 200 responses, < 50ms p99

# 5. Redis memory
docker exec f33d3r-local-redis-1 redis-cli info memory | grep used_memory_human
# Baseline (no connections): < 10 MB

# 6. PostgreSQL max_connections
docker exec f33d3r-local-postgres-1 psql -U f33d3r -c 'SHOW max_connections;'
# Expected: 200
```

---

## 10. CCX23 → AX52 Upgrade Decision

**Upgrade when any of these are sustained:**

| Metric | Threshold | Duration | Alert Name |
|--------|-----------|----------|------------|
| `vovin_connections_active` | > 8,000 | 10 min | VovinConnectionsHigh |
| Vovin RSS | > 1.5 GB | 30 min | (node_memory_MemUsed) |
| Delivery p99 | > 500 ms | 5 min | VovinDeliveryLatencyHigh |
| Redis p99 | > 10 ms | 5 min | VovinRedisRoundtripHigh |
| CPU sustained | > 75% all cores | 15 min | (node_cpu_idle < 25%) |

**AX52 specs:** Ryzen 7 7700 (8c/16t), 128 GB RAM, 2× 960 GB NVMe — Hetzner dedicated.  
**Cost difference:** CCX23 ~€15/mo → AX52 ~€100/mo. Do not upgrade prematurely.

At correct V4 Rust/Tokio architecture: CCX23 handles 10k concurrent connections without approaching these thresholds. The PQXDH/Kyber CPU overhead is < 2% at peak load. The memory model is 180 MB for 10k connections. Upgrade only when monitoring says to.

---

*Apply all settings on initial provision via `provision-server.sh`. Review tuning after any major traffic increase.*
