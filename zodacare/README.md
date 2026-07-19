# Zodacare — Safety Brain

The content moderation and risk scoring brain for F33D3R.

**Brain name:** Zodacare  
**Port:** 8090  
**Database:** f33d3r_safety (PostgreSQL 18)  
**Language:** Rust 1.95.0 (axum, sqlx, tokio)

Zodacare receives content reports from users, scores reporter and reported-user risk, and issues moderation actions that
are written into the PIAL record (feed-engine) — the system of record and sole authority for capabilities.

**Architecture rule:** Zodacare assesses risk and issues actions; the moderation decision pipeline (Elohim Veni) computes
the capability change and writes it to PIAL. No brain owns auth — PIAL is the record, and only the user or an admin
overrides it. Brains are pipelines that propose changes by writing PIAL.

---

## Moderation flow

```
1. User reports content → Nantar → POST /v1/reports (Zodacare)
2. Zodacare stores the report, classifies reason, updates risk score
3. If risk score exceeds threshold → POST /v1/user/:pial_id/action (warn/restrict/ban)
4. Zodacare calls Elohim Veni → revoke/restrict capabilities
5. Admin queue: pending reports visible in /admin dashboard
6. Human review: admin can escalate, resolve, or override
```

---

## Risk scoring

Every user has a risk score (0–100) that accumulates based on:

- Number of reports received and their severity
- Confirmed violations (resolved reports that led to action)
- Behavioral signals (high-velocity posting, unusual engagement patterns)
- Account age and trust (new accounts weighted higher risk)
- Realm level and PIAL trust score from Elohim Veni

Risk scores decay over time for accounts with no new violations.

---

## API

### Reports

| Method | Path                       | Description                                  |
|--------|----------------------------|----------------------------------------------|
| `POST` | `/v1/reports`              | Create a content report                      |
| `GET`  | `/v1/reports`              | List reports (filter: status, limit, offset) |
| `GET`  | `/v1/reports/:id`          | Get report detail                            |
| `POST` | `/v1/reports/:id/resolve`  | Mark resolved (reason + action taken)        |
| `POST` | `/v1/reports/:id/escalate` | Escalate to human review queue               |

### User risk

| Method | Path                       | Description             |
|--------|----------------------------|-------------------------|
| `GET`  | `/v1/user/:pial_id/risk`   | Get risk score (0–100)  |
| `POST` | `/v1/user/:pial_id/action` | Apply moderation action |

### Stats + health

| Method | Path        | Description                                  |
|--------|-------------|----------------------------------------------|
| `GET`  | `/v1/stats` | Reports total, pending count, actioned today |
| `GET`  | `/health`   | Service health                               |

---

## Report schema

```json
{
  "reporter_pial": "uuid",
  "content_id":    "uuid",
  "content_type":  "post",
  "reason":        "spam",
  "detail":        "repeated promotional content"
}
```

**Reason categories:** `spam` · `harassment` · `hate_speech` · `misinformation` · `csam` · `violence` · `copyright` ·
`other`

---

## Moderation actions

| Action     | Effect on PIAL                                    |
|------------|---------------------------------------------------|
| `warn`     | Log warning, no capability change                 |
| `restrict` | Set POSTING capability to `cooldown` (24h)        |
| `suspend`  | Set POSTING + MESSAGING to `restricted` (7 days)  |
| `ban`      | Set all capabilities to `revoked`, tombstone PIAL |

All actions are forwarded to Elohim Veni, which writes the capability change to the PIAL record (the authority) and keeps an immutable audit log.

---

## Report statuses

| Status      | Meaning                             |
|-------------|-------------------------------------|
| `pending`   | Awaiting review                     |
| `resolved`  | Reviewed, action taken or dismissed |
| `escalated` | Flagged for senior review           |

---

## Admin dashboard

Zodacare feeds the admin dashboard in Nantar (`/admin`). Admins can:

- View pending reports queue
- Resolve or escalate reports
- View user risk scores
- Apply moderation actions directly via Elohim Veni

---

## Project structure

```
zodacare/
├── src/
│   ├── handlers.rs     ← All HTTP endpoints
│   ├── models.rs       ← Report, risk score, action structs
│   ├── db.rs           ← Report storage, risk scoring queries
│   └── main.rs         ← Server startup
└── docker/
    └── Dockerfile      ← rust:1.95.0-slim-bookworm builder
```

---

## Running locally

```bash
# Via root compose (recommended)
docker compose -f ../docker-compose.local.yml up -d zodacare

# Check health
curl http://localhost:8090/health

# Check stats
curl http://localhost:8090/v1/stats
```

> **Note:** The `f33d3r_safety` database must exist before Zodacare starts.
> If the postgres container has been running since before Zodacare was added,
> create it manually:
> ```bash
> docker exec f33d3r-local-postgres-1 psql -U f33d3r -c "CREATE DATABASE f33d3r_safety;"
> docker compose -f ../docker-compose.local.yml restart zodacare
> ```
