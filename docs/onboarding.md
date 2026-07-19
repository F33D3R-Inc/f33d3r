# F33D3R Engineering Onboarding

Welcome. Read this before touching any code.

---

## The one rule

> **No brain reads another brain's database. No brain calls another brain outside the approved dependency matrix.**

Everything else in this doc explains why and how. But if you read nothing else, know that rule.

---

## What F33D3R is

A distributed social operating system. 8 independent services ("brains"), each owning its own data, communicating
through events. Think of it like organs in a body — each one has a job, and they coordinate through the bloodstream (
Kafka), not by reaching into each other.

The key insight: **we are not building a monolith that happens to run in multiple containers**. Each brain is genuinely
independent. It can be restarted, scaled, updated, or replaced without the others knowing.

---

## Start here

```bash
git clone git@gitlab.com:f33d3r-core/platform/f33d3r-local.git
cd f33d3r-local
bash bootstrap-local.sh
open http://localhost:8081
```

Create an account, poke around the UI. Then read:

1. `ARCHITECTURE.md` — system design
2. `BRAIN_MAP.md` — what each brain does and doesn't do
3. `README-LOCAL.md` — development workflow
4. `events/taxonomy.md` — event system

---

## PIAL — what it is and why it matters

PIAL is the platform's Apple ID. Every user gets a UUID (the PIAL root) that:

- Never changes (accounts/handles are disposable, PIAL is forever)
- Is the cross-brain identity — used everywhere instead of user ID or handle
- Carries the capability tree (what you can and can't do)
- Has an immutable event ledger (every capability change is recorded)

When you work on any feature that affects permissions, identity, or cross-brain data: **use the PIAL UUID, not the user
UUID**.

---

## The 8 brains — quick reference

| Brain                    | What it does                    | What it absolutely does not do     |
|--------------------------|---------------------------------|------------------------------------|
| **Nantar** (feed-engine) | UI, social layer, PIAL creation | Make moderation decisions          |
| **AethyrRank**           | Rank content                    | Control feed, enforce capabilities |
| **Zior**                 | Music intelligence              | Rank posts, touch wallet           |
| **Vovin**                | Encrypted message relay         | Read message content               |
| **Ain Soph**             | AET token ledger                | Grant capabilities                 |
| **Elohim Veni**          | Moderation decision pipeline → writes PIAL | Use psychology for decisions       |
| **Zodacare**             | Risk scoring, mod queue         | Directly revoke capabilities       |
| **Schema Registry**      | Schema versioning               | Store business data                |

---

## Making a change

### Frontend (Nantar / feed-engine)

1. Edit Go or HTML in `feed-engine/`
2. Build locally: `go build ./...`
3. Rebuild Docker: `docker compose -f docker-compose.local.yml build feed-engine`
4. Restart: `docker compose -f docker-compose.local.yml up -d feed-engine`
5. Open http://localhost:8081 and test

### Rust brain

Same pattern:

1. Edit Rust in `<brain>/src/`
2. `cargo build --release` (optional, Docker does this)
3. `docker compose build <brain>`
4. `docker compose up -d <brain>`

### Database change

Add an `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` to the relevant migration file. It runs automatically on next startup.

---

## Enforcement

Before opening a merge request:

```bash
# Run enforcement checks locally
python3 enforcement/ci-gates/graph_validation.py
python3 enforcement/contract-checker/version_policy.py
python3 enforcement/contract-checker/event_schema_validator.py
```

CI will also run these and block merge on failure.

---

## Versioning

Never change these without team sign-off and updating ALL affected Dockerfiles, go.mod, and base.html:

| Component | Pinned version |
|-----------|----------------|
| Rust      | 1.95.0         |
| Go        | 1.26.2         |
| HTMX      | 2.0.10         |

The `enforcement/contract-checker/version_policy.py` check will fail CI if any Dockerfile or go.mod is out of sync.

---

## Getting help

- `ARCHITECTURE.md` — system design decisions
- `BRAIN_MAP.md` — brain boundaries and dependency matrix
- `docs/deployment.md` — how to deploy
- `docs/brain-registry.md` — how to add a new brain

When in doubt: **check BRAIN_MAP.md first**. If your feature requires a brain to do something that BRAIN_MAP.md says it
cannot do, that's a design discussion, not a quick fix.
