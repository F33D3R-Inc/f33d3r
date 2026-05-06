# AethyrRank Engine

The core ranking brain for the F33D3R platform.

**Brain name:** AethyrRank  
**Port:** 8080  
**Database:** f33d3r_feed (PostgreSQL 18, shared with Nantar)  
**Language:** Rust 1.95.0 (axum, sqlx, tokio)

Implements a **7-stage constrained ranking pipeline** combining neural engagement prediction, psychological alignment constraints, safety enforcement, velocity signals, and diversity re-ranking.

---

## Pipeline

```
POST /rank
    │
    ├─ Stage 1: Pre-ranker
    │    fast_score = quality × freshness
    │    pool (500) → top_k (100)
    │
    ├─ Stage 2: Neural model
    │    P(engagement | user, content, ctx)
    │    Wide & Deep + session Transformer (heuristic stub until trained)
    │
    ├─ Stage 3a: AESQ constraint multiplier
    │    aesq_mult = f(alignment, expansion, shadow, quality, freshness)
    │    after_aesq = neural_score × aesq_mult
    │
    ├─ Stage 3b: Safety barrier  ← HARD CONSTRAINT
    │    B(u,i) ≥ ε_u → score = −∞, item removed from response
    │    Two stages: semantic classifier + behavioral drift guard
    │
    ├─ Stage 4: Velocity boost
    │    d(engagement)/dt → ±boost clamped to [−0.10, +0.20]
    │
    ├─ Stage 5: Revenue adjustment
    │    conversion × creator_rate × LTV → +adj (capped at 0.30)
    │    tier-weighted: creator > subscriber > free
    │
    ├─ Stage 6: Re-rank
    │    creator diversity spacing (no consecutive same-creator)
    │
    └─ Stage 7: Exploration injection
         LinUCB guides cold-start + underexposed creator slots
         1–4 items flagged exploration_slot: true
```

---

## Design constraints

| Wrong | Correct |
|-------|---------|
| AESQ = primary scorer | AESQ = constraint multiplier on neural score |
| LinUCB = core intelligence | LinUCB = exploration controller only |
| β-blend of AESQ + LinUCB | Additive layer model |
| No safety barrier | Hard B(u,i) < ε_u constraint (items fully removed) |
| No velocity layer | d(engagement)/dt fully integrated |
| No revenue layer | Tier-aware revenue adjustment |

---

## API

### `POST /rank`

**Request:**
```json
{
  "user_state": {
    "user_id":             "uuid",
    "interest_vector":     [0.2, 0.8, 0.1, 0.4, 0.6, 0.3, 0.5, 0.7],
    "interaction_history": ["content_id_1", "content_id_2"],
    "is_cold_start":       false,
    "creator_affinities":  {"creator_uuid": 0.7},
    "safety_epsilon":      0.10,
    "user_tier":           "free",
    "realm_level":         2
  },
  "content_pool": [
    {
      "content_id":             "uuid",
      "creator_id":             "uuid",
      "topic_vector":           [0.3, 0.6, 0.1, 0.5, 0.4, 0.2, 0.8, 0.3],
      "published_at":           "2026-04-01T12:00:00Z",
      "engagement":             {"likes": 340, "shares": 80, "comments": 45, "saves": 12, "view_time_seconds": 8.4, "impressions": 5000},
      "exposure_count":         5000,
      "creator_exposure":       120000,
      "tags":                   ["music", "indie"],
      "content_type":           "post",
      "velocity_score":         0.82,
      "adult_probability":      0.00
    }
  ],
  "session_context": {
    "surface":    "feed",
    "request_id": "uuid",
    "session_id": "uuid",
    "max_items":  20,
    "timestamp":  "2026-04-23T14:00:00Z"
  }
}
```

**Response:**
```json
{
  "request_id":   "uuid",
  "confidence":   0.74,
  "latency_ms":   11,
  "ranked_items": [
    {
      "content_id":       "uuid",
      "rank":             1,
      "final_score":      0.847,
      "exploration_slot": false,
      "safety_blocked":   false,
      "explanation":      "neural=0.761 × aesq_mult=0.934=0.711 | vel=+0.082 | rev=+0.054 | final=0.847",
      "score_breakdown": {
        "fast_score":      0.68,
        "neural_score":    0.761,
        "aesq_alignment":  0.91,
        "velocity_boost":  0.082,
        "revenue_adj":     0.054,
        "final_score":     0.847
      }
    }
  ]
}
```

