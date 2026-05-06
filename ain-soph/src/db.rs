/// Database schema and query helpers for Ain Soph — ETHRA/AET crypto network.
///
/// Schema uses a classical double-entry ledger:
///   accounts         — running AET balance per PIAL identity (O(1) reads)
///   transactions     — one row per economic event, with idempotency key
///   ledger_entries   — two rows per transaction (debit + credit), append-only
///   proposals        — governance vote proposals
///   votes            — individual vote records (one per PIAL per proposal)

use anyhow::Result;
use sqlx::PgPool;

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS accounts (
    user_id    TEXT        PRIMARY KEY,
    balance    BIGINT      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS transactions (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key  TEXT        NOT NULL UNIQUE,
    tx_type          TEXT        NOT NULL,
    from_user_id     TEXT,
    to_user_id       TEXT,
    amount           BIGINT      NOT NULL CHECK (amount > 0),
    fee              BIGINT      NOT NULL DEFAULT 0 CHECK (fee >= 0),
    net_amount       BIGINT      NOT NULL CHECK (net_amount >= 0),
    status           TEXT        NOT NULL DEFAULT 'completed',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_tx_from ON transactions(from_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tx_to   ON transactions(to_user_id,   created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tx_idem ON transactions(idempotency_key);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id             BIGSERIAL   PRIMARY KEY,
    transaction_id UUID        NOT NULL REFERENCES transactions(id),
    account_id     TEXT        NOT NULL,
    entry_type     TEXT        NOT NULL CHECK (entry_type IN ('debit','credit')),
    amount         BIGINT      NOT NULL CHECK (amount > 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ledger_tx  ON ledger_entries(transaction_id);
CREATE INDEX IF NOT EXISTS idx_ledger_acc ON ledger_entries(account_id, created_at DESC);

-- Governance proposals
CREATE TABLE IF NOT EXISTS proposals (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    title        TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    creator_id   TEXT        NOT NULL,
    weight_model TEXT        NOT NULL DEFAULT 'identity',
    options      TEXT[]      NOT NULL DEFAULT '{yes,no}',
    ends_at      TIMESTAMPTZ NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_proposals_status  ON proposals(status, ends_at DESC);
CREATE INDEX IF NOT EXISTS idx_proposals_creator ON proposals(creator_id);

-- Individual votes — one per PIAL per proposal (UNIQUE enforces this)
CREATE TABLE IF NOT EXISTS votes (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    proposal_id UUID        NOT NULL REFERENCES proposals(id),
    voter_id    TEXT        NOT NULL,
    option      TEXT        NOT NULL,
    weight      BIGINT      NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(proposal_id, voter_id)
);
CREATE INDEX IF NOT EXISTS idx_votes_proposal ON votes(proposal_id);
CREATE INDEX IF NOT EXISTS idx_votes_voter    ON votes(voter_id);
"#;

/// Apply schema (idempotent — safe to call on every startup).
pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    Ok(())
}

// ── Account helpers (all must be called inside an open transaction) ────────────

pub async fn ensure_account(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    user_id: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "INSERT INTO accounts (user_id, balance) VALUES ($1, 0) ON CONFLICT (user_id) DO NOTHING",
    )
    .bind(user_id)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

/// SELECT FOR UPDATE — locks the row for the duration of the transaction.
pub async fn locked_balance(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    user_id: &str,
) -> Result<i64, sqlx::Error> {
    let row: (i64,) =
        sqlx::query_as("SELECT balance FROM accounts WHERE user_id = $1 FOR UPDATE")
            .bind(user_id)
            .fetch_one(&mut **tx)
            .await?;
    Ok(row.0)
}

pub async fn credit_account(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    user_id: &str,
    amount: i64,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "UPDATE accounts SET balance = balance + $1, updated_at = NOW() WHERE user_id = $2",
    )
    .bind(amount)
    .bind(user_id)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

pub async fn debit_account(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    user_id: &str,
    amount: i64,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "UPDATE accounts SET balance = balance - $1, updated_at = NOW() WHERE user_id = $2",
    )
    .bind(amount)
    .bind(user_id)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

pub async fn append_ledger(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    tx_id: uuid::Uuid,
    account_id: &str,
    entry_type: &str,
    amount: i64,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "INSERT INTO ledger_entries (transaction_id, account_id, entry_type, amount)
         VALUES ($1,$2,$3,$4)",
    )
    .bind(tx_id)
    .bind(account_id)
    .bind(entry_type)
    .bind(amount)
    .execute(&mut **tx)
    .await?;
    Ok(())
}

// ── Read helpers (no transaction required) ────────────────────────────────────

pub async fn get_balance(pool: &PgPool, user_id: &str) -> Result<Option<i64>, sqlx::Error> {
    let row: Option<(i64,)> =
        sqlx::query_as("SELECT balance FROM accounts WHERE user_id = $1")
            .bind(user_id)
            .fetch_optional(pool)
            .await?;
    Ok(row.map(|r| r.0))
}

pub async fn get_account_updated_at(
    pool: &PgPool,
    user_id: &str,
) -> Result<Option<chrono::DateTime<chrono::Utc>>, sqlx::Error> {
    let row: Option<(chrono::DateTime<chrono::Utc>,)> =
        sqlx::query_as("SELECT updated_at FROM accounts WHERE user_id = $1")
            .bind(user_id)
            .fetch_optional(pool)
            .await?;
    Ok(row.map(|r| r.0))
}
