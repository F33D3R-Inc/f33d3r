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

-- Ledger checkpoints — Merkle roots received from Aethyr Ledger after each block seal.
-- Stored here so Ain Soph can cross-verify settlement without querying the ledger brain.
-- block_id is the ledger block_number (not a UUID) — incrementing integer from that chain.
CREATE TABLE IF NOT EXISTS ledger_checkpoints (
    id          BIGSERIAL   PRIMARY KEY,
    block_id    BIGINT      NOT NULL UNIQUE,   -- ledger block_number
    events_root TEXT        NOT NULL,          -- SHA-256 Merkle root (hex)
    tx_count    BIGINT      NOT NULL DEFAULT 0,
    sealed_at   TIMESTAMPTZ NOT NULL,          -- when ledger sealed the block
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_lc_block_id  ON ledger_checkpoints(block_id DESC);
CREATE INDEX IF NOT EXISTS idx_lc_sealed_at ON ledger_checkpoints(sealed_at DESC);

-- ══ Manhattan naming plane ═══════════════════════════════════════════════════
--
-- Ain Soph's durable handoff to Manhattan, and the constraint that makes the
-- handoff mean something.
--
-- Until now `accounts.user_id` was bare TEXT: no shape, no foreign key, no
-- resolution, and auto-created by every read and write. Whatever string a
-- caller sent became a wallet. That is how this database ended up holding two
-- accounts per person — one keyed on the PIAL and one keyed on feed-engine's
-- `users.id`, a row id belonging to another brain's database.
--
-- A wallet keyed on a handle follows the handle when registry-brain auctions or
-- reclaims it. A wallet keyed on a foreign row id detaches from its owner the
-- moment that brain renumbers. Neither is acceptable for the table that holds
-- the money, so ownership stops being a string a caller chose and becomes an
-- identity Manhattan resolved.

-- ── outbox ───────────────────────────────────────────────────────────────────
-- Manhattan is a separate brain reached over HTTP. A graph write sent
-- fire-and-forget is a graph write that a restart, a timeout or a rolling
-- deploy silently loses — and a naming plane that is silently missing rows is
-- worse than no naming plane, because everything downstream trusts it.
--
-- So the handoff is transactional. These rows are written by triggers on the
-- tables they describe, inside the same transaction as the row that caused
-- them. Either an account exists AND its registration is queued, or neither
-- happened. The drain in manhattan_outbox.rs delivers them in order, retrying
-- with backoff, and marks them delivered.
--
-- Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
-- does not know them and must not learn them.
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL CHECK (op IN ('node', 'name', 'edge', 'revoke_name')),
    payload         JSONB       NOT NULL,
    -- dedup_key makes enqueue idempotent, so a replayed insert or a backfill
    -- cannot queue the same registration twice.
    dedup_key       TEXT        NOT NULL UNIQUE,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ
);

-- The drain's only query: undelivered, due, oldest first.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_pending
    ON manhattan_outbox(next_attempt_at, id) WHERE delivered_at IS NULL;

-- ── Quarantine ───────────────────────────────────────────────────────────────
-- Head-of-line blocking is correct while a row can still land: later rows depend
-- on earlier ones, and running ahead would write edges pointing at nodes that do
-- not exist. It stops being correct the moment a row cannot land at all — then
-- it is not ordering the queue, it is ending it. One unfixable row used to stop
-- this brain's entire naming-plane output indefinitely, with a log line as the
-- only signal and a hand-written migration as the only cure.
--
-- A quarantined row is neither dropped nor delivered. It keeps its payload, its
-- error and its id; the drain simply stops claiming it, so everything queued
-- behind it moves again. It represents a naming-plane write that did NOT happen,
-- so it is reported as an incident on every tick until a person clears
-- blocked_at — loud by construction rather than by anyone remembering to look.
--
--   what is stuck:  SELECT id, op, blocked_at, blocked_reason, payload
--                     FROM manhattan_outbox
--                    WHERE blocked_at IS NOT NULL AND delivered_at IS NULL
--                    ORDER BY id;
--   put one back:   UPDATE manhattan_outbox
--                      SET blocked_at = NULL, blocked_reason = NULL,
--                          attempts = 0, next_attempt_at = NOW()
--                    WHERE id = $1;
--
-- Returning a row to the queue re-enters it at its original id, so the ordering
-- the drain depends on survives the round trip.
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_at     TIMESTAMPTZ;
ALTER TABLE manhattan_outbox ADD COLUMN IF NOT EXISTS blocked_reason TEXT;

-- The drain's query is now "undelivered, not quarantined, due, oldest first", so
-- it gets its own partial index. The one above stays: the backlog counters still
-- ask for everything undelivered, quarantined rows included, because a write
-- that did not happen must not disappear from the depth a health check reports.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_deliverable
    ON manhattan_outbox(next_attempt_at, id)
 WHERE delivered_at IS NULL AND blocked_at IS NULL;

-- What is quarantined, read on every drain tick.
CREATE INDEX IF NOT EXISTS idx_manhattan_outbox_blocked
    ON manhattan_outbox(id) WHERE blocked_at IS NOT NULL AND delivered_at IS NULL;

CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