### `POST /feedback`

```json
{
  "user_id":    "uuid",
  "session_id": "uuid",
  "surface":    "feed",
  "events": [
    {
      "content_id":          "uuid",
      "event_type":          "like",
      "position_at_display": 3,
      "timestamp":           "2026-04-23T14:01:10Z",
      "dwell_ms":            14200,
      "exploration_slot":    false
    }
  ]
}
```

Event types: `impression` · `click` · `like` · `share` · `comment` · `save` · `skip` · `negative_feedback` · `view_complete` · `purchase`

### `GET /health`

```json
{"status": "ok", "service": "aethyrrank-engine", "session_cache": 14823}
```

---

## Safety barrier

AethyrRank implements a **hard constraint**, not a soft weight.

| User setting | safety_epsilon | Behaviour |
|---|---|---|
| `safe_mode` | `1e-6` | Near-zero adult exposure. Borderline content amplified 1.8× for SFW detection. |
| `default` | `0.10` | Standard platform norms. |
| `adult_enabled` | `1.0` | No content restriction. |

Items that fail the constraint receive `final_score = −999999` and are **removed from the response** before it reaches the client. They never appear in the feed.

---

## Surfaces

AethyrRank supports multiple named surfaces, each with an independent LinUCB model:

- `feed` — Home feed (For You)
- `explore` — Discovery surface
- `trending` — Trending posts
- `trending_music` — Trending tracks
- `latest` — Chronological (no ranking, bypass)

---

## LinUCB persistence

The LinUCB model state is checkpointed to `linucb_checkpoints` table in `f33d3r_feed` every 60 seconds and on graceful shutdown. On startup, the previous checkpoint is loaded and training resumes without cold-starting.

---

## Project structure

```
aethyrrank-engine/
├── src/
│   ├── api/
│   │   ├── handlers.rs      ← 7-stage pipeline orchestrator
│   │   └── types.rs         ← all request/response DTOs
│   ├── pipeline/
│   │   ├── preranker.rs     ← Stage 1: fast quality×freshness filter
│   │   ├── neural.rs        ← Stage 2: neural score (heuristic stub + ONNX slot)
│   │   ├── velocity.rs      ← Stage 4: d(engagement)/dt boost
│   │   ├── revenue.rs       ← Stage 5: tier-aware revenue adjustment
│   │   └── reranker.rs      ← Stage 6: creator diversity spacing
│   ├── safety/
│   │   └── barrier.rs       ← Hard B(u,i) < ε_u constraint
│   ├── scoring/
│   │   └── aesq.rs          ← AESQ constraint multiplier (not scorer)
│   ├── bandit/
│   │   └── linucb.rs        ← Exploration controller (not core ranker)
│   ├── session/
│   │   └── cache.rs         ← Feature vector cache for accurate LinUCB updates
│   ├── persist.rs            ← LinUCB checkpoint to Postgres (60s interval)
│   ├── model_store.rs        ← Per-surface LinUCB model store
│   └── main.rs               ← Server startup + graceful shutdown
├── training/
│   ├── README.md            ← Model architecture + training guide
│   ├── train.py             ← PyTorch Wide & Deep training script
│   └── export_events.py     ← Clean data pipeline with exposure debiasing
├── docker/
│   └── Dockerfile           ← rust:1.95.0-slim-bookworm builder
└── config/default.toml
```

---

## Running locally

```bash
# Build
cargo build --release

# Run (requires DATABASE_URL env)
DATABASE_URL=postgres://f33d3r:password@localhost:5432/f33d3r_feed \
  RUST_LOG=info cargo run --release

# Docker (via root compose)
docker compose -f ../docker-compose.local.yml up -d aethyrrank-engine
```

---

## Latency budget

| Stage | Budget |
|-------|--------|
| Pre-rank (Stage 1) | < 1ms |
| Neural score (Stage 2, 100 items, stub) | < 5ms |
| AESQ + safety (Stage 3) | < 3ms |
| Velocity + revenue (Stages 4–5) | < 1ms |
| Re-rank + exploration (Stages 6–7) | < 2ms |
| **Total target** | **< 20ms** |

---

## Scaling

- **Stateless**: session cache and model store are in-process. For multi-replica deployments, back `SessionFeatureCache` with Redis (TTL 1h) and `ModelStore` with a Redis snapshot (flush every 60s).
- **Horizontal**: 3+ replicas behind a load balancer handle ~50K RPM at < 20ms p99.
