use anyhow::Result;
use sqlx::PgPool;

const SCHEMA: &str = r#"
-- ── Accounts ──────────────────────────────────────────────────────────────────
-- One row per PIAL. Balance in micro-AET (1 AET = 1_000_000 micro-AET).
-- Using BIGINT to avoid float rounding — all arithmetic is integer math.
-- sequence_no: monotonically increasing per account. Enables replay protection
-- and XRP-style ordered transaction streams.
CREATE TABLE IF NOT EXISTS ledger_accounts (
    pial_id         UUID        PRIMARY KEY,
    balance_uaet    BIGINT      NOT NULL DEFAULT 0 CHECK (balance_uaet >= 0),
    credit_balance  BIGINT      NOT NULL DEFAULT 0 CHECK (credit_balance >= 0),
    total_earned    BIGINT      NOT NULL DEFAULT 0,
    total_spent     BIGINT      NOT NULL DEFAULT 0,
    total_minted    BIGINT      NOT NULL DEFAULT 0,
    total_burned    BIGINT      NOT NULL DEFAULT 0,
    sequence_no     BIGINT      NOT NULL DEFAULT 0,  -- increments on every debit
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Events ────────────────────────────────────────────────────────────────────
-- Append-only event log. Every AET movement is an event.
-- amount_uaet is always positive; direction determined by event_type.
-- seq_from: sender's sequence_no at time of submit — enables XRP-style ordering.
CREATE TABLE IF NOT EXISTS ledger_events (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    block_id        UUID,                                         -- assigned by batcher
    event_type      TEXT        NOT NULL
                    CHECK (event_type IN (
                        'PURCHASE','MINT','TRANSFER','TIP','SUBSCRIPTION',
                        'RELAY_REWARD','BOOST','GOVERNANCE','BURN',
                        'CREATOR_PAYOUT','REFUND','FEE'
                    )),
    from_pial       UUID,                                         -- null for MINT
    to_pial         UUID,                                         -- null for BURN
    amount_uaet     BIGINT      NOT NULL CHECK (amount_uaet > 0),
    fee_uaet        BIGINT      NOT NULL DEFAULT 0,
    seq_from        BIGINT,                                       -- sender sequence at submission
    -- Glyph ECDSA-P256 signature from the initiating party (hex)
    glyph_sig       TEXT,
    -- Idempotency key — prevents double-submission
    idempotency_key TEXT        UNIQUE,
    status          TEXT        NOT NULL DEFAULT 'confirmed'
                    CHECK (status IN ('pending','confirmed','failed','reversed')),
    metadata        JSONB       NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_le_from    ON ledger_events(from_pial, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_le_to      ON ledger_events(to_pial, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_le_type    ON ledger_events(event_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_le_block   ON ledger_events(block_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_le_status  ON ledger_events(status);

-- ── Blocks ────────────────────────────────────────────────────────────────────
-- Events are grouped into blocks every ~2 seconds by the batcher.
-- Each block includes the hash of the previous block (hash-chained).
-- events_root: XOR-fold SHA-256 of all event IDs in the block (lightweight Merkle substitute).
-- A full Merkle tree can be derived client-side from the event list for auditing.
CREATE TABLE IF NOT EXISTS ledger_blocks (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    block_number    BIGINT      NOT NULL UNIQUE,
    prev_hash       TEXT        NOT NULL DEFAULT '0000000000000000',  -- genesis = zeros
    block_hash      TEXT        NOT NULL,
    events_root     TEXT        NOT NULL DEFAULT '',  -- commitment to event set
    event_count     INTEGER     NOT NULL DEFAULT 0,
    total_volume    BIGINT      NOT NULL DEFAULT 0,
    fee_collected   BIGINT      NOT NULL DEFAULT 0,
    sealed_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_lb_number ON ledger_blocks(block_number DESC);

-- ── Supply ────────────────────────────────────────────────────────────────────
-- Global supply tracking. One row, mutated on every mint/burn.
CREATE TABLE IF NOT EXISTS aet_supply (
    id              INTEGER     PRIMARY KEY DEFAULT 1 CHECK (id = 1),  -- singleton
    total_supply    BIGINT      NOT NULL DEFAULT 0,
    circulating     BIGINT      NOT NULL DEFAULT 0,
    total_minted    BIGINT      NOT NULL DEFAULT 0,
    total_burned    BIGINT      NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- ── Funding rails ─────────────────────────────────────────────────────────────
-- Tracks fiat → Aethyr Credit ingress events before AET conversion.
CREATE TABLE IF NOT EXISTS funding_events (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    pial_id         UUID        NOT NULL,
    rail            TEXT        NOT NULL    -- apple_iap | gift_card | card | google_play | crypto
                    CHECK (rail IN ('apple_iap','gift_card','card','google_play','crypto','internal')),
    fiat_amount     BIGINT      NOT NULL,   -- in cents (USD)
    fiat_currency   TEXT        NOT NULL DEFAULT 'USD',
    credit_amount   BIGINT      NOT NULL,   -- Aethyr Credits issued
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','confirmed','failed','refunded')),
    external_ref    TEXT,                   -- Apple receipt / gift card code hash
    metadata        JSONB       NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_fe_pial  ON funding_events(pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_fe_rail  ON funding_events(rail, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_fe_status ON funding_events(status);
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    // Ensure singleton rows exist (idempotent — separate from DDL to avoid raw_sql ordering issues)
    sqlx::query("INSERT INTO aet_supply (id) VALUES (1) ON CONFLICT DO NOTHING")
        .execute(pool).await?;
    sqlx::query(
        "INSERT INTO ledger_accounts (pial_id, balance_uaet) VALUES \
         ('00000000-0000-0000-0000-000000000000', 0) ON CONFLICT DO NOTHING"
    )
    .execute(pool).await?;
    Ok(())
}

/// Ensure an account exists for a PIAL. Idempotent.
pub async fn ensure_account(pool: &PgPool, pial_id: uuid::Uuid) {
    let _ = sqlx::query(
        "INSERT INTO ledger_accounts (pial_id) VALUES ($1) ON CONFLICT DO NOTHING",
    )
    .bind(pial_id)
    .execute(pool)
    .await;
}

/// Returns balance in micro-AET.
pub async fn get_balance(pool: &PgPool, pial_id: uuid::Uuid) -> i64 {
    sqlx::query_scalar("SELECT balance_uaet FROM ledger_accounts WHERE pial_id = $1")
        .bind(pial_id)
        .fetch_optional(pool)
        .await
        .unwrap_or(None)
        .unwrap_or(0)
}

/// Next block number.
pub async fn next_block_number(pool: &PgPool) -> i64 {
    sqlx::query_scalar("SELECT COALESCE(MAX(block_number), 0) + 1 FROM ledger_blocks")
        .fetch_one(pool)
        .await
        .unwrap_or(1)
}

/// Previous block hash (for chaining).
pub async fn prev_block_hash(pool: &PgPool) -> String {
    sqlx::query_scalar(
        "SELECT block_hash FROM ledger_blocks ORDER BY block_number DESC LIMIT 1",
    )
    .fetch_optional(pool)
    .await
    .unwrap_or(None)
    .unwrap_or_else(|| "0000000000000000000000000000000000000000000000000000000000000000".into())
}
