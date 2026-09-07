# Aethyr Schema Registry

The schema versioning service for the F33D3R brain network.

**Port:** 8079  
**Database:** f33d3r_registry (PostgreSQL 18)  
**Language:** Rust 1.95.0  
**Startup order:** First (all other brains depend on it)

Every brain registers its signal schemas here. The registry enforces backward-compatible evolution — a brain cannot break its consumers by removing fields or changing types without a version bump.

---

## Why this exists

F33D3R has 11 brain regions emitting signals to each other and to AethyrRank. Without a contract layer:

- Zior adding a field to `AudioAnalysis` silently breaks consumers
- Nantar redefining `PostEngagement` requires coordinated deployments of all brains
- Schema drift causes silent data corruption under load

The schema registry solves this: every signal type has a versioned JSON Schema. The compatibility checker enforces the rules. Brains evolve independently without breaking each other.

---

## API

### Brain registration

```
POST /v1/schemas/register           ← Register a new schema version
GET  /v1/schemas/:name/:version     ← Fetch schema by name + version
GET  /v1/schemas/:name/latest       ← Get latest version of a schema
```

### Health

```
GET /health   → {"status": "ok", "service": "aethyr-schema-registry"}
```

---

## Compatibility rules

| Change | Allowed |
|--------|---------|
| Add optional field | ✅ |
| Add required field with default | ✅ |
| Widen type (integer → number) | ✅ |
| Add enum value | ✅ |
| Remove any field | ❌ |
| Change field type (incompatible) | ❌ |
| Make optional field required | ❌ |
| Remove enum value | ❌ |
| Add required field without default | ❌ |

---

## Example: registering a schema

```bash
curl -X POST http://localhost:8079/v1/schemas/register \
  -H "Content-Type: application/json" \
  -d '{
    "name":    "zior.audio_analysis",
    "version": 1,
    "schema": {
      "type": "object",
      "properties": {
        "track_id":      {"type": "string"},
        "topic_vector":  {"type": "array", "items": {"type": "number"}},
        "bpm":           {"type": "number"},
        "mood":          {"type": "string"},
        "velocity_score":{"type": "number"}
      },
      "required": ["track_id", "topic_vector", "bpm", "mood"]
    }
  }'
```

---

## Schema names used by the platform

| Schema | Owner | Consumers |
|--------|-------|-----------|
| `aethyrrank.rank_request` | AethyrRank | Nantar |
| `aethyrrank.rank_response` | AethyrRank | Nantar |
| `zior.audio_analysis` | Zior | AethyrRank, Nantar |
| `vovin.message` | Vovin | Nantar (proxy) |
| `elohim_veni.capability` | Elohim Veni | All brains |
| `zodacare.report` | Zodacare | Elohim Veni |

---

## Running locally

```bash
# Via root compose (recommended — starts first)
docker compose -f ../docker-compose.local.yml up -d aethyr-schema-registry

# Local development
DATABASE_URL=postgres://f33d3r:password@localhost:5432/f33d3r_registry \
  RUST_LOG=info cargo run --release
```

---

## Project structure

```
aethyr-schema-registry/
├── src/
│   ├── handlers.rs     ← HTTP endpoints (register, get, latest)
│   ├── db.rs           ← Schema storage + version queries
│   ├── validator.rs    ← JSON Schema compatibility checking
│   └── main.rs         ← Server startup
└── docker/
    └── Dockerfile      ← rust:1.95.0-slim-bookworm builder
```
