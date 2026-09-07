use crate::models::*;
use crate::pial_ref::PialRef;
use anyhow::Result;
use chrono::{DateTime, Utc};
use sqlx::{PgPool, Row};
use uuid::Uuid;

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS shops (
    seller_pial  UUID        PRIMARY KEY,
    btc_address  TEXT        NOT NULL,
    is_active    BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS listings (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_pial      UUID        NOT NULL REFERENCES shops(seller_pial),
    title          TEXT        NOT NULL,
    description    TEXT        NOT NULL DEFAULT '',
    price_sats     BIGINT      NOT NULL,
    content_url    TEXT        NOT NULL,
    content_hash   TEXT        NOT NULL,
    cek_encrypted  BYTEA       NOT NULL,
    phash          TEXT,
    visibility     TEXT        NOT NULL DEFAULT 'public',
    is_active      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS purchases (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id     UUID        NOT NULL REFERENCES listings(id),
    buyer_pial     UUID        NOT NULL,
    btc_address    TEXT        NOT NULL,
    expected_sats  BIGINT      NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'pending',
    cek_for_buyer  BYTEA,
    expires_at     TIMESTAMPTZ NOT NULL,
    confirmed_at   TIMESTAMPTZ,
    delivered_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_purchases_status      ON purchases(status);
CREATE INDEX IF NOT EXISTS idx_purchases_btc_address ON purchases(btc_address);

CREATE TABLE IF NOT EXISTS marketplace_subscriptions (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_pial   UUID        NOT NULL,
    seller_pial  UUID        NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'active',
    period_end   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(buyer_pial, seller_pial)
);
"#;

const COMMERCE_SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS creator_eligibility (
    pial_id              TEXT PRIMARY KEY,
    kyc_tier             INTEGER NOT NULL DEFAULT 0,
    monetization_enabled BOOLEAN NOT NULL DEFAULT false,
    enabled_at           TIMESTAMPTZ,
    disabled_at          TIMESTAMPTZ,
    disabled_reason      TEXT,
    stripe_customer_id   TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS subscription_plans (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_pial_id TEXT NOT NULL,
    name            TEXT NOT NULL,
    price_aet       BIGINT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    is_active       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_plans_creator ON subscription_plans(creator_pial_id) WHERE is_active;

CREATE TABLE IF NOT EXISTS subscriptions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscriber_pial_id      TEXT NOT NULL,
    creator_pial_id         TEXT NOT NULL,
    plan_id                 UUID REFERENCES subscription_plans(id),
    status                  TEXT NOT NULL DEFAULT 'active',
    current_period_start    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    current_period_end      TIMESTAMPTZ NOT NULL,
    stripe_subscription_id  TEXT,
    cancelled_at            TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_subs_subscriber ON subscriptions(subscriber_pial_id);
CREATE INDEX IF NOT EXISTS idx_subs_creator    ON subscriptions(creator_pial_id);

CREATE TABLE IF NOT EXISTS ppv_items (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_pial_id TEXT NOT NULL,
    content_id      TEXT NOT NULL,
    price_aet       BIGINT NOT NULL,
    title           TEXT NOT NULL DEFAULT '',
    is_active       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ppv_purchases (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_pial_id    TEXT NOT NULL,
    ppv_item_id      UUID NOT NULL REFERENCES ppv_items(id),
    amount_aet       BIGINT NOT NULL,
    ain_soph_tx_id   TEXT,
    stripe_session_id TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(buyer_pial_id, ppv_item_id)
);

CREATE TABLE IF NOT EXISTS tips (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sender_pial_id    TEXT NOT NULL,
    recipient_pial_id TEXT NOT NULL,
    amount_aet        BIGINT NOT NULL,
    message           TEXT NOT NULL DEFAULT '',
    content_id        TEXT,
    ain_soph_tx_id    TEXT,
    stripe_session_id TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS commerce_transactions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tx_type         TEXT NOT NULL,
    reference_id    UUID NOT NULL,
    payer_pial_id   TEXT NOT NULL,
    creator_pial_id TEXT NOT NULL,
    gross_aet       BIGINT NOT NULL,
    ain_soph_tx_id  TEXT,
    idempotency_key TEXT,
    stripe_session_id TEXT,
    status          TEXT NOT NULL DEFAULT 'completed',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ctx_creator ON commerce_transactions(creator_pial_id);
CREATE INDEX IF NOT EXISTS idx_ctx_payer   ON commerce_transactions(payer_pial_id);

-- ── Payment state: the record exists before the money moves ──────────────────
-- subscribe / purchase_ppv / send_tip used to call Ain Soph first and INSERT
-- afterwards. A database failure in that window moved a user's money and left no
-- local record of it at all — nothing to reconcile against, nothing to refund
-- from, and no way to even discover it had happened.
--
-- The order is now: write the intent as 'pending', move the money, then settle
-- the row to 'completed' or 'failed'. An abandoned 'pending' row costs nothing;
-- a transfer with no row costs a user their balance. commerce_transactions
-- already carried status and idempotency_key for this; the two domain tables did
-- not.
--
-- Existing rows default to 'completed' because they are, by construction: every
-- one of them was written after a transfer that had already succeeded.
ALTER TABLE tips          ADD COLUMN IF NOT EXISTS status         TEXT NOT NULL DEFAULT 'completed';
ALTER TABLE tips          ADD COLUMN IF NOT EXISTS failure_reason TEXT;
ALTER TABLE ppv_purchases ADD COLUMN IF NOT EXISTS status         TEXT NOT NULL DEFAULT 'completed';
ALTER TABLE ppv_purchases ADD COLUMN IF NOT EXISTS failure_reason TEXT;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS failure_reason TEXT;
ALTER TABLE commerce_transactions ADD COLUMN IF NOT EXISTS failure_reason TEXT;

-- A failed attempt must not permanently block a retry. The unique pair only
-- applies to purchases that are still live or already paid for.
ALTER TABLE ppv_purchases DROP CONSTRAINT IF EXISTS ppv_purchases_buyer_pial_id_ppv_item_id_key;
CREATE UNIQUE INDEX IF NOT EXISTS uq_ppv_purchase_live
    ON ppv_purchases (buyer_pial_id, ppv_item_id) WHERE status <> 'failed';

-- The reconciliation surface: a row stuck pending is money that may have moved
-- with nobody watching. It is meant to be looked at, so it is indexed.
CREATE INDEX IF NOT EXISTS idx_ctx_pending
    ON commerce_transactions (created_at) WHERE status = 'pending';

-- The payment state machine has exactly three states everywhere it appears, so
-- a typo or a fourth invented state fails at write time rather than quietly
-- becoming a row no reader filters for.
--
-- Note what this does and does not buy. It makes an INVALID STATE unwritable.
-- It does not make a reader remember to filter — that is the harder half, and
-- the reason every reader of these columns was audited when 'pending' was
-- introduced rather than only the ones being edited.
ALTER TABLE tips          DROP CONSTRAINT IF EXISTS ck_tips_status;
ALTER TABLE tips          ADD  CONSTRAINT ck_tips_status
    CHECK (status IN ('pending', 'completed', 'failed'));
ALTER TABLE ppv_purchases DROP CONSTRAINT IF EXISTS ck_ppv_purchases_status;
ALTER TABLE ppv_purchases ADD  CONSTRAINT ck_ppv_purchases_status
    CHECK (status IN ('pending', 'completed', 'failed'));
ALTER TABLE commerce_transactions DROP CONSTRAINT IF EXISTS ck_ctx_status;
ALTER TABLE commerce_transactions ADD  CONSTRAINT ck_ctx_status
    CHECK (status IN ('pending', 'completed', 'failed'));
-- subscriptions carries its own vocabulary: a paid subscription is 'active',
-- and it can later be 'cancelled'.
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS ck_subscriptions_status;
ALTER TABLE subscriptions ADD  CONSTRAINT ck_subscriptions_status
    CHECK (status IN ('pending', 'active', 'cancelled', 'failed'));

CREATE TABLE IF NOT EXISTS stripe_idempotency_keys (
    key               TEXT PRIMARY KEY,
    stripe_session_id TEXT,
    result_json       TEXT NOT NULL DEFAULT '{}',
    fulfilled         BOOLEAN NOT NULL DEFAULT false,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_stripe_idem_session ON stripe_idempotency_keys(stripe_session_id);

ALTER TABLE shops ADD COLUMN IF NOT EXISTS xrp_address TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_shops_xrp_active ON shops(xrp_address) WHERE xrp_address != '' AND is_active = TRUE;

ALTER TABLE listings ADD COLUMN IF NOT EXISTS price_xrp_drops BIGINT NOT NULL DEFAULT 0;
ALTER TABLE listings ADD COLUMN IF NOT EXISTS payment_currency TEXT NOT NULL DEFAULT 'btc';

ALTER TABLE purchases ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'btc';
ALTER TABLE purchases ADD COLUMN IF NOT EXISTS xrp_address TEXT NOT NULL DEFAULT '';
"#;

// Themis's durable handoff to the Manhattan naming plane.
//
// Manhattan is a separate brain reached over HTTP. A registration sent
// fire-and-forget is a registration that a restart, a timeout or a rolling
// deploy silently loses — and a naming plane that is silently missing rows is
// worse than no naming plane, because everything downstream trusts it. In a
// database that moves money it is worse still: the identity being paid would be
// one this brain assumed rather than one the plane agreed on.
//
// So the handoff is transactional. These rows are written by triggers on the
// tables they describe, inside the same transaction as the row that caused
// them. Either a purchase exists AND its identities are queued for
// registration, or neither happened. The drain in manhattan_outbox.rs delivers
// them in order, retrying with backoff, and marks them delivered.
//
// The triggers are the reason this cannot rot. A future writer that inserts a
// tip through some path nobody has thought of yet still registers the identity
// it pays, because registering is a property of the table and not a step a
// caller must remember.
//
// Payloads carry NAMES, never node ids. Manhattan assigns node ids; this side
// does not know them and must not learn them, or the two planes acquire a
// second shared identifier and we are back where we started.
//
// Themis enqueues 'node' ops only. That is not an omission — it is the
// authority map (manhattan/migrations/0002_authority_map.sql) showing through.
// Any brain may register the existence of a name it must reference, and
// Manhattan assigns the owner from the kind: an identity registered here is
// owned by elohim-veni, a work by feed-engine, media by caeor. Asserting FACTS
// about those nodes — binding extra names in a governed namespace, drawing
// edges out of a node this brain does not own — is refused at the wire with
// 403, correctly, because themis is a reference-only participant in the naming
// plane. The drain still understands the other three ops so that the pattern is
// identical in every brain, and so a future op that themis IS permitted to
// write needs no new delivery path.
const MANHATTAN_OUTBOX_SCHEMA: &str = r#"
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

-- ── enqueue helper ───────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION manhattan_enqueue(p_op TEXT, p_dedup TEXT, p_payload JSONB)
RETURNS VOID AS $$
BEGIN
    INSERT INTO manhattan_outbox (op, dedup_key, payload)
    VALUES (p_op, p_dedup, p_payload)
    ON CONFLICT (dedup_key) DO NOTHING;
END;
$$ LANGUAGE plpgsql;

-- ── names ────────────────────────────────────────────────────────────────────
-- A PIAL is a UUID. Five of themis's fifteen identity columns are typed UUID and
-- ten are typed TEXT, and nothing today stops a TEXT column from holding
-- something that is not a UUID at all. A malformed value must not become a
-- malformed name: it would sit at the head of the queue forever, blocking every
-- registration behind it. So the name is built only when the value really is a
-- PIAL, in canonical lowercase hyphenated form so that two spellings of one
-- identity can never become two nodes.
CREATE OR REPLACE FUNCTION manhattan_pial_name(p_value TEXT)
RETURNS TEXT AS $$
BEGIN
    IF p_value IS NULL THEN
        RETURN NULL;
    END IF;
    IF btrim(p_value) !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
        RETURN NULL;
    END IF;
    RETURN 'pial:' || lower(btrim(p_value));
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- Encrypted marketplace content is content-addressed. A work or media item is
-- named by its id or by its content address, and both answer to one node — which
-- is exactly what stops themis from assuming feed-engine's id scheme.
CREATE OR REPLACE FUNCTION manhattan_content_name(p_value TEXT)
RETURNS TEXT AS $$
DECLARE v TEXT;
BEGIN
    IF p_value IS NULL THEN
        RETURN NULL;
    END IF;
    v := btrim(p_value);
    IF v = '' THEN
        RETURN NULL;
    END IF;
    IF v ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
        RETURN 'uuid:' || lower(v);
    END IF;
    v := regexp_replace(v, '^sha256:', '', 'i');
    IF v ~* '^[0-9a-f]{64}$' THEN
        RETURN 'cid:sha256:' || lower(v);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

CREATE OR REPLACE FUNCTION manhattan_enqueue_node(p_kind TEXT, p_name TEXT)
RETURNS VOID AS $$
BEGIN
    IF p_name IS NULL THEN
        RETURN;
    END IF;
    PERFORM manhattan_enqueue(
        'node',
        'node:' || p_name,
        jsonb_build_object(
            'kind',      p_kind,
            'name',      p_name,
            'namespace', split_part(p_name, ':', 1)));
END;
$$ LANGUAGE plpgsql;

-- ── the fifteen identity columns ─────────────────────────────────────────────
-- One trigger function, applied to every table that holds an identity
-- reference, told at attach time which of its columns are PIALs. Eight column
-- spellings, one behaviour: this is where the schema stops disagreeing with
-- itself about what a PIAL is, even though the columns are still named apart.
CREATE OR REPLACE FUNCTION manhattan_register_pials() RETURNS TRIGGER AS $$
DECLARE
    rec JSONB := to_jsonb(NEW);
    col TEXT;
BEGIN
    FOREACH col IN ARRAY TG_ARGV LOOP
        PERFORM manhattan_enqueue_node('identity', manhattan_pial_name(rec ->> col));
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- TG_ARGV[0] is the node kind, the rest are the columns holding a reference to
-- something of that kind.
CREATE OR REPLACE FUNCTION manhattan_register_content() RETURNS TRIGGER AS $$
DECLARE
    rec  JSONB := to_jsonb(NEW);
    kind TEXT  := TG_ARGV[0];
    i    INT;
BEGIN
    FOR i IN 1 .. TG_NARGS - 1 LOOP
        PERFORM manhattan_enqueue_node(kind, manhattan_content_name(rec ->> TG_ARGV[i]));
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_manhattan_pials ON shops;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF seller_pial ON shops
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('seller_pial');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON listings;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF shop_pial ON listings
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('shop_pial');

DROP TRIGGER IF EXISTS trg_manhattan_content ON listings;
CREATE TRIGGER trg_manhattan_content AFTER INSERT OR UPDATE OF content_hash ON listings
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_content('work', 'content_hash');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON purchases;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF buyer_pial ON purchases
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('buyer_pial');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON marketplace_subscriptions;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF buyer_pial, seller_pial ON marketplace_subscriptions
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('buyer_pial', 'seller_pial');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON creator_eligibility;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF pial_id ON creator_eligibility
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON subscription_plans;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF creator_pial_id ON subscription_plans
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('creator_pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON subscriptions;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF subscriber_pial_id, creator_pial_id ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('subscriber_pial_id', 'creator_pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON ppv_items;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF creator_pial_id ON ppv_items
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('creator_pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_content ON ppv_items;
CREATE TRIGGER trg_manhattan_content AFTER INSERT OR UPDATE OF content_id ON ppv_items
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_content('work', 'content_id');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON ppv_purchases;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF buyer_pial_id ON ppv_purchases
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('buyer_pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON tips;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF sender_pial_id, recipient_pial_id ON tips
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('sender_pial_id', 'recipient_pial_id');

DROP TRIGGER IF EXISTS trg_manhattan_content ON tips;
CREATE TRIGGER trg_manhattan_content AFTER INSERT OR UPDATE OF content_id ON tips
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_content('work', 'content_id');

DROP TRIGGER IF EXISTS trg_manhattan_pials ON commerce_transactions;
CREATE TRIGGER trg_manhattan_pials AFTER INSERT OR UPDATE OF payer_pial_id, creator_pial_id ON commerce_transactions
    FOR EACH ROW EXECUTE FUNCTION manhattan_register_pials('payer_pial_id', 'creator_pial_id');

-- ── backfill ─────────────────────────────────────────────────────────────────
-- Everything that already exists is enqueued once, so the naming plane starts
-- complete instead of only knowing about identities this brain touched after
-- the deploy. The pair list below is the canonical inventory of the fifteen
-- columns and the three content columns — read it as the rename plan's input.
--
-- Themis applies its schema on every boot rather than through numbered
-- migrations, so the backfill is guarded on the outbox never having held a row.
-- Delivered rows are kept, not deleted, which makes that an exact "has this
-- brain ever enqueued anything" test rather than a "is the queue empty now" one.
DO $$
DECLARE
    r RECORD;
    v TEXT;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM manhattan_outbox) THEN
    FOR r IN SELECT * FROM (VALUES
        ('shops',                     'seller_pial'),
        ('listings',                  'shop_pial'),
        ('purchases',                 'buyer_pial'),
        ('marketplace_subscriptions', 'buyer_pial'),
        ('marketplace_subscriptions', 'seller_pial'),
        ('creator_eligibility',       'pial_id'),
        ('subscription_plans',        'creator_pial_id'),
        ('subscriptions',             'subscriber_pial_id'),
        ('subscriptions',             'creator_pial_id'),
        ('ppv_items',                 'creator_pial_id'),
        ('ppv_purchases',             'buyer_pial_id'),
        ('tips',                      'sender_pial_id'),
        ('tips',                      'recipient_pial_id'),
        ('commerce_transactions',     'payer_pial_id'),
        ('commerce_transactions',     'creator_pial_id')
    ) AS t(tbl, col) LOOP
        FOR v IN EXECUTE format(
            'SELECT DISTINCT %I::text FROM %I WHERE %I IS NOT NULL', r.col, r.tbl, r.col)
        LOOP
            PERFORM manhattan_enqueue_node('identity', manhattan_pial_name(v));
        END LOOP;
    END LOOP;

    FOR r IN SELECT * FROM (VALUES
        ('listings',  'content_hash', 'work'),
        ('ppv_items', 'content_id',   'work'),
        ('tips',      'content_id',   'work')
    ) AS t(tbl, col, kind) LOOP
        FOR v IN EXECUTE format(
            'SELECT DISTINCT %I::text FROM %I WHERE %I IS NOT NULL', r.col, r.tbl, r.col)
        LOOP
            PERFORM manhattan_enqueue_node(r.kind, manhattan_content_name(v));
        END LOOP;
    END LOOP;
  END IF;
END $$;
"#;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    // One-time migration: old BTC-based subscriptions(buyer_pial) → marketplace_subscriptions.
    // If marketplace_subscriptions already exists (from a prior partial migration), drop the
    // old subscriptions instead — it will be recreated as the commerce table by COMMERCE_SCHEMA.
    sqlx::query(
        r#"
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'subscriptions' AND column_name = 'buyer_pial'
    ) THEN
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_name = 'marketplace_subscriptions'
        ) THEN
            ALTER TABLE subscriptions RENAME TO marketplace_subscriptions;
        ELSE
            DROP TABLE subscriptions;
        END IF;
    END IF;
END $$;
"#,
    )
    .execute(pool)
    .await?;
    sqlx::raw_sql(COMMERCE_SCHEMA).execute(pool).await?;
    // Last, because its triggers and backfill read every table above.
    sqlx::raw_sql(MANHATTAN_OUTBOX_SCHEMA).execute(pool).await?;
    Ok(())
}

/// How many registrations are queued and not yet delivered to Manhattan.
/// A backlog that only grows is the visible symptom of a drain that is stuck on
/// a row it cannot deliver, which is why /health reports it.
pub async fn manhattan_outbox_backlog(pool: &PgPool) -> Result<i64> {
    let depth: i64 =
        sqlx::query_scalar("SELECT COUNT(*) FROM manhattan_outbox WHERE delivered_at IS NULL")
            .fetch_one(pool)
            .await?;
    Ok(depth)
}

fn row_to_shop(row: &sqlx::postgres::PgRow) -> Result<Shop> {
    Ok(Shop {
        seller_pial: row.try_get("seller_pial")?,
        btc_address: row.try_get("btc_address")?,
        xrp_address: row.try_get("xrp_address")?,
        is_active: row.try_get("is_active")?,
        created_at: row.try_get("created_at")?,
    })
}

fn row_to_listing(row: &sqlx::postgres::PgRow) -> Result<Listing> {
    Ok(Listing {
        id: row.try_get("id")?,
        shop_pial: row.try_get("shop_pial")?,
        title: row.try_get("title")?,
        description: row.try_get("description")?,
        price_sats: row.try_get("price_sats")?,
        price_xrp_drops: row.try_get("price_xrp_drops")?,
        payment_currency: row.try_get("payment_currency")?,
        content_url: row.try_get("content_url")?,
        content_hash: row.try_get("content_hash")?,
        cek_encrypted: row.try_get("cek_encrypted")?,
        phash: row.try_get("phash")?,
        visibility: row.try_get("visibility")?,
        is_active: row.try_get("is_active")?,
        created_at: row.try_get("created_at")?,
    })
}

fn row_to_purchase(row: &sqlx::postgres::PgRow) -> Result<Purchase> {
    Ok(Purchase {
        id: row.try_get("id")?,
        listing_id: row.try_get("listing_id")?,
        buyer_pial: row.try_get("buyer_pial")?,
        btc_address: row.try_get("btc_address")?,
        expected_sats: row.try_get("expected_sats")?,
        currency: row.try_get("currency")?,
        xrp_address: row.try_get("xrp_address")?,
        status: row.try_get("status")?,
        cek_for_buyer: row.try_get("cek_for_buyer")?,
        expires_at: row.try_get("expires_at")?,
        confirmed_at: row.try_get("confirmed_at")?,
        delivered_at: row.try_get("delivered_at")?,
        // Present only when the query joined listings; a purchase-only row has
        // no such columns and that is not an error.
        listing_title: row
            .try_get::<Option<String>, _>("listing_title")
            .ok()
            .flatten(),
        content_url: row
            .try_get::<Option<String>, _>("content_url")
            .ok()
            .flatten(),
        content_hash: row
            .try_get::<Option<String>, _>("content_hash")
            .ok()
            .flatten(),
    })
}

fn row_to_marketplace_subscription(row: &sqlx::postgres::PgRow) -> Result<MarketplaceSubscription> {
    Ok(MarketplaceSubscription {
        id: row.try_get("id")?,
        buyer_pial: row.try_get("buyer_pial")?,
        seller_pial: row.try_get("seller_pial")?,
        status: row.try_get("status")?,
        period_end: row.try_get("period_end")?,
        created_at: row.try_get("created_at")?,
    })
}

pub async fn shop_open(
    pool: &PgPool,
    seller_pial: PialRef,
    btc_address: &str,
    xrp_address: &str,
) -> Result<Shop> {
    let row = sqlx::query(
        r#"INSERT INTO shops (seller_pial, btc_address, xrp_address)
           VALUES ($1, $2, $3)
           ON CONFLICT (seller_pial) DO UPDATE SET btc_address = EXCLUDED.btc_address, xrp_address = EXCLUDED.xrp_address, is_active = TRUE
           RETURNING seller_pial, btc_address, xrp_address, is_active, created_at"#
    )
    .bind(seller_pial.uuid())
    .bind(btc_address)
    .bind(xrp_address)
    .fetch_one(pool).await?;
    row_to_shop(&row)
}

pub async fn shop_get_by_btc(pool: &PgPool, btc_address: &str) -> Result<Option<Shop>> {
    let row = sqlx::query(
        "SELECT seller_pial, btc_address, xrp_address, is_active, created_at FROM shops WHERE btc_address = $1 AND is_active = TRUE LIMIT 1"
    )
    .bind(btc_address)
    .fetch_optional(pool).await?;
    row.map(|r| row_to_shop(&r)).transpose()
}

pub async fn shop_get_by_xrp(pool: &PgPool, xrp_address: &str) -> Result<Option<Shop>> {
    let row = sqlx::query(
        "SELECT seller_pial, btc_address, xrp_address, is_active, created_at FROM shops WHERE xrp_address = $1 AND xrp_address != '' AND is_active = TRUE LIMIT 1"
    )
    .bind(xrp_address)
    .fetch_optional(pool).await?;
    row.map(|r| row_to_shop(&r)).transpose()
}

pub async fn shop_close(pool: &PgPool, seller_pial: PialRef) -> Result<()> {
    sqlx::query("UPDATE shops SET is_active = FALSE WHERE seller_pial = $1")
        .bind(seller_pial.uuid())
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn shop_get(pool: &PgPool, seller_pial: PialRef) -> Result<Option<Shop>> {
    let row = sqlx::query(
        "SELECT seller_pial, btc_address, xrp_address, is_active, created_at FROM shops WHERE seller_pial = $1"
    )
    .bind(seller_pial.uuid())
    .fetch_optional(pool).await?;
    row.map(|r| row_to_shop(&r)).transpose()
}

pub async fn listing_create(
    pool: &PgPool,
    shop_pial: PialRef,
    req: &ListingCreateReq,
    cek_bytes: Vec<u8>,
) -> Result<Listing> {
    let vis = req.visibility.as_deref().unwrap_or("public");
    let drops = req.price_xrp_drops.unwrap_or(0);
    let currency = req.payment_currency.as_deref().unwrap_or("btc");
    let row = sqlx::query(
        r#"INSERT INTO listings (shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility)
           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
           RETURNING id, shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility, is_active, created_at"#
    )
    .bind(shop_pial.uuid())
    .bind(&req.title)
    .bind(&req.description)
    .bind(req.price_sats)
    .bind(drops)
    .bind(currency)
    .bind(&req.content_url)
    .bind(&req.content_hash)
    .bind(&cek_bytes)
    .bind(&req.phash)
    .bind(vis)
    .fetch_one(pool).await?;
    row_to_listing(&row)
}

pub async fn listing_get(pool: &PgPool, id: Uuid) -> Result<Option<Listing>> {
    let row = sqlx::query(
        "SELECT id, shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility, is_active, created_at FROM listings WHERE id = $1"
    )
    .bind(id)
    .fetch_optional(pool).await?;
    row.map(|r| row_to_listing(&r)).transpose()
}

pub async fn listings_for_shop(
    pool: &PgPool,
    shop_pial: PialRef,
    include_private: bool,
) -> Result<Vec<Listing>> {
    let rows = if include_private {
        sqlx::query(
            "SELECT id, shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility, is_active, created_at FROM listings WHERE shop_pial = $1 AND is_active = TRUE ORDER BY created_at DESC"
        )
        .bind(shop_pial.uuid())
        .fetch_all(pool).await?
    } else {
        sqlx::query(
            "SELECT id, shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility, is_active, created_at FROM listings WHERE shop_pial = $1 AND is_active = TRUE AND visibility = 'public' ORDER BY created_at DESC"
        )
        .bind(shop_pial.uuid())
        .fetch_all(pool).await?
    };
    rows.iter().map(|r| row_to_listing(r)).collect()
}

pub async fn listings_public(pool: &PgPool, limit: i64, offset: i64) -> Result<Vec<Listing>> {
    let rows = sqlx::query(
        "SELECT l.id, l.shop_pial, l.title, l.description, l.price_sats, l.price_xrp_drops, l.payment_currency, l.content_url, l.content_hash, l.cek_encrypted, l.phash, l.visibility, l.is_active, l.created_at FROM listings l JOIN shops s ON l.shop_pial = s.seller_pial WHERE l.is_active = TRUE AND l.visibility = 'public' AND s.is_active = TRUE ORDER BY l.created_at DESC LIMIT $1 OFFSET $2"
    )
    .bind(limit)
    .bind(offset)
    .fetch_all(pool).await?;
    rows.iter().map(|r| row_to_listing(r)).collect()
}

pub async fn purchase_create(
    pool: &PgPool,
    listing_id: Uuid,
    buyer_pial: PialRef,
    btc_address: &str,
    expected_sats: i64,
    currency: &str,
    xrp_address: &str,
) -> Result<Purchase> {
    let expires_at: DateTime<Utc> = Utc::now() + chrono::Duration::minutes(30);
    let row = sqlx::query(
        r#"INSERT INTO purchases (listing_id, buyer_pial, btc_address, expected_sats, currency, xrp_address, expires_at)
           VALUES ($1,$2,$3,$4,$5,$6,$7)
           RETURNING id, listing_id, buyer_pial, btc_address, expected_sats, currency, xrp_address, status, cek_for_buyer, expires_at, confirmed_at, delivered_at"#
    )
    .bind(listing_id)
    .bind(buyer_pial.uuid())
    .bind(btc_address)
    .bind(expected_sats)
    .bind(currency)
    .bind(xrp_address)
    .bind(expires_at)
    .fetch_one(pool).await?;
    row_to_purchase(&row)
}

pub async fn purchase_get(pool: &PgPool, id: Uuid) -> Result<Option<Purchase>> {
    let row = sqlx::query(
        "SELECT id, listing_id, buyer_pial, btc_address, expected_sats, currency, xrp_address, status, cek_for_buyer, expires_at, confirmed_at, delivered_at FROM purchases WHERE id = $1"
    )
    .bind(id)
    .fetch_optional(pool).await?;
    row.map(|r| row_to_purchase(&r)).transpose()
}

pub async fn purchases_pending(pool: &PgPool) -> Result<Vec<Purchase>> {
    let rows = sqlx::query(
        "SELECT id, listing_id, buyer_pial, btc_address, expected_sats, currency, xrp_address, status, cek_for_buyer, expires_at, confirmed_at, delivered_at FROM purchases WHERE status = 'pending' AND currency = 'btc' AND expires_at > NOW()"
    )
    .fetch_all(pool).await?;
    rows.iter().map(|r| row_to_purchase(r)).collect()
}

pub async fn purchases_pending_xrp(pool: &PgPool) -> Result<Vec<Purchase>> {
    let rows = sqlx::query(
        "SELECT id, listing_id, buyer_pial, btc_address, expected_sats, currency, xrp_address, status, cek_for_buyer, expires_at, confirmed_at, delivered_at FROM purchases WHERE status = 'pending' AND currency = 'xrp' AND expires_at > NOW()"
    )
    .fetch_all(pool).await?;
    rows.iter().map(|r| row_to_purchase(r)).collect()
}

pub async fn purchase_mark_confirmed(pool: &PgPool, id: Uuid) -> Result<()> {
    sqlx::query("UPDATE purchases SET status = 'confirmed', confirmed_at = NOW() WHERE id = $1")
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn purchase_mark_delivered(
    pool: &PgPool,
    id: Uuid,
    cek_for_buyer: Vec<u8>,
) -> Result<()> {
    sqlx::query(
        "UPDATE purchases SET status = 'delivered', cek_for_buyer = $2, delivered_at = NOW() WHERE id = $1"
    )
    .bind(id)
    .bind(&cek_for_buyer)
    .execute(pool).await?;
    Ok(())
}

pub async fn purchase_mark_expired(pool: &PgPool, id: Uuid) -> Result<()> {
    sqlx::query("UPDATE purchases SET status = 'expired' WHERE id = $1 AND status = 'pending'")
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn purchase_get_by_buyer_and_listing(
    pool: &PgPool,
    buyer_pial: PialRef,
    listing_id: Uuid,
) -> Result<Option<Purchase>> {
    let row = sqlx::query(
        "SELECT id, listing_id, buyer_pial, btc_address, expected_sats, currency, xrp_address, status, cek_for_buyer, expires_at, confirmed_at, delivered_at FROM purchases WHERE buyer_pial = $1 AND listing_id = $2 AND status = 'delivered' LIMIT 1"
    )
    .bind(buyer_pial.uuid())
    .bind(listing_id)
    .fetch_optional(pool).await?;
    row.map(|r| row_to_purchase(&r)).transpose()
}

pub async fn marketplace_subscription_create(
    pool: &PgPool,
    buyer_pial: PialRef,
    seller_pial: PialRef,
) -> Result<MarketplaceSubscription> {
    let period_end: DateTime<Utc> = Utc::now() + chrono::Duration::days(30);
    let row = sqlx::query(
        r#"INSERT INTO marketplace_subscriptions (buyer_pial, seller_pial, period_end)
           VALUES ($1,$2,$3)
           ON CONFLICT (buyer_pial, seller_pial) DO UPDATE SET status = 'active', period_end = EXCLUDED.period_end
           RETURNING id, buyer_pial, seller_pial, status, period_end, created_at"#
    )
    .bind(buyer_pial.uuid())
    .bind(seller_pial.uuid())
    .bind(period_end)
    .fetch_one(pool).await?;
    row_to_marketplace_subscription(&row)
}

pub async fn marketplace_subscription_cancel(
    pool: &PgPool,
    buyer_pial: PialRef,
    seller_pial: PialRef,
) -> Result<()> {
    sqlx::query(
        "UPDATE marketplace_subscriptions SET status = 'expired' WHERE buyer_pial = $1 AND seller_pial = $2"
    )
    .bind(buyer_pial.uuid())
    .bind(seller_pial.uuid())
    .execute(pool).await?;
    Ok(())
}

pub async fn marketplace_subscription_active(
    pool: &PgPool,
    buyer_pial: PialRef,
    seller_pial: PialRef,
) -> Result<bool> {
    let row = sqlx::query(
        "SELECT 1 as exists FROM marketplace_subscriptions WHERE buyer_pial = $1 AND seller_pial = $2 AND status = 'active' AND period_end > NOW()"
    )
    .bind(buyer_pial.uuid())
    .bind(seller_pial.uuid())
    .fetch_optional(pool).await?;
    Ok(row.is_some())
}

pub async fn purchases_by_buyer(pool: &PgPool, buyer_pial: PialRef) -> Result<Vec<Purchase>> {
    let rows = sqlx::query(
        "SELECT p.id, p.listing_id, p.buyer_pial, p.btc_address, p.expected_sats, p.currency, p.xrp_address, p.status, p.cek_for_buyer, p.expires_at, p.confirmed_at, p.delivered_at, \
                l.title AS listing_title, l.content_url, l.content_hash \
           FROM purchases p \
           JOIN listings l ON l.id = p.listing_id \
          WHERE p.buyer_pial = $1 AND p.status = 'delivered' \
          ORDER BY p.delivered_at DESC"
    )
    .bind(buyer_pial.uuid())
    .fetch_all(pool).await?;
    rows.iter().map(|r| row_to_purchase(r)).collect()
}

pub async fn listing_update_title(pool: &PgPool, id: Uuid, title: &str) -> Result<()> {
    sqlx::query("UPDATE listings SET title = $1 WHERE id = $2")
        .bind(title)
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn listing_update_description(pool: &PgPool, id: Uuid, description: &str) -> Result<()> {
    sqlx::query("UPDATE listings SET description = $1 WHERE id = $2")
        .bind(description)
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn listing_set_active(pool: &PgPool, id: Uuid, active: bool) -> Result<()> {
    sqlx::query("UPDATE listings SET is_active = $1 WHERE id = $2")
        .bind(active)
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

pub async fn seller_stats(pool: &PgPool, seller_pial: PialRef) -> Result<(i64, i64, i64)> {
    // (total_delivered, total_sats, pending_count)
    let delivered: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM purchases p
         JOIN listings l ON l.id = p.listing_id
         WHERE l.shop_pial = $1 AND p.status = 'delivered'",
    )
    .bind(seller_pial.uuid())
    .fetch_one(pool)
    .await?;

    let revenue: i64 = sqlx::query_scalar(
        "SELECT COALESCE(SUM(p.expected_sats), 0) FROM purchases p
         JOIN listings l ON l.id = p.listing_id
         WHERE l.shop_pial = $1 AND p.status = 'delivered'",
    )
    .bind(seller_pial.uuid())
    .fetch_one(pool)
    .await?;

    let pending: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM purchases p
         JOIN listings l ON l.id = p.listing_id
         WHERE l.shop_pial = $1 AND p.status = 'pending'",
    )
    .bind(seller_pial.uuid())
    .fetch_one(pool)
    .await?;

    Ok((delivered, revenue, pending))
}
