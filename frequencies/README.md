# Auralis — F33D3R Frequencies

The brain that owns the Frequency: F33D3R's native live-audio social object.

| | |
|---|---|
| Service | `auralis` |
| Port | 8108 (HTTP, internal) · 40000/udp media (Phase 3) |
| Database | `f33d3r_frequencies` |
| Hot state | Redis, namespace `freq:` |
| Events | Sitra Achra topic `frequency.events`, schemas in `events/schemas/frequency.*.json` |

## What it owns

Lifecycle, durable state, participants, roles, speaker requests, moderation,
signaling authorization, media allocation, the SFU (Phase 3), recording
coordination (Phase 5), metrics, and every `frequency.*` event.

Nantar remains the only HTML edge. It calls these routes with
`X-Internal-Key` and `X-Pial-Identity`, renders Facets from the read model,
and consumes `frequency.events` to push mutations over FA Live.

## Lifecycle

```
draft → scheduled → starting → live → ending → ended → processing_replay → archived
                                       └→ moderation_terminated      cancelled / failed
```

Every state change is a compare-and-set on `frequencies.version` and writes
its event into `frequency_events` in the same transaction. The drain
(`src/events/drain.rs`) publishes them in `seq` order under an advisory lock
and marks them; a broker outage is a growing `frequency_events_unpublished`
gauge, never a lost event.

## Layout

```
src/
  main.rs            boot: secrets refused when empty, migrate, redis, manhattan, drain, reconcile, serve
  config.rs          env → Config
  auth.rs            Service / Actor extractors (X-Internal-Key required on every route)
  error.rs           AuralisError → status + machine code
  identity.rs        name → pial:<uuid> via Manhattan
  migrate.rs         numbered, checksummed, forward-only migrations
  observ.rs          shared across every Rust brain — byte-identical, do not edit here
  domain/            pure: state machine, role matrix, admission, host-loss, request rules
  repository/        postgres (CAS, outbox), redis (presence, sets, leases, limits)
  events/            envelope + types, JSON schema writer, outbox drain
  api/               router, read model (view.rs), shared ops, handlers
  security/          signaling tokens (HMAC), rate limits, input hygiene
  telemetry/         every metric name, described once
  tasks.rs           boot reconcile, presence sweep + host grace, lease renew, prune
  herald.rs          push with cooldown
```

## Running the tests

```
cd frequencies
cargo fmt --check && cargo clippy --all-targets -- -D warnings
AURALIS_TEST_DATABASE_URL=postgres://f33d3r:f33d3rdev@localhost:5432/postgres cargo test
```

Without `AURALIS_TEST_DATABASE_URL` the database-backed tests print that they
were skipped and pass. Set it; the compare-and-set and outbox pins only mean
something against a real server.

## Regenerating event schemas

```
cargo run -q -- --write-schemas ../events/schemas
```

A test fails if the checked-in files differ from what the code generates.

## Environment

| Variable | Default | Notes |
|---|---|---|
| `PORT` | 8108 | |
| `DATABASE_URL` | required | |
| `REDIS_URL` | `redis://redis:6379` | |
| `KAFKA_BROKERS` | required | `sitra-achra:9092` |
| `INTERNAL_API_KEY` | required | refuses to boot when empty |
| `AURALIS_SIGNAL_SECRET` | required, ≥32 chars | signs browser signaling tokens; distinct from the internal key |
| `MANHATTAN_URL` | | |
| `HERALD_URL` | `http://herald:8105` | |
| `AURALIS_NODE_ID` | hostname | media-lease owner id |
| `AURALIS_PUBLIC_HOST` | `localhost` | advertised media host (Phase 3) |
| `AURALIS_MEDIA_UDP_PORT` | 40000 | media socket (Phase 3) |
| `AURALIS_PRESENCE_TTL_SECS` | 45 | presence key TTL |
| `AURALIS_PRESENCE_SWEEP_SECS` | 10 | sweeper cadence |
| `AURALIS_HOST_GRACE_SECS` | 120 | host absence before co-host continues or session ends |
| `AURALIS_LEASE_TTL_SECS` | 30 | media-node lease; renewed at a third |
| `AURALIS_NOTIFY_COOLDOWN_SECS` | 300 | Herald cooldown per person per kind |
| `AURALIS_STARTING_TIMEOUT_SECS` | 60 | `starting` older than this is failed on boot |
