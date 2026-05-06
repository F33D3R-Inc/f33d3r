/// Handle Registry — database schema and query helpers.
///
/// Design:
///   handles          — canonical handle → PIAL mapping (citext for case-insensitive uniqueness)
///   handle_events    — immutable append-only lifecycle audit log
///   handle_auctions  — ETHRA-priced handle marketplace
///   reserved_handles — system/trademark namespace protection
///
/// Architecture rule enforced here:
///   A handle is a pointer. PIAL is identity.
///   Transferring a handle NEVER transfers: followers, reputation, wallet, messages.

use anyhow::Result;
use sqlx::PgPool;

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
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    Ok(())
}

/// Check if a handle is in the reserved namespace.
pub async fn is_reserved(pool: &PgPool, handle: &str) -> bool {
    sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS(SELECT 1 FROM reserved_handles WHERE handle = $1)"
    )
    .bind(handle)
    .fetch_one(pool)
    .await
    .unwrap_or(false)
}

/// Check if a handle exists and is active.
pub async fn handle_exists(pool: &PgPool, handle: &str) -> bool {
    sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS(SELECT 1 FROM handles WHERE handle = $1)"
    )
    .bind(handle)
    .fetch_one(pool)
    .await
    .unwrap_or(false)
}

/// Append an event to the audit log (never fails silently in prod).
pub async fn log_event(
    pool:         &PgPool,
    handle_id:    uuid::Uuid,
    event_type:   &str,
    from_pial:    Option<uuid::Uuid>,
    to_pial:      Option<uuid::Uuid>,
    metadata:     serde_json::Value,
) {
    let _ = sqlx::query(
        "INSERT INTO handle_events (handle_id, event_type, from_pial_id, to_pial_id, metadata)
         VALUES ($1, $2, $3, $4, $5)"
    )
    .bind(handle_id).bind(event_type)
    .bind(from_pial).bind(to_pial)
    .bind(metadata)
    .execute(pool)
    .await;
}
