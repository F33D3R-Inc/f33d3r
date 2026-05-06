# Ain Soph — Wallet Brain

The AET token and financial ledger brain for F33D3R.

**Brain name:** Ain Soph  
**Port:** 8089  
**Database:** f33d3r_wallet (PostgreSQL 18)  
**Language:** Rust 1.95.0 (axum, sqlx, tokio)

Ain Soph manages AET token balances, transfers, and the double-entry accounting ledger. Every financial operation is recorded as a debit/credit pair — the ledger is always balanced.

---

## AET token

AET is F33D3R's native token. Users earn AET through:

- Creating posts that resonate (engagement-weighted rewards)
- Levelling up Realm rank (milestone bonuses)
- Receiving tips from other users
- Selling music tracks via Zior

AET can be spent on:

- Tipping other creators
- Purchasing paid music tracks
- Accessing gated content (Thessalon, planned)

Full withdrawal and exchange are handled by the **Thessalon engine** (planned).

---

## Ledger model

Every transaction creates two ledger entries (double-entry accounting):

```
Deposit 100 AET:
  DEBIT  external_source   100 AET
  CREDIT user_account      97.5 AET   (after 2.5% platform fee)
  CREDIT ain_soph_fees      2.5 AET   (platform float account)

Transfer 10 AET (user A → user B):
  DEBIT  user_a_account    10 AET
  CREDIT user_b_account    10 AET
```

Balances are always derivable by summing the ledger. The `accounts` table stores the running balance as a read-optimised cache.

---

## API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Service health (returns 200 with empty body) |
| `GET` | `/balance/:user_id` | Get account balance in AET cents |
| `POST` | `/deposit` | Credit funds to an account (deducts 2.5% fee) |
| `POST` | `/transfer` | P2P transfer (idempotent via idempotency key) |
| `POST` | `/withdraw` | Debit account + trigger external transfer |

### `GET /balance/:user_id`

```json
{
  "user_id":  "uuid",
  "balance":  2500,
  "currency": "AET",
  "address":  "aet1q..."
}
```

`balance` is in AET cents (divide by 100 for display). Nantar shows this on the wallet page.

### `POST /deposit`

```json
{
  "user_id":       "uuid",
  "amount_cents":  1000,
  "source":        "platform_reward",
  "idempotency_key": "unique-string"
}
```

### `POST /transfer`

```json
{
  "from_user_id":    "uuid",
  "to_user_id":      "uuid",
  "amount_cents":    500,
  "memo":            "tip for track",
  "idempotency_key": "unique-string"
}
```

Idempotency keys prevent double-charges on network retries. Duplicate requests with the same key return the original result without re-executing.

---

## Fee structure

| Operation | Fee |
|-----------|-----|
| Deposit | 2.5% platform fee |
| Transfer (tip, purchase) | 0% (peer-to-peer) |
| Withdrawal | TBD (Thessalon) |

Platform fees accumulate in the `ain_soph_fees` account.

---

## Nantar integration

The wallet page in Nantar (`/wallet`) calls:
```
GET /balance/:user_id → Ain Soph :8089
```

with a 3-second timeout. If Ain Soph is unreachable, the page shows "Wallet service unavailable" and prompts the user to refresh.

---

## Project structure

```
ain-soph/
├── src/
│   ├── handlers.rs     ← All HTTP endpoints (balance, deposit, transfer, withdraw)
│   ├── models.rs       ← Account, transaction, ledger entry structs
│   ├── db.rs           ← Double-entry ledger queries, balance cache
│   └── main.rs         ← Server startup + schema migration
└── docker/
    └── Dockerfile      ← rust:1.95.0-slim-bookworm builder
```

---

## Running locally

```bash
# Via root compose (recommended)
docker compose -f ../docker-compose.local.yml up -d ain-soph

# Check health (returns 200, empty body)
curl -v http://localhost:8089/health

# Check a balance
curl http://localhost:8089/balance/{user_uuid}
```

---

## Thessalon (planned)

The **Thessalon engine** will extend Ain Soph with:

- External cryptocurrency gateway (Uphold, Stripe)
- AET ↔ fiat conversion
- Creator subscription billing
- Withdrawal to external wallet
- Revenue splitting (platform / creator / collaborators)

Thessalon will communicate with Ain Soph via the existing deposit/transfer/withdraw API.
