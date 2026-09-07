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

-- ── Manhattan naming plane ────────────────────────────────────────────────────
-- This database holds four identity-bearing columns under three spellings:
-- ledger_accounts.pial_id, funding_events.pial_id, ledger_events.from_pial and
-- ledger_events.to_pial. Every one of them is a REFERENCE to an identity that
-- elohim-veni owns; none of them is the identity itself. So this brain does not
-- join into another brain's tables to find out who a settlement belongs to. It
-- registers the PIAL as a NAME in Manhattan and resolves that name there.
--
-- The handoff is transactional, not fire-and-forget. Manhattan is a separate
-- service reached over HTTP; a registration sent and lost to a restart, a
-- timeout or a rolling deploy leaves the naming plane silently incomplete — and
-- on a ledger "silently incomplete" means a settlement whose counterparty
-- nobody can name. So the registration is enqueued by a trigger, inside the SAME
-- transaction as the money row that caused it: either the transfer committed AND
-- its counterparties are queued for registration, or neither happened. A
-- rollback takes the registration with it. A background drain then delivers them
-- in order, retrying with backoff, and marks them delivered.
--
-- The triggers are why this cannot rot. A future writer that inserts a ledger
-- event through some path nobody has thought of yet still registers its
-- counterparties, because registering is a property of the table and not a step
-- a caller must remember.
--
-- Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
-- does not know them and must not learn them, or the two planes acquire a second
-- shared identifier and we are back where we started.
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL CHECK (op IN ('node', 'name', 'edge', 'revoke_name')),
    payload         JSONB       NOT NULL,
    -- dedup_key makes enqueue idempotent, so a replayed insert or a backfill
    -- cannot queue the same registration twice. It is also what lets every one
    -- of the four columns enqueue the same PIAL without producing four rows.
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

