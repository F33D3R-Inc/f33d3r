use sqlx::PgPool;
use anyhow::Result;

pub async fn migrate(pool: &PgPool) -> Result<()> {
    sqlx::query(r#"
CREATE TABLE IF NOT EXISTS creator_eligibility (
    pial_id             TEXT PRIMARY KEY,
    kyc_tier            INTEGER NOT NULL DEFAULT 0,
    monetization_enabled BOOLEAN NOT NULL DEFAULT false,
    enabled_at          TIMESTAMPTZ,
    disabled_at         TIMESTAMPTZ,
    disabled_reason     TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
)"#).execute(pool).await?;

    sqlx::query(r#"
CREATE TABLE IF NOT EXISTS subscription_plans (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_pial_id TEXT NOT NULL,
    name            TEXT NOT NULL,
    price_aet       BIGINT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    is_active       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
)"#).execute(pool).await?;

    sqlx::query(
        "CREATE INDEX IF NOT EXISTS idx_plans_creator ON subscription_plans(creator_pial_id) WHERE is_active"
    ).execute(pool).await?;

    sqlx::query(r#"
CREATE TABLE IF NOT EXISTS subscriptions (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscriber_pial_id   TEXT NOT NULL,
    creator_pial_id      TEXT NOT NULL,
    plan_id              UUID REFERENCES subscription_plans(id),
    status               TEXT NOT NULL DEFAULT 'active',
    current_period_start TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    current_period_end   TIMESTAMPTZ NOT NULL,
    cancelled_at         TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
)"#).execute(pool).await?;

    sqlx::query(
        "CREATE INDEX IF NOT EXISTS idx_subs_subscriber ON subscriptions(subscriber_pial_id)"
    ).execute(pool).await?;
    sqlx::query(
        "CREATE INDEX IF NOT EXISTS idx_subs_creator ON subscriptions(creator_pial_id)"
    ).execute(pool).await?;

    sqlx::query(r#"
CREATE TABLE IF NOT EXISTS ppv_items (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    creator_pial_id TEXT NOT NULL,
    content_id      TEXT NOT NULL,
    price_aet       BIGINT NOT NULL,
    title           TEXT NOT NULL DEFAULT '',
    is_active       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
)"#).execute(pool).await?;

    sqlx::query(r#"
CREATE TABLE IF NOT EXISTS ppv_purchases (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    buyer_pial_id   TEXT NOT NULL,
    ppv_item_id     UUID NOT NULL REFERENCES ppv_items(id),
    amount_aet      BIGINT NOT NULL,
    ain_soph_tx_id  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(buyer_pial_id, ppv_item_id)
)"#).execute(pool).await?;

    sqlx::query(r#"
CREATE TABLE IF NOT EXISTS tips (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sender_pial_id      TEXT NOT NULL,
    recipient_pial_id   TEXT NOT NULL,
    amount_aet          BIGINT NOT NULL,
    message             TEXT NOT NULL DEFAULT '',
    content_id          TEXT,
    ain_soph_tx_id      TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
)"#).execute(pool).await?;

    sqlx::query(r#"
CREATE TABLE IF NOT EXISTS transactions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tx_type         TEXT NOT NULL,
    reference_id    UUID NOT NULL,
    payer_pial_id   TEXT NOT NULL,
    creator_pial_id TEXT NOT NULL,
    gross_aet       BIGINT NOT NULL,
    ain_soph_tx_id  TEXT,
    status          TEXT NOT NULL DEFAULT 'completed',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
)"#).execute(pool).await?;

    sqlx::query(
        "CREATE INDEX IF NOT EXISTS idx_tx_creator ON transactions(creator_pial_id)"
    ).execute(pool).await?;
    sqlx::query(
        "CREATE INDEX IF NOT EXISTS idx_tx_payer ON transactions(payer_pial_id)"
    ).execute(pool).await?;

    Ok(())
}
