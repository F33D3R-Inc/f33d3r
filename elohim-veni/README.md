# Elohim Veni — Security Brain

The PIAL enforcement engine for F33D3R.

**Brain name:** Elohim Veni  
**Port:** 8093  
**Database:** f33d3r_security (PostgreSQL 18)  
**Language:** Rust 1.95.0 (axum, sqlx, tokio)

Elohim Veni is the security and identity enforcement brain. It owns capability state for every PIAL root, makes
deterministic enforcement decisions, and maintains an immutable audit log of every decision.

**Architecture rule:** Elohim Veni enforces capabilities. It does NOT make ranking decisions, infer psychology, or
understand content. It receives facts (this PIAL attempted action X) and returns decisions (allow / deny) based purely
on capability state.

---

## PIAL — Persistent Identity + Access Layer

PIAL is F33D3R's Apple ID equivalent. Every user account is bound to a PIAL root UUID that:

- Persists across handle changes, account resets, and device changes
- Is the cross-brain identity anchor (all brains use PIAL UUID, not account ID)
- Carries capability state managed exclusively by Elohim Veni
- Has an immutable event ledger recording all decisions
- Can be tombstoned (permanent ban) without affecting the platform's other users

```
PIAL Root (UUID)
  ├── Capability tree (POSTING, MESSAGING, etc.)
  ├── Trust score (0–100, updated after enforcement decisions)
  ├── Enforcement state (none | warning | restricted | suspended | terminated)
  └── Event ledger (append-only, never deleted)
```

---

## Capabilities

| Capability          | Controls                                |
|---------------------|-----------------------------------------|
| `POSTING`           | Creating posts and replies              |
| `MESSAGING`         | Sending DMs via Vovin                   |
| `MUSIC_UPLOAD`      | Publishing tracks via Zior              |
| `REALM_PROGRESSION` | Earning XP and levelling up             |
| `NSFW_ACCESS`       | Viewing adult content                   |
| `MONETIZATION`      | Earning AET from content                |
| `NEW_ACCOUNT_TRUST` | Grace period protection from spam flags |

### Capability states

| State        | Meaning                               |
|--------------|---------------------------------------|
| `granted`    | Active. Check expiration timestamp.   |
| `restricted` | Temporarily limited with conditions.  |
| `revoked`    | Permanently denied.                   |
| `cooldown`   | Time-locked. Expires at `expires_at`. |

---

## API

### PIAL lifecycle

| Method | Path                       | Description                                                                           |
|--------|----------------------------|---------------------------------------------------------------------------------------|
| `POST` | `/v1/pial/bootstrap`       | Create PIAL state for a new account (idempotent — accepts canonical UUID from Nantar) |
| `POST` | `/v1/pial/:pial_id/status` | Update PIAL status (ACTIVE / SUSPENDED / REVOKED)                                     |
| `POST` | `/v1/pial/:pial_id/ban`    | Hard ban — REVOKE + revoke all capabilities                                           |

### Capabilities

| Method | Path                             | Description                     |
|--------|----------------------------------|---------------------------------|
| `GET`  | `/v1/pial/:pial_id/capabilities` | Get all capabilities for a PIAL |
| `POST` | `/v1/pial/:pial_id/capabilities` | Set a single capability state   |
| `POST` | `/v1/pial/capabilities/bulk`     | Batch-set multiple capabilities |

### Trust

| Method | Path                      | Description              |
|--------|---------------------------|--------------------------|
| `GET`  | `/v1/pial/:pial_id/trust` | Get trust score and tier |

### Decisions

| Method | Path                | Description                                   |
|--------|---------------------|-----------------------------------------------|
| `POST` | `/v1/decisions`     | Make an enforcement decision (allow/deny)     |
| `GET`  | `/v1/decisions/log` | Paginated audit log (filter by pial_id, date) |

### Stats

| Method | Path        | Description                             |
|--------|-------------|-----------------------------------------|
| `GET`  | `/v1/stats` | Total PIALs, decisions today, deny rate |
| `GET`  | `/health`   | Service health                          |

---

## Decision engine

Every enforcement decision is:

1. Deterministic (same inputs always produce same output)
2. Auditable (logged to `decision_log` table, immutable)
3. Psychology-free (Elohim Veni never receives or uses Jung archetype data)

Decision types: `ALLOW` · `ALLOW_RESTRICTED` · `QUARANTINE` · `DENY`

Trust scores are updated after every decision using exponential decay on anomalies.

---

## Zodacare integration

When Zodacare issues a moderation action (warn / restrict / suspend / ban), it calls Elohim Veni to:

- Revoke or restrict the relevant capability
- Update the PIAL's enforcement state
- Log the enforcement action to the immutable ledger

Zodacare tells Elohim Veni WHAT to do (based on its risk scoring). Elohim Veni records HOW it was done and provides the
authoritative capability state.

---

## Nantar integration

Nantar checks capabilities before write actions:

```go
// In handlers.go:
if !dbpkg.HasCapability(h.db, user.PIALID, model.CapPosting) {
    http.Error(w, "Posting is restricted on this account", http.StatusForbidden)
    return
}
```

Capabilities are cached in f33d3r_feed (`pial_capabilities` table) and mirrored from Elohim Veni. Elohim Veni is the
authoritative source.

---

## Project structure

```
elohim-veni/
├── src/
│   ├── handlers.rs     ← All HTTP endpoints
│   ├── models.rs       ← Request/response types, capability structs
│   ├── db.rs           ← PIAL state queries, capability CRUD, decision log
│   ├── decision.rs     ← Decision engine — deterministic rule evaluator
│   └── main.rs         ← Server startup
└── docker/
    └── Dockerfile      ← rust:1.95.0-slim-bookworm builder
```

---

## Running locally

```bash
# Via root compose (recommended)
docker compose -f ../docker-compose.local.yml up -d elohim-veni

# Check health
curl http://localhost:8093/health

# Check stats
curl http://localhost:8093/v1/stats

# Get capabilities for a PIAL
curl http://localhost:8093/v1/pial/{pial_id}/capabilities
```

---

## Bootstrap flow

On user signup, Nantar calls:

```
POST /v1/pial/bootstrap
{
  "pial_id":    "uuid-from-nantar",
  "account_id": "user-uuid",
  "handle":     "username"
}
```

Elohim Veni creates the PIAL state record (idempotent — safe to call multiple times). It does NOT generate its own
UUID — the canonical UUID comes from Nantar.

Default capabilities granted at bootstrap: `POSTING` · `MESSAGING` · `MUSIC_UPLOAD` · `REALM_PROGRESSION` ·
`NEW_ACCOUNT_TRUST`
