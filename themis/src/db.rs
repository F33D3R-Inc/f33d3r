use crate::models::*;
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

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::raw_sql(SCHEMA).execute(pool).await?;
    // One-time migration: old BTC-based subscriptions(buyer_pial) → marketplace_subscriptions.
    // If marketplace_subscriptions already exists (from a prior partial migration), drop the
    // old subscriptions instead — it will be recreated as the commerce table by COMMERCE_SCHEMA.
    sqlx::query(r#"
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
"#).execute(pool).await?;
    sqlx::raw_sql(COMMERCE_SCHEMA).execute(pool).await?;
    Ok(())
}

fn row_to_shop(row: &sqlx::postgres::PgRow) -> Result<Shop> {
    Ok(Shop {
        seller_pial: row.try_get("seller_pial")?,
        btc_address: row.try_get("btc_address")?,
        xrp_address: row.try_get("xrp_address")?,
        is_active:   row.try_get("is_active")?,
        created_at:  row.try_get("created_at")?,
    })
}

fn row_to_listing(row: &sqlx::postgres::PgRow) -> Result<Listing> {
    Ok(Listing {
        id:               row.try_get("id")?,
        shop_pial:        row.try_get("shop_pial")?,
        title:            row.try_get("title")?,
        description:      row.try_get("description")?,
        price_sats:       row.try_get("price_sats")?,
        price_xrp_drops:  row.try_get("price_xrp_drops")?,
        payment_currency: row.try_get("payment_currency")?,
        content_url:      row.try_get("content_url")?,
        content_hash:     row.try_get("content_hash")?,
        cek_encrypted:    row.try_get("cek_encrypted")?,
        phash:            row.try_get("phash")?,
        visibility:       row.try_get("visibility")?,
        is_active:        row.try_get("is_active")?,
        created_at:       row.try_get("created_at")?,
    })
}

fn row_to_purchase(row: &sqlx::postgres::PgRow) -> Result<Purchase> {
    Ok(Purchase {
        id:            row.try_get("id")?,
        listing_id:    row.try_get("listing_id")?,
        buyer_pial:    row.try_get("buyer_pial")?,
        btc_address:   row.try_get("btc_address")?,
        expected_sats: row.try_get("expected_sats")?,
        currency:      row.try_get("currency")?,
        xrp_address:   row.try_get("xrp_address")?,
        status:        row.try_get("status")?,
        cek_for_buyer: row.try_get("cek_for_buyer")?,
        expires_at:    row.try_get("expires_at")?,
        confirmed_at:  row.try_get("confirmed_at")?,
        delivered_at:  row.try_get("delivered_at")?,
    })
}

fn row_to_marketplace_subscription(row: &sqlx::postgres::PgRow) -> Result<MarketplaceSubscription> {
    Ok(MarketplaceSubscription {
        id:          row.try_get("id")?,
        buyer_pial:  row.try_get("buyer_pial")?,
        seller_pial: row.try_get("seller_pial")?,
        status:      row.try_get("status")?,
        period_end:  row.try_get("period_end")?,
        created_at:  row.try_get("created_at")?,
    })
}

