/// Handle Registry — database schema and query helpers.
///
/// Design:
///   handles          — canonical handle → PIAL mapping (citext for case-insensitive uniqueness)
///   handle_events    — immutable append-only lifecycle audit log
///   handle_auctions  — ETHRA-priced handle marketplace
///   reserved_handles — system/trademark namespace protection
///   manhattan_outbox — durable, ordered handoff of handle names to Manhattan
///
/// Architecture rule enforced here:
///   A handle is a pointer. PIAL is identity.
///   Transferring a handle NEVER transfers: followers, reputation, wallet, messages.
///
/// Manhattan encodes that same rule across every brain: an identity node is
/// named `pial:<uuid>` and never moves; `handle:<h>` is a second name bound to
/// it, revocable and re-bindable. This brain is the authority for the `handle`
/// namespace, so it is the only brain permitted to bind or retire one — see
/// manhattan/migrations/0002_authority_map.sql.
use anyhow::{Context, Result};
use sqlx::PgPool;
use tracing::error;

const SCHEMA: &str = r#"
-- Case-insensitive text extension (handles are case-insensitive by spec)
CREATE EXTENSION IF NOT EXISTS citext;

-- ── Handle registry ───────────────────────────────────────────────────────────
-- One row per handle. handle is globally unique (citext = case-insensitive).
-- pial_id is the ONLY link to identity — nothing else here carries identity weight.
CREATE TABLE IF NOT EXISTS handles (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    handle          CITEXT      NOT NULL,
    pial_id         UUID        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','inactive','frozen','quarantined','auction','reserved')),
    tier            TEXT        NOT NULL DEFAULT 'standard'
                    CHECK (tier IN ('standard','premium','elite')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_bound_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(handle)
);
CREATE INDEX IF NOT EXISTS idx_handles_pial   ON handles(pial_id);
CREATE INDEX IF NOT EXISTS idx_handles_status ON handles(status);
-- Partial index for fast active resolution (hot path)
CREATE INDEX IF NOT EXISTS idx_handles_active ON handles(handle) WHERE status = 'active';

-- ── Lifecycle event log ───────────────────────────────────────────────────────
-- Append-only. Never update or delete rows.
-- event_type: register | bind | unbind | transfer | freeze | unfreeze |
--             reclaim | quarantine | auction_start | auction_settle
CREATE TABLE IF NOT EXISTS handle_events (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    handle_id       UUID        NOT NULL REFERENCES handles(id) ON DELETE RESTRICT,
    event_type      TEXT        NOT NULL,
    from_pial_id    UUID,
    to_pial_id      UUID,
    metadata        JSONB       NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_he_handle ON handle_events(handle_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_he_pial   ON handle_events(to_pial_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_he_type   ON handle_events(event_type, created_at DESC);

-- ── Auctions ──────────────────────────────────────────────────────────────────
-- Handle auctions settle in ETHRA. Bids logged here; settlement calls Ain Soph.
CREATE TABLE IF NOT EXISTS handle_auctions (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    handle_id             UUID        NOT NULL REFERENCES handles(id) ON DELETE RESTRICT,
    start_time            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    end_time              TIMESTAMPTZ NOT NULL,
    starting_price_aet    INTEGER     NOT NULL DEFAULT 100,
    current_price_aet     INTEGER     NOT NULL DEFAULT 100,
    highest_bidder_pial   UUID,
    status                TEXT        NOT NULL DEFAULT 'active'
                          CHECK (status IN ('active','ended','settled','cancelled')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_auctions_status ON handle_auctions(status, end_time);
CREATE INDEX IF NOT EXISTS idx_auctions_handle ON handle_auctions(handle_id);

-- ── Auction bids ──────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS auction_bids (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    auction_id      UUID        NOT NULL REFERENCES handle_auctions(id) ON DELETE CASCADE,
    bidder_pial     UUID        NOT NULL,
    amount_aet      INTEGER     NOT NULL CHECK (amount_aet > 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_bids_auction ON auction_bids(auction_id, created_at DESC);

-- ── Reserved namespace ────────────────────────────────────────────────────────
-- Handles that can NEVER be registered by users.
-- reason: system | trademark | admin | offensive
CREATE TABLE IF NOT EXISTS reserved_handles (
    handle  CITEXT  PRIMARY KEY,
    reason  TEXT    NOT NULL DEFAULT 'system'
);

INSERT INTO reserved_handles (handle, reason) VALUES
    ('admin',        'system'),   ('root',        'system'),
    ('system',       'system'),   ('null',        'system'),
    ('undefined',    'system'),   ('api',         'system'),
    ('support',      'system'),   ('help',        'system'),
    ('security',     'system'),   ('moderator',   'system'),
    ('official',     'system'),   ('verified',    'system'),
    ('f33d3r',       'trademark'),('aethyr',      'trademark'),
    ('pial',         'system'),   ('fabric',      'system'),
    ('elohim',       'system'),   ('vovin',       'system'),
    ('zior',         'system'),   ('nantar',      'system'),
    ('zodacare',     'system'),   ('thessalon',   'system'),
    ('registrar',    'system'),   ('registry',    'system')
ON CONFLICT DO NOTHING;

-- ── Manhattan naming plane — transactional outbox ────────────────────────────
--
-- Manhattan is a separate brain reached over HTTP. This brain is the authority
-- for the `handle` namespace (manhattan/migrations/0002_authority_map.sql), so
-- every handle lifecycle transition here is also a name operation there. A name
-- operation sent fire-and-forget is one that a restart, a timeout or a rolling
-- deploy silently loses, and a naming plane missing a handle rebind is not a
-- cosmetic defect: it leaves `handle:bob` resolving to the identity that gave
-- bob up. That is the exact identity bug Manhattan exists to end.
--
-- So the handoff is transactional. Rows are written by a trigger on `handles`,
-- inside the same transaction as the write that changed the truth. Either a
-- handle moved AND the name operations are queued, or neither happened. A
-- background drain then delivers them in id order, retrying with backoff.
--
-- Why the trigger is on `handles` and not on `handle_events`:
--   `handle_events` is written by a second statement after the `handles` write
--   commits, so an outbox driven from it would lose every transition that
--   crashed in between — which is precisely the durability the outbox is for.
--   `handles` is where truth changes, so that is where the enqueue belongs, and
--   any future writer that touches the table registers its change whether or
--   not its author remembered to.
--
-- Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
-- does not know them and must not learn them, or the two planes acquire a
-- second shared identifier and we are back where we started.
--
-- `edge` is deliberately absent from the op CHECK. The registrar binds names;
-- it asserts no relationships. An op the drain cannot execute would be a row
-- that blocks the queue forever, so the schema refuses to hold one.
CREATE TABLE IF NOT EXISTS manhattan_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    op              TEXT        NOT NULL CHECK (op IN ('node','name','revoke_name')),
    payload         JSONB       NOT NULL,
    -- dedup_key makes enqueue idempotent, so a re-run of the boot backfill
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

-- A transition is not content-addressable: bob may go A → B → A → B, and every
-- one of those four moves is a distinct revoke and a distinct rebind that must
-- all be delivered. Content-addressed dedup keys would collapse the third and
-- fourth into the first and second and leave bob revoked but never rebound. So
-- transition rows are keyed by a monotonic counter and are never deduplicated;
-- only the backfill, which restates a steady state rather than a transition,
-- uses a content-addressed key so re-running it is a no-op.
CREATE SEQUENCE IF NOT EXISTS manhattan_transition_seq;

CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

-- Which handle statuses carry a live name in Manhattan.
--   active  — bound and resolving.
--   auction — still owned by the seller until settlement; a handle must not
--             stop resolving merely because bidding opened on it.
-- Everything else (inactive, frozen, quarantined, reserved) is a state in which
-- the handle must resolve to nobody.
CREATE OR REPLACE FUNCTION manhattan_handle_resolves(p_status TEXT) RETURNS BOOLEAN AS $$
    SELECT p_status IN ('active','auction');
$$ LANGUAGE sql IMMUTABLE;

-- One trigger expresses every lifecycle endpoint, because every endpoint is a
-- transition of (handle, pial_id, status) and nothing else:
--
--   register        INSERT, active                       → node + bind
--   bind            inactive|quarantined → active        → node + bind
--   unbind          active → inactive                    → revoke
--   freeze          active → frozen                      → revoke
--   quarantine      active → quarantined                 → revoke
--   reclaim         active → reserved                    → revoke
--   unfreeze        frozen → active                      → node + bind
--   transfer        pial_id changes while active         → revoke, then bind
--   auction open    active → auction                     → (no change)
--   auction settle  auction → active, pial_id changes    → revoke, then bind
--   auction settle  auction → active, no bids            → (no change)
--
-- Revoke is enqueued before bind and the outbox is drained in id order, so a
-- handle is never momentarily resolving to two identities at once.
CREATE OR REPLACE FUNCTION manhattan_sync_handle() RETURNS TRIGGER AS $$
DECLARE
    was_bound BOOLEAN := FALSE;
    is_bound  BOOLEAN;
    moved     BOOLEAN;
    seq       BIGINT;
BEGIN
    is_bound := manhattan_handle_resolves(NEW.status);

    IF TG_OP = 'UPDATE' THEN
        was_bound := manhattan_handle_resolves(OLD.status);
        moved     := (NEW.pial_id IS DISTINCT FROM OLD.pial_id)
                  OR (NEW.handle  IS DISTINCT FROM OLD.handle);
    ELSE
        moved := TRUE;
    END IF;

    -- The name must stop resolving to its previous holder before anything else
    -- can hold it. Revoking on a handle rename as well as a pial change keeps
    -- this correct for a writer that renames a row, whether or not one exists
    -- today.
    IF was_bound AND (NOT is_bound OR moved) THEN
        seq := nextval('manhattan_transition_seq');
        PERFORM manhattan_enqueue(
            'revoke_name',
            'revoke:handle:' || OLD.handle::text || ':' || seq::text,
            jsonb_build_object('name', 'handle:' || OLD.handle::text));
    END IF;

    IF is_bound AND (NOT was_bound OR moved) THEN
        -- The identity node is named by PIAL and owned by elohim-veni. This
        -- brain does not own it and does not need to: naming rights are granted
        -- by namespace, and `handle` is this brain's.
        PERFORM manhattan_enqueue(
            'node',
            'node:pial:' || NEW.pial_id::text,
            jsonb_build_object(
                'kind',      'identity',
                'name',      'pial:' || NEW.pial_id::text,
                'namespace', 'pial'));

        seq := nextval('manhattan_transition_seq');
        PERFORM manhattan_enqueue(
            'name',
            'bind:handle:' || NEW.handle::text || ':' || NEW.pial_id::text || ':' || seq::text,
            jsonb_build_object(
                'node_name', 'pial:' || NEW.pial_id::text,
                'name',      'handle:' || NEW.handle::text,
                'namespace', 'handle'));
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_sync_handle ON handles;
CREATE TRIGGER trg_manhattan_sync_handle
    AFTER INSERT OR UPDATE OF handle, pial_id, status ON handles
    FOR EACH ROW EXECUTE FUNCTION manhattan_sync_handle();

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Every handle that already resolves is enqueued once, so Manhattan starts
-- complete instead of only knowing handles registered after this deploy. The
-- keys are content-addressed and the enqueue helper is ON CONFLICT DO NOTHING,
-- so this runs on every boot and is a no-op after the first.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT handle, pial_id FROM handles
              WHERE manhattan_handle_resolves(status) LOOP
        PERFORM manhattan_enqueue(
            'node',
            'node:pial:' || r.pial_id::text,
            jsonb_build_object(
                'kind','identity',
                'name','pial:' || r.pial_id::text,
                'namespace','pial'));
        PERFORM manhattan_enqueue(
            'name',
            'backfill:handle:' || r.handle::text || ':' || r.pial_id::text,
            jsonb_build_object(
                'node_name','pial:' || r.pial_id::text,
                'name','handle:' || r.handle::text,
                'namespace','handle'));
    END LOOP;
END $$;
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    Ok(())
}

/// Check if a handle is in the reserved namespace.
pub async fn is_reserved(pool: &PgPool, handle: &str) -> bool {
    sqlx::query_scalar::<_, bool>("SELECT EXISTS(SELECT 1 FROM reserved_handles WHERE handle = $1)")
        .bind(handle)
        .fetch_one(pool)
        .await
        .unwrap_or(false)
}

/// Check if a handle exists and is active.
pub async fn handle_exists(pool: &PgPool, handle: &str) -> bool {
    sqlx::query_scalar::<_, bool>("SELECT EXISTS(SELECT 1 FROM handles WHERE handle = $1)")
        .bind(handle)
        .fetch_one(pool)
        .await
        .unwrap_or(false)
}

/// Append an event to the audit log (never fails silently in prod).
pub async fn log_event(
    pool: &PgPool,
    handle_id: uuid::Uuid,
    event_type: &str,
    from_pial: Option<uuid::Uuid>,
    to_pial: Option<uuid::Uuid>,
    metadata: serde_json::Value,
) {
    // The audit log is append-only and must never fail quietly: a lifecycle
    // event that happened but was never recorded is an unexplained handle
    // movement. The mutation itself has already committed, so this cannot
    // change the response — but it says so, loudly, every time.
    if let Err(e) = sqlx::query(
        "INSERT INTO handle_events (handle_id, event_type, from_pial_id, to_pial_id, metadata)
         VALUES ($1, $2, $3, $4, $5)",
    )
    .bind(handle_id)
    .bind(event_type)
    .bind(from_pial)
    .bind(to_pial)
    .bind(metadata)
    .execute(pool)
    .await
    {
        error!(
            error      = %e,
            handle_id  = %handle_id,
            event_type = event_type,
            "handle_events: lifecycle event could not be recorded",
        );
    }
}

/// How many Manhattan name operations are queued and not yet delivered.
///
/// A backlog that only grows means handle resolution across the platform is
/// drifting away from this brain's truth, so it is reported on /health rather
/// than left to be discovered.
pub async fn manhattan_outbox_pending(pool: &PgPool) -> Result<i64> {
    sqlx::query_scalar::<_, i64>("SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL")
        .fetch_one(pool)
        .await
        .context("counting undelivered manhattan_outbox rows")
}