-- ── what an account key has to look like ─────────────────────────────────────
-- The lowercase hyphenated UUID of a PIAL, or the platform float account, which
-- is a book rather than a person. Nothing else. A handle can never satisfy this
-- because a handle is not a UUID.
--
-- Read what this does and does not say. It is a SHAPE test. It cannot tell a
-- PIAL from any other UUID, because a foreign row id is UUID-shaped too — which
-- is precisely why it must never be used to decide that a key names a person.
-- Its only remaining jobs are documenting the CHECK constraints below and
-- counting the pre-constraint rows that /health reports.
CREATE OR REPLACE FUNCTION manhattan_is_identity_key(p_key TEXT)
RETURNS BOOLEAN AS $$
    SELECT p_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$';
$$ LANGUAGE sql IMMUTABLE STRICT;

-- ── ain-soph does not mint identity nodes ────────────────────────────────────
-- These triggers used to enqueue `node` rows naming `pial:<account key>`, guarded
-- only by the shape test above. A shape test cannot tell a PIAL from a foreign
-- row id, and accounts.user_id holds a mix of the two, so the guard passed and
-- Manhattan minted an identity node for a users.id — a second node for a person
-- who already had one, which is the exact failure the naming plane exists to
-- prevent. Nothing 403'd, because create_node assigns the owner from the kind
-- map: the writes landed owned by elohim-veni, from a brain that owns no
-- identity at all.
--
-- The kind `identity` belongs to elohim-veni, which registers every real PIAL as
-- it is minted. Ain-soph's job is to REFERENCE that name, never to create the
-- node behind it — and a `pial:` name that does not resolve is now the signal it
-- always should have been: the identity was never registered by its owner, and
-- attributing money to it is a guess. `identity.rs` resolves the claim through
-- Manhattan and refuses it if it does not already exist; this is the other half,
-- which is to stop manufacturing the evidence that made the guess look verified.
--
-- Dropped rather than left in place: a trigger that only fires on rows a
-- constraint already rejects is not harmless, it is a loaded gun waiting for the
-- constraint to be widened.
DROP TRIGGER IF EXISTS trg_manhattan_register_account  ON accounts;
DROP TRIGGER IF EXISTS trg_manhattan_register_proposal ON proposals;
DROP TRIGGER IF EXISTS trg_manhattan_register_vote     ON votes;
DROP FUNCTION IF EXISTS manhattan_register_wallet_identity();

-- ── the constraint ───────────────────────────────────────────────────────────
-- From here on the database itself refuses a wallet that is not owned by an
-- identity. Resolution in the handler is the road; this is the wall behind it,
-- so a path added later that forgets to resolve fails loudly instead of
-- quietly minting another shadow account.
--
-- NOT VALID is deliberate and is not a skipped error. Pre-existing rows hold
-- balances that this brain has no authority to reattribute — deciding whose
-- money a mis-keyed account holds is a reconciliation, not a migration. The
-- constraint therefore binds every future insert and update immediately, and
-- the rows that predate it are counted and surfaced on /health until someone
-- with that authority resolves them.
--
-- The pattern is written out here rather than called through
-- manhattan_is_identity_key: a constraint guarding the money table must not
-- depend on a function that a later CREATE OR REPLACE could quietly widen.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'accounts_owner_is_identity') THEN
        ALTER TABLE accounts ADD CONSTRAINT accounts_owner_is_identity
            CHECK (user_id = 'ain_soph_fees'
                   OR user_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
            NOT VALID;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'proposals_creator_is_identity') THEN
        ALTER TABLE proposals ADD CONSTRAINT proposals_creator_is_identity
            CHECK (creator_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
            NOT VALID;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'votes_voter_is_identity') THEN
        ALTER TABLE votes ADD CONSTRAINT votes_voter_is_identity
            CHECK (voter_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')
            NOT VALID;
    END IF;
END $$;

-- ── no backfill ──────────────────────────────────────────────────────────────
-- There used to be one here: every distinct account, proposal and vote key was
-- enqueued as an identity node on every boot, guarded by the same shape test.
-- It ran over accounts.user_id, which holds a mix of PIALs and foreign row ids,
-- so it minted an identity node for every mis-keyed account it found — and did
-- it again on each boot for any key added since.
--
-- It is gone rather than narrowed. A backfill that registers identities is a
-- backfill this brain has no authority to run: elohim-veni owns the `identity`
-- kind and registers every PIAL as it mints it, so a `pial:` name ain-soph
-- cannot resolve is not a gap to fill in, it is a wallet attributed to someone
-- who was never registered. That is a reconciliation for a person to make, and
-- unresolved_account_keys() counts it on /health until they do.
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
    let row: (i64,) = sqlx::query_as("SELECT balance FROM accounts WHERE user_id = $1 FOR UPDATE")
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
    let row: Option<(i64,)> = sqlx::query_as("SELECT balance FROM accounts WHERE user_id = $1")
        .bind(user_id)
        .fetch_optional(pool)
        .await?;
    Ok(row.map(|r| r.0))
}

/// How many account keys predate the identity constraint and are therefore not
/// attributable to a resolved identity.
///
/// These rows cannot be repaired from here: deciding whose money a mis-keyed
/// account holds is a reconciliation, not a migration, and this brain does not
/// own identity. Counting them is what keeps them from being forgotten — the
/// number is reported on every /health poll until it reaches zero.
pub async fn unresolved_account_keys(pool: &PgPool) -> Result<i64, sqlx::Error> {
    let row: (i64,) = sqlx::query_as(
        "SELECT COUNT(*) FROM accounts
          WHERE user_id <> 'ain_soph_fees'
            AND NOT manhattan_is_identity_key(user_id)",
    )
    .fetch_one(pool)
    .await?;
    Ok(row.0)
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