pub async fn shop_open(pool: &PgPool, seller_pial: Uuid, btc_address: &str, xrp_address: &str) -> Result<Shop> {
    let row = sqlx::query(
        r#"INSERT INTO shops (seller_pial, btc_address, xrp_address)
           VALUES ($1, $2, $3)
           ON CONFLICT (seller_pial) DO UPDATE SET btc_address = EXCLUDED.btc_address, xrp_address = EXCLUDED.xrp_address, is_active = TRUE
           RETURNING seller_pial, btc_address, xrp_address, is_active, created_at"#
    )
    .bind(seller_pial)
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

pub async fn shop_close(pool: &PgPool, seller_pial: Uuid) -> Result<()> {
    sqlx::query("UPDATE shops SET is_active = FALSE WHERE seller_pial = $1")
        .bind(seller_pial)
        .execute(pool).await?;
    Ok(())
}

pub async fn shop_get(pool: &PgPool, seller_pial: Uuid) -> Result<Option<Shop>> {
    let row = sqlx::query(
        "SELECT seller_pial, btc_address, xrp_address, is_active, created_at FROM shops WHERE seller_pial = $1"
    )
    .bind(seller_pial)
    .fetch_optional(pool).await?;
    row.map(|r| row_to_shop(&r)).transpose()
}

pub async fn listing_create(
    pool: &PgPool,
    shop_pial: Uuid,
    req: &ListingCreateReq,
    cek_bytes: Vec<u8>,
) -> Result<Listing> {
    let vis     = req.visibility.as_deref().unwrap_or("public");
    let drops   = req.price_xrp_drops.unwrap_or(0);
    let currency = req.payment_currency.as_deref().unwrap_or("btc");
    let row = sqlx::query(
        r#"INSERT INTO listings (shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility)
           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
           RETURNING id, shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility, is_active, created_at"#
    )
    .bind(shop_pial)
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

pub async fn listings_for_shop(pool: &PgPool, shop_pial: Uuid, include_private: bool) -> Result<Vec<Listing>> {
    let rows = if include_private {
        sqlx::query(
            "SELECT id, shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility, is_active, created_at FROM listings WHERE shop_pial = $1 AND is_active = TRUE ORDER BY created_at DESC"
        )
        .bind(shop_pial)
        .fetch_all(pool).await?
    } else {
        sqlx::query(
            "SELECT id, shop_pial, title, description, price_sats, price_xrp_drops, payment_currency, content_url, content_hash, cek_encrypted, phash, visibility, is_active, created_at FROM listings WHERE shop_pial = $1 AND is_active = TRUE AND visibility = 'public' ORDER BY created_at DESC"
        )
        .bind(shop_pial)
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
    buyer_pial: Uuid,
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
    .bind(buyer_pial)
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
        .execute(pool).await?;
    Ok(())
}

pub async fn purchase_mark_delivered(pool: &PgPool, id: Uuid, cek_for_buyer: Vec<u8>) -> Result<()> {
    sqlx::query(
        "UPDATE purchases SET status = 'delivered', cek_for_buyer = $2, delivered_at = NOW() WHERE id = $1"
    )
    .bind(id)
    .bind(&cek_for_buyer)
    .execute(pool).await?;
    Ok(())
}

pub async fn purchase_mark_expired(pool: &PgPool, id: Uuid) -> Result<()> {
    sqlx::query(
        "UPDATE purchases SET status = 'expired' WHERE id = $1 AND status = 'pending'"
    )
    .bind(id)
    .execute(pool).await?;
    Ok(())
}

pub async fn purchase_get_by_buyer_and_listing(
    pool: &PgPool,
    buyer_pial: Uuid,
    listing_id: Uuid,
) -> Result<Option<Purchase>> {
    let row = sqlx::query(
        "SELECT id, listing_id, buyer_pial, btc_address, expected_sats, currency, xrp_address, status, cek_for_buyer, expires_at, confirmed_at, delivered_at FROM purchases WHERE buyer_pial = $1 AND listing_id = $2 AND status = 'delivered' LIMIT 1"
    )
    .bind(buyer_pial)
    .bind(listing_id)
    .fetch_optional(pool).await?;
    row.map(|r| row_to_purchase(&r)).transpose()
}

pub async fn marketplace_subscription_create(pool: &PgPool, buyer_pial: Uuid, seller_pial: Uuid) -> Result<MarketplaceSubscription> {
    let period_end: DateTime<Utc> = Utc::now() + chrono::Duration::days(30);
    let row = sqlx::query(
        r#"INSERT INTO marketplace_subscriptions (buyer_pial, seller_pial, period_end)
           VALUES ($1,$2,$3)
           ON CONFLICT (buyer_pial, seller_pial) DO UPDATE SET status = 'active', period_end = EXCLUDED.period_end
           RETURNING id, buyer_pial, seller_pial, status, period_end, created_at"#
    )
    .bind(buyer_pial)
    .bind(seller_pial)
    .bind(period_end)
    .fetch_one(pool).await?;
    row_to_marketplace_subscription(&row)
}

pub async fn marketplace_subscription_cancel(pool: &PgPool, buyer_pial: Uuid, seller_pial: Uuid) -> Result<()> {
    sqlx::query(
        "UPDATE marketplace_subscriptions SET status = 'expired' WHERE buyer_pial = $1 AND seller_pial = $2"
    )
    .bind(buyer_pial)
    .bind(seller_pial)
    .execute(pool).await?;
    Ok(())
}

pub async fn marketplace_subscription_active(pool: &PgPool, buyer_pial: Uuid, seller_pial: Uuid) -> Result<bool> {
    let row = sqlx::query(
        "SELECT 1 as exists FROM marketplace_subscriptions WHERE buyer_pial = $1 AND seller_pial = $2 AND status = 'active' AND period_end > NOW()"
    )
    .bind(buyer_pial)
    .bind(seller_pial)
    .fetch_optional(pool).await?;
    Ok(row.is_some())
}

pub async fn purchases_by_buyer(pool: &PgPool, buyer_pial: Uuid) -> Result<Vec<Purchase>> {
    let rows = sqlx::query(
        "SELECT id, listing_id, buyer_pial, btc_address, expected_sats, currency, xrp_address, status, cek_for_buyer, expires_at, confirmed_at, delivered_at FROM purchases WHERE buyer_pial = $1 AND status = 'delivered' ORDER BY delivered_at DESC"
    )
    .bind(buyer_pial)
    .fetch_all(pool).await?;
    rows.iter().map(|r| row_to_purchase(r)).collect()
}

pub async fn listing_update_title(pool: &PgPool, id: Uuid, title: &str) -> Result<()> {
    sqlx::query("UPDATE listings SET title = $1 WHERE id = $2")
        .bind(title)
        .bind(id)
        .execute(pool).await?;
    Ok(())
}

pub async fn listing_update_description(pool: &PgPool, id: Uuid, description: &str) -> Result<()> {
    sqlx::query("UPDATE listings SET description = $1 WHERE id = $2")
        .bind(description)
        .bind(id)
        .execute(pool).await?;
    Ok(())
}

pub async fn listing_set_active(pool: &PgPool, id: Uuid, active: bool) -> Result<()> {
    sqlx::query("UPDATE listings SET is_active = $1 WHERE id = $2")
        .bind(active)
        .bind(id)
        .execute(pool).await?;
    Ok(())
}

pub async fn seller_stats(pool: &PgPool, seller_pial: Uuid) -> Result<(i64, i64, i64)> {
    // (total_delivered, total_sats, pending_count)
    let delivered: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM purchases p
         JOIN listings l ON l.id = p.listing_id
         WHERE l.shop_pial = $1 AND p.status = 'delivered'"
    ).bind(seller_pial).fetch_one(pool).await?;

    let revenue: i64 = sqlx::query_scalar(
        "SELECT COALESCE(SUM(p.expected_sats), 0) FROM purchases p
         JOIN listings l ON l.id = p.listing_id
         WHERE l.shop_pial = $1 AND p.status = 'delivered'"
    ).bind(seller_pial).fetch_one(pool).await?;

    let pending: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM purchases p
         JOIN listings l ON l.id = p.listing_id
         WHERE l.shop_pial = $1 AND p.status = 'pending'"
    ).bind(seller_pial).fetch_one(pool).await?;

    Ok((delivered, revenue, pending))
}
