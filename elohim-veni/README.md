# Elohim Veni — Security Brain

The PIAL enforcement engine for F33D3R.

**Brain name:** Elohim Veni  
**Port:** 8093  
**Database:** f33d3r_security (PostgreSQL 18)  
**Language:** Rust 1.95.0 (axum, sqlx, tokio)

Elohim Veni is the security moderation-decision brain, and it is the **identity authority**. Manhattan's authority map
(`manhattan/migrations/0002_authority_map.sql`) assigns this brain the `identity`, `key` and `address` node kinds and the
`pial` and `addr` namespaces: identity, signing keys and ECDH keys are asserted here, and every other brain's PIAL column
is a reference to what this brain owns. It makes deterministic moderation decisions, writes capability changes, and keeps
an immutable audit log of every decision.

It does not sign users in — feed-engine runs the login flow and mints the PIAL UUID that `POST /v1/pial/bootstrap`
carries. Where the UUID is generated and where identity is asserted are different questions; the second one is answered
here and recorded in the naming plane.

**Architecture rule:** Elohim Veni decides and writes capability changes to PIAL. It does NOT make ranking decisions,
infer psychology, or understand content. It receives facts (this PIAL attempted action X) and writes decisions (allow /
deny) to the PIAL record.

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

## Manhattan — the naming plane

**The rule: no brain may reference another brain's rows. It may only reference a NAME, and it resolves that name through
Manhattan.** Names are `<namespace>:<value>` — `pial:<uuid>` is an identity, `handle:<h>` is a transferable pointer to
one, `uuid:<id>` names an entity.

What this brain registers, being the authority for it:

| Registered                    | As                                                 |
|-------------------------------|----------------------------------------------------|
| every PIAL in `pial_states`   | an `identity` node named `pial:<uuid>`             |
| every signing and ECDH key    | a `key` node named `uuid:<key_id>`                 |
| whose key it is               | a `signs_for` edge from the key to the identity    |

**The duplicated key tables.** `pial_signing_keys`, `pial_ecdh_keys` and `trust_scores` exist in both `f33d3r_security`
and `f33d3r_feed`, with source comments in each disagreeing about which was authoritative. This side is the authority —
the authority map says so for the `key` kind — and feed-engine's copies are read-through caches with no write privilege.
The answer is no longer a comment: each key is a node in the graph with an edge to the identity it signs for, so
"whose key is this" resolves to exactly one recorded answer.

`key_id` names the key MATERIAL, not the row. Rotating a PIAL's key mints a new `key_id`, so the graph records a new key
signing for the same identity rather than quietly redefining the old one.

**Registration is durable, not fire-and-forget.** A graph write sent over HTTP and forgotten is one a restart, a timeout
or a rolling deploy silently loses, and a naming plane that is silently missing rows is worse than none. So triggers on
`pial_states`, `pial_signing_keys` and `pial_ecdh_keys` write into `manhattan_outbox` inside the same transaction as the
row that caused them: either the row exists and its registration is queued, or neither happened. `manhattan_outbox.rs`
drains the queue every 10s in strict id order, stopping the batch on the first failure — later rows depend on earlier
ones — retrying with capped exponential backoff, and marking a row delivered only once Manhattan has accepted it.

**Identity is resolved by PIAL and by nothing else.** The `:pial_id` path segment accepts a PIAL uuid or the `pial:<uuid>`
name as it stands; any other name (`handle:tehanibentley`, an address) is resolved through Manhattan to the identity
behind it, and everything downstream keys on the PIAL that comes back. There is no local fallback — falling back to a
join is the drift the naming plane exists to end, so an unreachable plane is an outage, not a hint.

`MANHATTAN_URL` unset means registrations queue and are not delivered. That is visible on `/health`:

```json
{
  "status": "ok",
  "service": "elohim-veni",
  "manhattan": true,
  "manhattan_outbox_pending": 0,
  "manhattan_outbox_oldest_age_seconds": null,
  "manhattan_outbox_head_error": null
}
```

Depth says the queue is behind; the age of its head and the error that head last failed with say whether it is moving.

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
│   ├── db.rs           ← Schema, PIAL state queries, capability CRUD, decision log
│   ├── decision.rs     ← Decision engine — deterministic rule evaluator
│   ├── manhattan_client.rs  ← The naming plane client. IDENTICAL in every brain —
│   │                          copied from infra/manhattan/, never edited here
│   ├── manhattan_outbox.rs  ← Ordered, retrying drain of the transactional outbox
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
  "pial_id": "uuid-from-nantar",
  "tier":    "BASIC"
}
```

Elohim Veni creates the PIAL state record, keyed on the PIAL and on nothing else (idempotent — safe to call multiple
times). It does NOT generate its own UUID; the canonical UUID comes from Nantar.

`account_id` is no longer stored. It was another brain's row id used here as a second identity key, and its UNIQUE
constraint let a re-bootstrap move a PIAL out from under an account — a PIAL is the root that never moves. Callers still
sending the field are unaffected: it is ignored. The account mapping lives in `f33d3r_feed.users`, where the account is.

The same transaction that creates the row queues the identity's registration into Manhattan.

Default capabilities granted at bootstrap: `POSTING` · `MESSAGING` · `MUSIC_UPLOAD` · `REALM_PROGRESSION` ·
`NEW_ACCOUNT_TRUST`