-- migrate() re-applies this whole schema on every boot, so the one-shot backfill
-- below needs a record that it has already run. Without it a ledger with years
-- of events would re-scan every event table on every restart to enqueue rows the
-- unique dedup_key would only discard again.
CREATE TABLE IF NOT EXISTS manhattan_backfill (
    name         TEXT        PRIMARY KEY,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

-- A person's identity node is named by PIAL, never by handle. A handle is a
-- pointer that registry-brain can auction, transfer or reclaim; PIAL is the root
-- that never moves. A ledger account keyed on a handle would follow the handle
-- to its next owner and pay the wrong person, so no name in the handle namespace
-- is ever minted from here — this brain enqueues PIALs and nothing else.
--
-- The treasury is the one pial_id in this database that is not a person. It is
-- the platform's fee sink, keyed on the nil UUID precisely because no PIAL is
-- ever issued that value. Registering it would put a person-shaped identity node
-- in the naming plane for something that is not a person, so it is skipped here
-- and refused resolution in the service.
CREATE OR REPLACE FUNCTION manhattan_enqueue_identity(p_pial UUID) RETURNS VOID AS $$
BEGIN
    IF p_pial IS NULL OR p_pial = '00000000-0000-0000-0000-000000000000'::uuid THEN
        RETURN;
    END IF;

    PERFORM manhattan_enqueue(
        'node',
        'node:pial:' || p_pial::text,
        jsonb_build_object(
            'kind',      'identity',
            'name',      'pial:' || p_pial::text,
            'namespace', 'pial'));
END;
$$ LANGUAGE plpgsql;

-- ── ledger_accounts.pial_id ──────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_register_account() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_enqueue_identity(NEW.pial_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_account ON ledger_accounts;
CREATE TRIGGER trg_manhattan_register_account
    AFTER INSERT ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_account();

-- ── funding_events.pial_id ───────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_register_funding() RETURNS TRIGGER AS $$
BEGIN
    PERFORM manhattan_enqueue_identity(NEW.pial_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_funding ON funding_events;
CREATE TRIGGER trg_manhattan_register_funding
    AFTER INSERT ON funding_events
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_funding();

-- ── ledger_events.from_pial / to_pial ────────────────────────────────────────
-- Both sides of a settlement. Either may be NULL by design — a MINT has no
-- sender and a BURN has no receiver — and the enqueue helper skips NULL rather
-- than the trigger having to know which event types allow which.
--
-- The two counterparties are enqueued in UUID order, not sender-then-receiver.
-- `ON CONFLICT (dedup_key) DO NOTHING` waits on a concurrent uncommitted insert
-- of the same key, so if this trigger enqueued in sender-then-receiver order,
-- two simultaneous transfers in opposite directions between the same pair of new
-- PIALs would take those two index locks in opposite orders and deadlock —
-- and Postgres would resolve it by aborting a settlement. Sorting means every
-- transaction takes them in the same order, so there is no cycle to detect.
--
-- Nothing today can reach that state, because ensure_account already commits
-- both rows before the transfer transaction opens. That is an accident of call
-- ordering in one handler, not a property of this table, and a ledger should not
-- depend on an accident to avoid aborting a settlement.
CREATE OR REPLACE FUNCTION manhattan_register_settlement() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.from_pial IS NOT NULL AND NEW.to_pial IS NOT NULL
       AND NEW.to_pial < NEW.from_pial THEN
        PERFORM manhattan_enqueue_identity(NEW.to_pial);
        PERFORM manhattan_enqueue_identity(NEW.from_pial);
    ELSE
        PERFORM manhattan_enqueue_identity(NEW.from_pial);
        PERFORM manhattan_enqueue_identity(NEW.to_pial);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_register_settlement ON ledger_events;
CREATE TRIGGER trg_manhattan_register_settlement
    AFTER INSERT ON ledger_events
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_settlement();

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Every PIAL this ledger already holds money for is enqueued once, so the naming
-- plane starts complete instead of only knowing about accounts touched after
-- this deploy. An account nobody can resolve is an account nobody can audit.
DO $$
DECLARE r RECORD;
BEGIN
    IF EXISTS (SELECT 1 FROM manhattan_backfill WHERE name = 'identities_v1') THEN
        RETURN;
    END IF;

    FOR r IN SELECT DISTINCT pial_id AS p FROM ledger_accounts LOOP
        PERFORM manhattan_enqueue_identity(r.p);
    END LOOP;

    FOR r IN SELECT DISTINCT pial_id AS p FROM funding_events LOOP
        PERFORM manhattan_enqueue_identity(r.p);
    END LOOP;

    FOR r IN SELECT DISTINCT from_pial AS p FROM ledger_events WHERE from_pial IS NOT NULL LOOP
        PERFORM manhattan_enqueue_identity(r.p);
    END LOOP;

    FOR r IN SELECT DISTINCT to_pial AS p FROM ledger_events WHERE to_pial IS NOT NULL LOOP
        PERFORM manhattan_enqueue_identity(r.p);
    END LOOP;

    INSERT INTO manhattan_backfill (name) VALUES ('identities_v1');
END $$;
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    // Ensure singleton rows exist (idempotent — separate from DDL to avoid raw_sql ordering issues)
    sqlx::query("INSERT INTO aet_supply (id) VALUES (1) ON CONFLICT DO NOTHING")
        .execute(pool)
        .await?;
    sqlx::query(
        "INSERT INTO ledger_accounts (pial_id, balance_uaet) VALUES \
         ('00000000-0000-0000-0000-000000000000', 0) ON CONFLICT DO NOTHING",
    )
    .execute(pool)
    .await?;
    Ok(())
}

/// Ensure an account exists for a PIAL. Idempotent.
///
/// The failure is returned rather than discarded. Every mint and credit in this
/// service is an UPDATE keyed on this row; if the insert fails and nobody is
/// told, the UPDATE matches zero rows, the ledger_event and the supply counter
/// are still written, and AET is minted into an account that does not exist. A
/// swallowed error here is a hole in the money supply, so there is no swallowing.
pub async fn ensure_account(pool: &PgPool, pial_id: uuid::Uuid) -> Result<()> {
    sqlx::query("INSERT INTO ledger_accounts (pial_id) VALUES ($1) ON CONFLICT DO NOTHING")
        .bind(pial_id)
        .execute(pool)
        .await?;
    Ok(())
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
    sqlx::query_scalar("SELECT block_hash FROM ledger_blocks ORDER BY block_number DESC LIMIT 1")
        .fetch_optional(pool)
        .await
        .unwrap_or(None)
        .unwrap_or_else(|| {
            "0000000000000000000000000000000000000000000000000000000000000000".into()
        })
}
